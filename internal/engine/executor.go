package engine

import (
	"context"
	"fmt"
	"log"
	"sync"

	"mm-platform-engine/internal/core"
	"mm-platform-engine/internal/exchange"
	"mm-platform-engine/internal/modules"
)

// pendingExecution holds the latest desired state waiting to be applied.
type pendingExecution struct {
	output     *core.TickOutput
	liveOrders []core.LiveOrder
}

// Executor applies desired order state to the exchange using state-based
// conflation and cancel-first execution priority.
//
// Conflation model:
//   - Execute() stores the LATEST desired state under mu and signals the worker.
//   - A single background goroutine (controlled by execGate) drains the latest
//     state in a loop. Intermediate states are dropped — only the newest matters.
//   - A deferred re-check after the worker loop exits closes the race where a new
//     state arrives between the loop's nil-check and the gate release.
//
// Cancel-first model:
//   - Phase 1: All DiffCancel ops run concurrently via goroutines; a WaitGroup
//     blocks until every cancel has returned (success or fallback).
//   - Phase 2: DiffAmend and DiffPlace ops run concurrently after Phase 1 drains.
//     This prevents placing into stale resting liquidity.
type Executor struct {
	exch       exchange.Exchange
	diffEngine *modules.OrderDiffEngine
	execEngine *modules.ExecutionEngine
	symbol     string

	// State-based conflation.
	mu      sync.Mutex
	pending *pendingExecution // nil = nothing queued

	// execGate is a buffered-1 channel used as a "worker running" flag.
	// Non-blocking send starts the worker; worker releases on exit.
	execGate chan struct{}

	// Callbacks
	onOrderEvent  core.OrderEventCallback
	onOrderPlaced func(order *exchange.Order, levelIndex int)
	onOrderGone   func(orderID string)
}

// NewExecutor creates an Executor. No goroutines are started until Execute is called.
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

// SetOnOrderPlaced registers a callback invoked after a successful order placement.
// Used to immediately register the order in OrderTracker, preventing duplicates.
func (ex *Executor) SetOnOrderPlaced(cb func(order *exchange.Order, levelIndex int)) {
	ex.onOrderPlaced = cb
}

// SetOnOrderGone registers a callback invoked when a cancel returns "already gone".
// This evicts ghost orders from the tracker that will never receive a WS CANCELED event.
func (ex *Executor) SetOnOrderGone(cb func(orderID string)) {
	ex.onOrderGone = cb
}

// Execute records the latest desired state and triggers the background worker.
// Intermediate states between worker iterations are automatically conflated (dropped).
func (ex *Executor) Execute(ctx context.Context, output *core.TickOutput, liveOrders []core.LiveOrder) {
	log.Printf("[EXECUTOR] Execute action=%s orders=%d live=%d",
		output.Action, len(output.DesiredOrders), len(liveOrders))

	switch output.Action {
	case core.TickActionKeep:
		return

	case core.TickActionCancelAll:
		// CancelAll supersedes all queued work — clear pending then execute immediately.
		ex.mu.Lock()
		ex.pending = nil
		ex.mu.Unlock()
		go func() {
			if err := ex.cancelAll(ctx, output.Reason); err != nil {
				log.Printf("[EXECUTOR] CancelAll error: %v", err)
			}
		}()
		log.Printf("[EXECUTOR] CANCEL_ALL dispatched (reason=%s)", output.Reason)

	case core.TickActionReplace, core.TickActionAmend:
		ex.mu.Lock()
		ex.pending = &pendingExecution{output: output, liveOrders: liveOrders}
		ex.mu.Unlock()
		ex.triggerWorker(ctx)
	}
}

