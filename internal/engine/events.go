package engine

import (
	"log"
	"time"

	"mm-platform-engine/internal/core"
	"mm-platform-engine/internal/types"
)

// EventType classifies events flowing through the EventBus.
type EventType int

const (
	// Market data events (public WebSocket)
	EventOrderBookUpdate EventType = iota + 1 // Best bid/ask or depth changed
	EventPublicTrade                          // Public trade executed

	// User data events (private WebSocket)
	_                  // reserved (was EventFill — now handled via EventOrderUpdate)
	EventOrderUpdate   // Order status changed + fill delta computed internally
	EventBalanceUpdate // Account balance changed

	// Internal events
	EventConfigReload // Hot config reload from MongoDB
	EventWSDisconnect // WebSocket connection lost
	EventWSReconnect  // WebSocket reconnected
	EventTimerTick    // Backstop timer fired (fallback)
	EventForceRecalc  // Manual triggered recalc
)

// String returns a human-readable name for the event type.
func (e EventType) String() string {
	switch e {
	case EventOrderBookUpdate:
		return "OrderBookUpdate"
	case EventPublicTrade:
		return "PublicTrade"
	case EventOrderUpdate:
		return "OrderUpdate"
	case EventBalanceUpdate:
		return "BalanceUpdate"
	case EventConfigReload:
		return "ConfigReload"
	case EventWSDisconnect:
		return "WSDisconnect"
	case EventWSReconnect:
		return "WSReconnect"
	case EventTimerTick:
		return "TimerTick"
	case EventForceRecalc:
		return "ForceRecalc"
	default:
		return "Unknown"
	}
}

// Event is a single message flowing through the EventBus.
type Event struct {
	Type      EventType
	Timestamp time.Time
	Data      interface{} // Typed per EventType — see *Data structs below
}

// OrderBookUpdateData carries orderbook state from WS.
type OrderBookUpdateData struct {
	BestBid float64
	BestAsk float64
	Bids    []core.PriceLevel // Full depth (may be nil for BBO-only)
	Asks    []core.PriceLevel
}

// PublicTradeData carries a single public trade.
type PublicTradeData struct {
	Price    float64
	Quantity float64
	Side     string
	IsMaker  bool
}

// ConfigReloadData carries the new configuration.
type ConfigReloadData struct {
	NewCfg interface{}
}

// FillEventData wraps types.FillEvent for the bus.
// We use the existing types directly — no wrapper needed for:
//   - EventOrderUpdate   → *types.OrderEvent (fill delta computed by EngineState)
//   - EventBalanceUpdate → *types.AccountEvent
//   - EventWSDisconnect  → error
//   - EventWSReconnect   → nil
//   - EventTimerTick     → nil
//   - EventForceRecalc   → nil

// EventBus is a fan-in channel: multiple producers Emit events,
// a single consumer (Engine) reads from C().
type EventBus struct {
	ch       chan Event
	capacity int
	dropped  int64 // counter for monitoring
}

// NewEventBus creates an EventBus with the given buffer capacity.
func NewEventBus(capacity int) *EventBus {
	if capacity <= 0 {
		capacity = 256
	}
	return &EventBus{
		ch:       make(chan Event, capacity),
		capacity: capacity,
	}
}

// Emit sends an event to the bus. Non-blocking: drops the event if
// the channel is full. This is intentional — the engine recalculates
// using current state, so skipping an intermediate event is safe.
func (b *EventBus) Emit(evt Event) bool {
	select {
	case b.ch <- evt:
		return true
	default:
		b.dropped++
		if b.dropped%100 == 1 {
			log.Printf("[EventBus] dropped event %s (total dropped: %d)", evt.Type, b.dropped)
		}
		return false
	}
}

// C returns the read-only channel for the engine's select loop.
func (b *EventBus) C() <-chan Event {
	return b.ch
}

// EmitOrderBookUpdate is a convenience helper.
func (b *EventBus) EmitOrderBookUpdate(bestBid, bestAsk float64, bids, asks []core.PriceLevel) {
	b.Emit(Event{
		Type:      EventOrderBookUpdate,
		Timestamp: time.Now(),
		Data: &OrderBookUpdateData{
			BestBid: bestBid,
			BestAsk: bestAsk,
			Bids:    bids,
			Asks:    asks,
		},
	})
}

// EmitOrderUpdate is a convenience helper.
func (b *EventBus) EmitOrderUpdate(evt *types.OrderEvent) {
	b.Emit(Event{
		Type:      EventOrderUpdate,
		Timestamp: evt.Timestamp,
		Data:      evt,
	})
}

// EmitBalanceUpdate is a convenience helper.
func (b *EventBus) EmitBalanceUpdate(evt *types.AccountEvent) {
	b.Emit(Event{
		Type:      EventBalanceUpdate,
		Timestamp: evt.Timestamp,
		Data:      evt,
	})
}

// EmitWSDisconnect signals a WebSocket disconnect.
func (b *EventBus) EmitWSDisconnect(err error) {
	b.Emit(Event{
		Type:      EventWSDisconnect,
		Timestamp: time.Now(),
		Data:      err,
	})
}
