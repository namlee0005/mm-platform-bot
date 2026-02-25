package modules

import (
	"context"
	"fmt"
	"log"
	"math/rand"
	"sync"
	"time"

	"mm-platform-engine/internal/core"
	"mm-platform-engine/internal/exchange"
)

// ExecutionEngine processes order diffs with throttling, jitter, retries,
// and idempotent error handling.
type ExecutionEngine struct {
	cfg     *ExecutionConfig
	antiCfg *AntiAbuseConfig
	exch    exchange.Exchange
	symbol  string

	mu                sync.Mutex
	lastOrderTime     time.Time
	ordersSentThisSec int
	secStart          time.Time

	// Event callback for broadcasting to WS clients
	onOrderEvent core.OrderEventCallback
}

func NewExecutionEngine(
	cfg *ExecutionConfig,
	antiCfg *AntiAbuseConfig,
	exch exchange.Exchange,
	symbol string,
) *ExecutionEngine {
	return &ExecutionEngine{
		cfg:      cfg,
		antiCfg:  antiCfg,
		exch:     exch,
		symbol:   symbol,
		secStart: time.Now(),
	}
}

// SetOrderEventCallback sets the callback for order events
func (ee *ExecutionEngine) SetOrderEventCallback(cb core.OrderEventCallback) {
	ee.onOrderEvent = cb
}

// emitEvent sends an order event to the callback if set
func (ee *ExecutionEngine) emitEvent(eventType core.OrderEventType, orderID, side string, price, qty float64, level int, reason string) {
	if ee.onOrderEvent == nil {
		return
	}
	ee.onOrderEvent(core.BotOrderEvent{
		Type:      eventType,
		Symbol:    ee.symbol,
		OrderID:   orderID,
		Side:      side,
		Price:     price,
		Qty:       qty,
		Level:     level,
		Reason:    reason,
		Timestamp: time.Now().UnixMilli(),
	})
}

// Execute processes a batch of order diffs.
// Order of operations: cancels first, then amends, then places.
func (ee *ExecutionEngine) Execute(ctx context.Context, diffs []core.OrderDiff) error {
	if len(diffs) == 0 {
		return nil
	}

	// Separate by action type
	var cancels, amends, places []core.OrderDiff
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

	// 1. Process cancels
	for _, d := range cancels {
		if err := ee.throttle(ctx); err != nil {
			return err
		}
		if err := ee.executeCancel(ctx, d); err != nil {
			log.Printf("[EXEC] Cancel failed for %s: %v (continuing)", d.Live.OrderID, err)
		}
	}

	// 2. Process amends (cancel + replace)
	for _, d := range amends {
		if err := ee.throttle(ctx); err != nil {
			return err
		}
		if err := ee.executeAmend(ctx, d); err != nil {
			log.Printf("[EXEC] Amend failed for %s: %v (continuing)", d.Live.OrderID, err)
		}
	}

	// 3. Process places (batch if possible)
	if len(places) > 0 {
		if err := ee.executePlaceBatch(ctx, places); err != nil {
			log.Printf("[EXEC] Batch place error: %v", err)
			// Fall back to individual placement
			for _, d := range places {
				if err := ee.throttle(ctx); err != nil {
					return err
				}
				if err := ee.executePlaceSingle(ctx, d); err != nil {
					log.Printf("[EXEC] Place failed for %s: %v", d.Desired.Tag, err)
				}
			}
		}
	}

	return nil
}

// CancelAll cancels all open orders for the symbol
func (ee *ExecutionEngine) CancelAll(ctx context.Context) error {
	log.Printf("[EXEC] Cancelling all orders for %s", ee.symbol)
	return ee.exch.CancelAllOrders(ctx, ee.symbol)
}

func (ee *ExecutionEngine) executeCancel(ctx context.Context, d core.OrderDiff) error {
	ee.addJitter()
	log.Printf("[EXEC] Cancel %s %s L%d @ %.8f (id=%s, reason=%s)",
		ee.symbol, d.Live.Side, d.Live.LevelIndex, d.Live.Price, d.Live.OrderID, d.Reason)

	err := ee.exch.CancelOrder(ctx, ee.symbol, d.Live.OrderID)
	if err == nil {
		ee.emitEvent(core.OrderEventTypeCancel, d.Live.OrderID, d.Live.Side, d.Live.Price, d.Live.Qty, d.Live.LevelIndex, d.Reason)
	}
	return err
}