// triggerWorker starts the worker goroutine if it is not already running.
// The worker drains the latest pending state in a loop until idle, then exits.
// The deferred re-check handles the race where state arrives between the
// inner loop's nil-check and the gate release.
func (ex *Executor) triggerWorker(ctx context.Context) {
	select {
	case ex.execGate <- struct{}{}:
		go func() {
			defer func() {
				<-ex.execGate
				// Re-trigger if a new state arrived while the loop was draining.
				ex.mu.Lock()
				hasMore := ex.pending != nil
				ex.mu.Unlock()
				if hasMore {
					ex.triggerWorker(ctx)
				}
			}()

			for {
				ex.mu.Lock()
				p := ex.pending
				ex.pending = nil
				ex.mu.Unlock()

				if p == nil {
					return
				}

				if err := ex.processState(ctx, p); err != nil {
					log.Printf("[EXECUTOR] processState error: %v", err)
				}
			}
		}()

	default:
		// Worker is already running. It will pick up the latest pending state
		// on the next loop iteration — this tick is conflated.
		log.Printf("[EXECUTOR] Conflated — worker running, latest state queued")
	}
}

// processState computes diffs for the pending state and executes them.
func (ex *Executor) processState(ctx context.Context, p *pendingExecution) error {
	output := p.output
	liveOrders := p.liveOrders

	var diffs []core.OrderDiff
	switch output.Action {
	case core.TickActionReplace:
		diffs = ex.diffEngine.Diff(output.DesiredOrders, liveOrders)
		if len(diffs) == 0 {
			log.Printf("[EXECUTOR] REPLACE — 0 diffs, skipping")
			return nil
		}
		places, amends, cancels := modules.CountByAction(diffs)
		log.Printf("[EXECUTOR] REPLACE: %d places, %d amends, %d cancels", places, amends, cancels)

	case core.TickActionAmend:
		diffs = ex.buildAmendDiffs(output, liveOrders)
		if len(diffs) == 0 {
			log.Printf("[EXECUTOR] AMEND — 0 diffs, skipping")
			return nil
		}
		log.Printf("[EXECUTOR] AMEND: %d diffs (cancel=%d add=%d)",
			len(diffs), len(output.OrdersToCancel), len(output.OrdersToAdd))
	}

	return ex.executeDiffs(ctx, diffs)
}

// CancelAll cancels all orders for the symbol synchronously.
func (ex *Executor) CancelAll(ctx context.Context, reason string) error {
	return ex.cancelAll(ctx, reason)
}

func (ex *Executor) cancelAll(ctx context.Context, reason string) error {
	log.Printf("[EXECUTOR] CancelAll: %s", reason)
	// Always REST for CancelAll — WS CancelAll is unreliable on Bybit and causes
	// panics in some CCXT implementations due to concurrent map access.
	return ex.exch.CancelAllOrders(ctx, ex.symbol)
}

