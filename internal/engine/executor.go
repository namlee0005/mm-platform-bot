package engine

import (
	"context"
	"fmt"
	"log"
	"time"

	"mm-platform-engine/internal/core"
	"mm-platform-engine/internal/exchange"
	"mm-platform-engine/internal/modules"
)

// Executor handles async order execution with WS-first, REST-fallback strategy.
// It wraps the existing ExecutionEngine and adds WS order support.
// Uses a semaphore channel to serialize execution — only 1 execution at a time.
type Executor struct {
	exch       exchange.Exchange
	diffEngine *modules.OrderDiffEngine
	execEngine *modules.ExecutionEngine
	symbol     string

	// Serialization — buffered channel of size 1 acts as a semaphore.
	// If previous execution is still running, new one is skipped (not queued).
	execGate chan struct{}

	// Event callback
	onOrderEvent  core.OrderEventCallback
	onOrderPlaced func(order *exchange.Order, levelIndex int) // called after successful place
	onOrderGone   func(orderID string)                        // called when cancel returns "already gone" — removes ghost from tracker
}

// NewExecutor creates an Executor.
func NewExecutor(
	exch exchange.Exchange,
	symbol string,
	diffEngine *modules.OrderDiffEngine,
	execEngine *modules.ExecutionEngine,
) *Executor {
	return &Executor{
		exch:       exch,
		symbol:     symbol,
		diffEngine: diffEngine,
		execEngine: execEngine,
		execGate:   make(chan struct{}, 1),
	}
}

// SetOrderEventCallback sets the callback for order events.
func (ex *Executor) SetOrderEventCallback(cb core.OrderEventCallback) {
	ex.onOrderEvent = cb
	ex.execEngine.SetOrderEventCallback(cb)
}

// SetOnOrderPlaced sets callback for when an order is successfully placed.
// Used to immediately add to OrderTracker so backstop doesn't create duplicates.
func (ex *Executor) SetOnOrderPlaced(cb func(order *exchange.Order, levelIndex int)) {
	ex.onOrderPlaced = cb
}

// SetOnOrderGone sets callback for when cancel returns "already gone".
// This removes ghost orders from the tracker that will never get a WS CANCELED event.
func (ex *Executor) SetOnOrderGone(cb func(orderID string)) {
	ex.onOrderGone = cb
}

// Execute processes a TickOutput asynchronously.
// Uses OrderDiffEngine for REPLACE to compute minimal changes.
// Serialized — if previous execution is still running, this one is skipped.
func (ex *Executor) Execute(ctx context.Context, output *core.TickOutput, liveOrders []core.LiveOrder) {
	log.Printf("[EXECUTOR] DEBUG: Execute action=%s orders=%d live=%d", output.Action, len(output.DesiredOrders), len(liveOrders))

	switch output.Action {
	case core.TickActionKeep:
		log.Printf("[EXECUTOR] DEBUG: KEEP — no changes")
		return

	case core.TickActionCancelAll:
		log.Printf("[EXECUTOR] DEBUG: CANCEL_ALL reason=%s", output.Reason)
		ex.runAsync(ctx, func() error {
			return ex.cancelAll(ctx, output.Reason)
		})

	case core.TickActionReplace:
		diffs := ex.diffEngine.Diff(output.DesiredOrders, liveOrders)
		if len(diffs) == 0 {
			log.Printf("[EXECUTOR] DEBUG: REPLACE — diff computed 0 changes, skipping")
			return
		}
		places, amends, cancels := modules.CountByAction(diffs)
		log.Printf("[EXECUTOR] REPLACE via diff: %d places, %d amends, %d cancels",
			places, amends, cancels)

		ex.runAsync(ctx, func() error {
			return ex.executeDiffs(ctx, diffs)
		})

	case core.TickActionAmend:
		diffs := ex.buildAmendDiffs(output, liveOrders)
		if len(diffs) == 0 {
			log.Printf("[EXECUTOR] DEBUG: AMEND — 0 diffs, skipping")
			return
		}
		log.Printf("[EXECUTOR] DEBUG: AMEND — %d diffs (cancel=%d add=%d)",
			len(diffs), len(output.OrdersToCancel), len(output.OrdersToAdd))
		ex.runAsync(ctx, func() error {
			return ex.executeDiffs(ctx, diffs)
		})
	}
}