func (ee *ExecutionEngine) executeAmend(ctx context.Context, d core.OrderDiff) error {
	ee.addJitter()

	log.Printf("[EXEC] Amend %s %s L%d: %.8f@%.8f → %.8f@%.8f (reason=%s)",
		ee.symbol, d.Live.Side, d.Live.LevelIndex,
		d.Live.Qty, d.Live.Price,
		d.Desired.Qty, d.Desired.Price,
		d.Reason)

	ee.emitEvent(core.OrderEventTypeAmend, d.Live.OrderID, d.Live.Side, d.Desired.Price, d.Desired.Qty, d.Live.LevelIndex, d.Reason)

	if err := ee.exch.CancelOrder(ctx, ee.symbol, d.Live.OrderID); err != nil {
		return fmt.Errorf("amend cancel failed: %w", err)
	}

	time.Sleep(time.Duration(50+rand.Intn(100)) * time.Millisecond)

	return ee.executePlaceSingle(ctx, core.OrderDiff{
		Action:  core.DiffPlace,
		Desired: d.Desired,
		Reason:  d.Reason,
	})
}

func (ee *ExecutionEngine) executePlaceBatch(ctx context.Context, places []core.OrderDiff) error {
	if len(places) == 0 {
		return nil
	}

	reqs := make([]*exchange.OrderRequest, 0, len(places))
	for _, d := range places {
		reqs = append(reqs, &exchange.OrderRequest{
			Symbol:        ee.symbol,
			Side:          d.Desired.Side,
			Type:          "LIMIT",
			Price:         d.Desired.Price,
			Quantity:      d.Desired.Qty,
			ClientOrderID: d.Desired.Tag,
		})
	}

	batchSize := ee.cfg.BatchSize
	if batchSize <= 0 {
		batchSize = 20
	}

	for i := 0; i < len(reqs); i += batchSize {
		end := i + batchSize
		if end > len(reqs) {
			end = len(reqs)
		}

		if err := ee.throttle(ctx); err != nil {
			return err
		}
		ee.addJitter()

		resp, err := ee.exch.BatchPlaceOrders(ctx, reqs[i:end])
		if err != nil {
			return fmt.Errorf("batch place failed: %w", err)
		}

		for j, o := range resp.Orders {
			reason := "new_level"
			level := 0
			if i+j < len(places) {
				reason = places[i+j].Reason
				level = places[i+j].Desired.LevelIndex
			}
			log.Printf("[EXEC] Place %s %s L%d %.8f @ %.8f (id=%s, reason=%s)",
				ee.symbol, o.Side, level, o.Quantity, o.Price, o.OrderID, reason)
			ee.emitEvent(core.OrderEventTypePlace, o.OrderID, o.Side, o.Price, o.Quantity, level, reason)
		}
		for _, e := range resp.Errors {
			log.Printf("[EXEC] Batch error: %s", e)
		}
	}

	return nil
}

func (ee *ExecutionEngine) executePlaceSingle(ctx context.Context, d core.OrderDiff) error {
	ee.addJitter()

	req := &exchange.OrderRequest{
		Symbol:        ee.symbol,
		Side:          d.Desired.Side,
		Type:          "LIMIT",
		Price:         d.Desired.Price,
		Quantity:      d.Desired.Qty,
		ClientOrderID: d.Desired.Tag,
	}

	order, err := ee.exch.PlaceOrder(ctx, req)
	if err != nil {
		return fmt.Errorf("place order failed: %w", err)
	}

	log.Printf("[EXEC] Place %s %s L%d %.8f @ %.8f (id=%s, reason=%s)",
		ee.symbol, order.Side, d.Desired.LevelIndex, order.Quantity, order.Price, order.OrderID, d.Reason)
	ee.emitEvent(core.OrderEventTypePlace, order.OrderID, order.Side, order.Price, order.Quantity, d.Desired.LevelIndex, d.Reason)
	return nil
}

func (ee *ExecutionEngine) throttle(ctx context.Context) error {
	ee.mu.Lock()
	defer ee.mu.Unlock()

	now := time.Now()
	if now.Sub(ee.secStart) >= time.Second {
		ee.secStart = now
		ee.ordersSentThisSec = 0
	}

	limit := ee.cfg.RateLimitOrdersPerSec
	if limit <= 0 {
		limit = 10
	}

	if ee.ordersSentThisSec >= limit {
		waitTime := time.Second - now.Sub(ee.secStart)
		if waitTime > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(waitTime):
			}
			ee.secStart = time.Now()
			ee.ordersSentThisSec = 0
		}
	}

	ee.ordersSentThisSec++
	ee.lastOrderTime = time.Now()
	return nil
}

func (ee *ExecutionEngine) addJitter() {
	jitterMs := rand.Intn(50) + 10
	time.Sleep(time.Duration(jitterMs) * time.Millisecond)
}