// executeDiffs applies a diff set using WS-first execution with cancel-first ordering.
//
// Phase 1 — Cancels (concurrent, barrier):
//   All DiffCancel operations fire concurrently via goroutines. A WaitGroup blocks
//   until every cancel has returned. This ensures stale orders are gone before new
//   ones are placed, preventing self-crossing and duplicate resting liquidity.
//
// Phase 2 — Amends + Places (concurrent):
//   DiffAmend and DiffPlace operations run concurrently after Phase 1 completes.
func (ex *Executor) executeDiffs(ctx context.Context, diffs []core.OrderDiff) error {
	if len(diffs) == 0 {
		return nil
	}

	wsExch, hasWs := ex.exch.(exchange.WSOrderExchange)
	if !hasWs || !wsExch.HasWsOrderSupport() {
		log.Printf("[EXECUTOR] No WS support — REST execution engine")
		return ex.execEngine.Execute(ctx, diffs)
	}
	log.Printf("[EXECUTOR] WS execution (hasEdit=%v)", wsExch.HasWsEditSupport())

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

	// ── Phase 1: Concurrent cancels — hard barrier before any places ─────────
	if len(cancels) > 0 {
		var wg sync.WaitGroup
		for _, d := range cancels {
			wg.Add(1)
			go func(d core.OrderDiff) {
				defer wg.Done()
				if err := wsExch.CancelOrderWs(ctx, ex.symbol, d.Live.OrderID); err != nil {
					log.Printf("[EXECUTOR] WS cancel %s failed: %v (REST fallback)", d.Live.OrderID, err)
					if restErr := ex.exch.CancelOrder(ctx, ex.symbol, d.Live.OrderID); restErr != nil {
						log.Printf("[EXECUTOR] REST cancel %s also failed: %v", d.Live.OrderID, restErr)
					}
				}
				// Evict immediately — WS CANCELED event may never arrive for already-gone orders.
				if ex.onOrderGone != nil {
					ex.onOrderGone(d.Live.OrderID)
				}
			}(d)
		}
		wg.Wait()
		log.Printf("[EXECUTOR] Phase1 complete: %d cancels done", len(cancels))
	}

	// ── Phase 2: Concurrent amends + places ───────────────────────────────────
	var wg sync.WaitGroup

	for _, d := range amends {
		wg.Add(1)
		go func(d core.OrderDiff) {
			defer wg.Done()
			if wsExch.HasWsEditSupport() {
				_, err := wsExch.EditOrderWs(ctx, d.Live.OrderID, ex.symbol, d.Desired.Price, d.Desired.Qty)
				if err == nil {
					log.Printf("[EXECUTOR] WS amend %s L%d: %.8f→%.8f",
						d.Live.Side, d.Live.LevelIndex, d.Live.Price, d.Desired.Price)
					return
				}
				log.Printf("[EXECUTOR] WS amend %s failed: %v (cancel+place)", d.Live.OrderID, err)
			}
			// Fallback: cancel then place
			_ = wsExch.CancelOrderWs(ctx, ex.symbol, d.Live.OrderID)
			if ex.onOrderGone != nil {
				ex.onOrderGone(d.Live.OrderID)
			}
			if _, err := ex.placeOrderWs(ctx, wsExch, d.Desired); err != nil {
				log.Printf("[EXECUTOR] WS place (amend fallback) failed: %v", err)
			}
		}(d)
	}

	for _, d := range places {
		wg.Add(1)
		go func(d core.OrderDiff) {
			defer wg.Done()
			if _, err := ex.placeOrderWs(ctx, wsExch, d.Desired); err != nil {
				log.Printf("[EXECUTOR] WS place failed: %v (REST fallback)", err)
				ex.placeOrderREST(ctx, d.Desired)
			}
		}(d)
	}

	wg.Wait()
	return nil
}

func (ex *Executor) placeOrderWs(ctx context.Context, wsExch exchange.WSOrderExchange, desired *core.DesiredOrder) (*exchange.Order, error) {
	req := &exchange.OrderRequest{
		Symbol:        ex.symbol,
		Side:          desired.Side,
		Type:          "LIMIT",
		TimeInForce:   "GTX", // PostOnly — reject if would cross book
		Price:         desired.Price,
		Quantity:      desired.Qty,
		ClientOrderID: desired.Tag,
	}
	order, err := wsExch.PlaceOrderWs(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("WS place failed: %w", err)
	}
	log.Printf("[EXECUTOR] WS place %s %s L%d %.8f @ %.8f (id=%s)",
		ex.symbol, order.Side, desired.LevelIndex, order.Quantity, order.Price, order.OrderID)
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
		TimeInForce:   "GTX",
		Price:         desired.Price,
		Quantity:      desired.Qty,
		ClientOrderID: desired.Tag,
	}
	order, err := ex.exch.PlaceOrder(ctx, req)
	if err != nil {
		log.Printf("[EXECUTOR] REST place failed: %v", err)
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

	liveMap := make(map[string]*core.LiveOrder, len(liveOrders))
	for i := range liveOrders {
		liveMap[liveOrders[i].OrderID] = &liveOrders[i]
	}

	for _, id := range output.OrdersToCancel {
		if live, ok := liveMap[id]; ok {
			diffs = append(diffs, core.OrderDiff{
				Action: core.DiffCancel,
				Live:   live,
				Reason: output.Reason,
			})
		}
	}

	for i := range output.OrdersToAdd {
		diffs = append(diffs, core.OrderDiff{
			Action:  core.DiffPlace,
			Desired: &output.OrdersToAdd[i],
			Reason:  output.Reason,
		})
	}

	return diffs
}