// runAsync executes fn in a goroutine, serialized by execGate.
// Waits up to 2s for previous execution to finish before skipping.
func (ex *Executor) runAsync(ctx context.Context, fn func() error) {
	select {
	case ex.execGate <- struct{}{}:
		go func() {
			defer func() { <-ex.execGate }()
			if err := fn(); err != nil {
				log.Printf("[EXECUTOR] Execution failed: %v", err)
			}
		}()
	default:
		// Wait briefly for previous execution to finish
		select {
		case ex.execGate <- struct{}{}:
			go func() {
				defer func() { <-ex.execGate }()
				if err := fn(); err != nil {
					log.Printf("[EXECUTOR] Execution failed: %v", err)
				}
			}()
		case <-time.After(2 * time.Second):
			log.Printf("[EXECUTOR] Skipped — previous execution still running after 2s")
		case <-ctx.Done():
		}
	}
}

// CancelAll cancels all orders for the symbol.
func (ex *Executor) CancelAll(ctx context.Context, reason string) error {
	return ex.cancelAll(ctx, reason)
}

func (ex *Executor) cancelAll(ctx context.Context, reason string) error {
	log.Printf("[EXECUTOR] CancelAll: %s", reason)

	// Always use REST for CancelAll — WS CancelAllOrders is not supported by
	// many exchanges (e.g. bybit) and the ccxt lib panics in a separate goroutine,
	// which cannot be recovered and causes concurrent map access crashes.
	return ex.exch.CancelAllOrders(ctx, ex.symbol)
}

func (ex *Executor) executeDiffs(ctx context.Context, diffs []core.OrderDiff) error {
	if len(diffs) == 0 {
		return nil
	}

	// Check if exchange supports WS orders
	wsExch, hasWs := ex.exch.(exchange.WSOrderExchange)
	if !hasWs || !wsExch.HasWsOrderSupport() {
		log.Printf("[EXECUTOR] DEBUG: No WS order support — using REST execution engine")
		return ex.execEngine.Execute(ctx, diffs)
	}
	log.Printf("[EXECUTOR] DEBUG: Using WS orders (hasEdit=%v)", wsExch.HasWsEditSupport())

	// WS-first execution: cancels → amends → places
	cancels := make([]core.OrderDiff, 0, len(diffs))
	amends := make([]core.OrderDiff, 0, len(diffs))
	places := make([]core.OrderDiff, 0, len(diffs))
	for _, d := range diffs {
		switch d.Action {
		case core.DiffCancel:
			cancels = append(cancels, d)
		case core.DiffAmend:
			amends = append(amends, d)
		case core.DiffPlace:
			places = append(places, d)
		}
	}

	// 1. Cancels via WS
	for _, d := range cancels {
		if err := wsExch.CancelOrderWs(ctx, ex.symbol, d.Live.OrderID); err != nil {
			log.Printf("[EXECUTOR] WS cancel failed for %s: %v (trying REST)", d.Live.OrderID, err)
			if restErr := ex.exch.CancelOrder(ctx, ex.symbol, d.Live.OrderID); restErr != nil {
				log.Printf("[EXECUTOR] REST cancel also failed for %s: %v", d.Live.OrderID, restErr)
			}
		}
		// Remove from tracker immediately. For "already gone" orders, the WS will never
		// send a CANCELED event, so the tracker would keep ghost orders forever.
		// For normal cancels, the WS CANCELED event will arrive later but Remove is idempotent.
		if ex.onOrderGone != nil {
			ex.onOrderGone(d.Live.OrderID)
		}
	}

	// 2. Amends via WS EditOrder (if supported) or cancel+place
	for _, d := range amends {
		if wsExch.HasWsEditSupport() {
			_, err := wsExch.EditOrderWs(ctx, d.Live.OrderID, ex.symbol, d.Desired.Price, d.Desired.Qty)
			if err == nil {
				log.Printf("[EXECUTOR] WS edit %s %s L%d: %.8f→%.8f",
					d.Live.Side, ex.symbol, d.Live.LevelIndex, d.Live.Price, d.Desired.Price)
				continue
			}
			log.Printf("[EXECUTOR] WS edit failed for %s: %v (cancel+place)", d.Live.OrderID, err)
		}
		// Fallback: cancel + place
		_ = wsExch.CancelOrderWs(ctx, ex.symbol, d.Live.OrderID)
		if ex.onOrderGone != nil {
			ex.onOrderGone(d.Live.OrderID)
		}
		if _, err := ex.placeOrderWs(ctx, wsExch, d.Desired); err != nil {
			log.Printf("[EXECUTOR] WS place after amend failed: %v", err)
		}
	}

	// 3. Places via WS
	for _, d := range places {
		if _, err := ex.placeOrderWs(ctx, wsExch, d.Desired); err != nil {
			log.Printf("[EXECUTOR] WS place failed: %v (trying REST)", err)
			// Fallback to REST
			ex.placeOrderREST(ctx, d.Desired)
		}
	}

	return nil
}

