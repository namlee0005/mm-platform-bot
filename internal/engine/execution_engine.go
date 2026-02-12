package engine

import (
	"context"
	"fmt"
	"log"
	"math/rand"
	"sync"
	"time"

	"mm-platform-engine/internal/exchange"
)

// OrderEventType represents the type of order event
type OrderEventType string

const (
	OrderEventPlace  OrderEventType = "place"
	OrderEventCancel OrderEventType = "cancel"
	OrderEventAmend  OrderEventType = "amend"
	OrderEventFill   OrderEventType = "fill"
)

// OrderEvent represents an order event for broadcasting
type OrderEvent struct {
	Type      OrderEventType `json:"type"`
	Symbol    string         `json:"symbol"`
	OrderID   string         `json:"order_id"`
	Side      string         `json:"side"`
	Price     float64        `json:"price"`
	Qty       float64        `json:"qty"`
	Level     int            `json:"level"`
	Reason    string         `json:"reason"`
	Timestamp int64          `json:"timestamp"`
}

// OrderEventCallback is called when an order event occurs
type OrderEventCallback func(event OrderEvent)

// ExecutionEngine processes order diffs with throttling, jitter, retries,
// and idempotent error handling.
type ExecutionEngine struct {
	cfg      *ExecutionConfig
	antiCfg  *AntiAbuseConfig
	exchange exchange.Exchange
	symbol   string

	mu                sync.Mutex
	lastOrderTime     time.Time
	ordersSentThisSec int
	secStart          time.Time

	// Event callback for broadcasting to WS clients
	onOrderEvent OrderEventCallback
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
		exchange: exch,
		symbol:   symbol,
		secStart: time.Now(),
	}
}

// SetOrderEventCallback sets the callback for order events
func (ee *ExecutionEngine) SetOrderEventCallback(cb OrderEventCallback) {
	ee.onOrderEvent = cb
}

// emitEvent sends an order event to the callback if set
func (ee *ExecutionEngine) emitEvent(eventType OrderEventType, orderID, side string, price, qty float64, level int, reason string) {
	if ee.onOrderEvent == nil {
		return
	}
	ee.onOrderEvent(OrderEvent{
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
// This minimizes the time we have stale orders on the book.
func (ee *ExecutionEngine) Execute(ctx context.Context, diffs []OrderDiff) error {
	if len(diffs) == 0 {
		return nil
	}

	// Separate by action type
	var cancels, amends, places []OrderDiff
	for _, d := range diffs {
		switch d.Action {
		case DiffCancel:
			cancels = append(cancels, d)
		case DiffAmend:
			amends = append(amends, d)
		case DiffPlace:
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
			// Non-fatal: order might already be filled/cancelled
		}
	}

	// 2. Process amends (cancel + replace since most CEXs don't support atomic amend)
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
	return ee.exchange.CancelAllOrders(ctx, ee.symbol)
}

func (ee *ExecutionEngine) executeCancel(ctx context.Context, d OrderDiff) error {
	ee.addJitter()
	log.Printf("[EXEC] Cancel %s %s L%d @ %.8f (id=%s, reason=%s)",
		ee.symbol, d.Live.Side, d.Live.LevelIndex, d.Live.Price, d.Live.OrderID, d.Reason)

	err := ee.exchange.CancelOrder(ctx, ee.symbol, d.Live.OrderID)
	if err == nil {
		ee.emitEvent(OrderEventCancel, d.Live.OrderID, d.Live.Side, d.Live.Price, d.Live.Qty, d.Live.LevelIndex, d.Reason)
	}
	return err
}

func (ee *ExecutionEngine) executeAmend(ctx context.Context, d OrderDiff) error {
	// Most CEXs don't support atomic amend — cancel then place
	ee.addJitter()

	log.Printf("[EXEC] Amend %s %s L%d: %.8f@%.8f → %.8f@%.8f (reason=%s)",
		ee.symbol, d.Live.Side, d.Live.LevelIndex,
		d.Live.Qty, d.Live.Price,
		d.Desired.Qty, d.Desired.Price,
		d.Reason)

	// Emit amend event (shows the transition)
	ee.emitEvent(OrderEventAmend, d.Live.OrderID, d.Live.Side, d.Desired.Price, d.Desired.Qty, d.Live.LevelIndex, d.Reason)

	if err := ee.exchange.CancelOrder(ctx, ee.symbol, d.Live.OrderID); err != nil {
		// If cancel fails (already filled?), don't place replacement
		return fmt.Errorf("amend cancel failed: %w", err)
	}

	// Small delay between cancel and place
	time.Sleep(time.Duration(50+rand.Intn(100)) * time.Millisecond)

	return ee.executePlaceSingle(ctx, OrderDiff{
		Action:  DiffPlace,
		Desired: d.Desired,
		Reason:  d.Reason,
	})
}

func (ee *ExecutionEngine) executePlaceBatch(ctx context.Context, places []OrderDiff) error {
	if len(places) == 0 {
		return nil
	}

	// Build batch request
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

	// Split into batches if needed
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

		resp, err := ee.exchange.BatchPlaceOrders(ctx, reqs[i:end])
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
			ee.emitEvent(OrderEventPlace, o.OrderID, o.Side, o.Price, o.Quantity, level, reason)
		}
		for _, e := range resp.Errors {
			log.Printf("[EXEC] Batch error: %s", e)
		}
	}

	return nil
}

func (ee *ExecutionEngine) executePlaceSingle(ctx context.Context, d OrderDiff) error {
	ee.addJitter()

	req := &exchange.OrderRequest{
		Symbol:        ee.symbol,
		Side:          d.Desired.Side,
		Type:          "LIMIT",
		Price:         d.Desired.Price,
		Quantity:      d.Desired.Qty,
		ClientOrderID: d.Desired.Tag,
	}

	order, err := ee.exchange.PlaceOrder(ctx, req)
	if err != nil {
		return fmt.Errorf("place order failed: %w", err)
	}

	log.Printf("[EXEC] Place %s %s L%d %.8f @ %.8f (id=%s, reason=%s)",
		ee.symbol, order.Side, d.Desired.LevelIndex, order.Quantity, order.Price, order.OrderID, d.Reason)
	ee.emitEvent(OrderEventPlace, order.OrderID, order.Side, order.Price, order.Quantity, d.Desired.LevelIndex, d.Reason)
	return nil
}

// throttle implements rate limiting
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

// addJitter adds a small random delay to avoid predictable timing
func (ee *ExecutionEngine) addJitter() {
	jitterMs := rand.Intn(50) + 10 // 10-60ms
	time.Sleep(time.Duration(jitterMs) * time.Millisecond)
}