func (ex *Executor) placeOrderWs(ctx context.Context, wsExch exchange.WSOrderExchange, desired *core.DesiredOrder) (*exchange.Order, error) {
	req := &exchange.OrderRequest{
		Symbol:        ex.symbol,
		Side:          desired.Side,
		Type:          "LIMIT",
		Price:         desired.Price,
		Quantity:      desired.Qty,
		ClientOrderID: desired.Tag,
	}
	order, err := wsExch.PlaceOrderWs(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("WS place order failed: %w", err)
	}
	log.Printf("[EXECUTOR] WS place %s %s L%d %.8f @ %.8f (id=%s)",
		ex.symbol, order.Side, desired.LevelIndex, order.Quantity, order.Price, order.OrderID)
	// Immediately register in tracker so backstop doesn't create duplicates
	if ex.onOrderPlaced != nil {
		ex.onOrderPlaced(order, desired.LevelIndex)
	}
	return order, nil
}

func (ex *Executor) placeOrderREST(ctx context.Context, desired *core.DesiredOrder) {
	req := &exchange.OrderRequest{
		Symbol:        ex.symbol,
		Side:          desired.Side,
		Type:          "LIMIT",
		Price:         desired.Price,
		Quantity:      desired.Qty,
		ClientOrderID: desired.Tag,
	}
	order, err := ex.exch.PlaceOrder(ctx, req)
	if err != nil {
		log.Printf("[EXECUTOR] REST place also failed: %v", err)
		return
	}
	log.Printf("[EXECUTOR] REST place %s %s L%d %.8f @ %.8f (id=%s)",
		ex.symbol, order.Side, desired.LevelIndex, order.Quantity, order.Price, order.OrderID)
	if ex.onOrderPlaced != nil {
		ex.onOrderPlaced(order, desired.LevelIndex)
	}
}

// buildAmendDiffs converts explicit OrdersToCancel/OrdersToAdd into OrderDiffs.
func (ex *Executor) buildAmendDiffs(output *core.TickOutput, liveOrders []core.LiveOrder) []core.OrderDiff {
	var diffs []core.OrderDiff

	// Build live order lookup
	liveMap := make(map[string]*core.LiveOrder, len(liveOrders))
	for i := range liveOrders {
		liveMap[liveOrders[i].OrderID] = &liveOrders[i]
	}

	// Cancels
	for _, id := range output.OrdersToCancel {
		if live, ok := liveMap[id]; ok {
			diffs = append(diffs, core.OrderDiff{
				Action: core.DiffCancel,
				Live:   live,
				Reason: output.Reason,
			})
		}
	}

	// Places
	for i := range output.OrdersToAdd {
		diffs = append(diffs, core.OrderDiff{
			Action:  core.DiffPlace,
			Desired: &output.OrdersToAdd[i],
			Reason:  output.Reason,
		})
	}

	return diffs
}
