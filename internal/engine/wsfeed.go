package engine

import (
	"context"
	"log"
	"strconv"
	"sync"

	"mm-platform-engine/internal/core"
	"mm-platform-engine/internal/exchange"
	"mm-platform-engine/internal/types"
)

// WSFeedManager subscribes to exchange WebSocket feeds and converts
// callbacks into Events on the EventBus.
type WSFeedManager struct {
	exch   exchange.Exchange
	bus    *EventBus
	symbol string
	botID  string

	mu        sync.Mutex
	connected bool
	cancel    context.CancelFunc
}

// NewWSFeedManager creates a new WSFeedManager.
func NewWSFeedManager(exch exchange.Exchange, bus *EventBus, symbol, botID string) *WSFeedManager {
	return &WSFeedManager{
		exch:   exch,
		bus:    bus,
		symbol: symbol,
		botID:  botID,
	}
}

// Start subscribes to all WebSocket feeds.
func (wf *WSFeedManager) Start(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	wf.cancel = cancel

	// 1. Subscribe to user stream (fills, orders, balance)
	if err := wf.startUserStream(ctx); err != nil {
		cancel()
		return err
	}

	wf.mu.Lock()
	wf.connected = true
	wf.mu.Unlock()

	log.Printf("[WSFeed] Started for %s", wf.symbol)
	return nil
}

// Stop cancels all WebSocket subscriptions.
func (wf *WSFeedManager) Stop() {
	if wf.cancel != nil {
		wf.cancel()
	}
	wf.mu.Lock()
	wf.connected = false
	wf.mu.Unlock()
	log.Printf("[WSFeed] Stopped for %s", wf.symbol)
}

// startUserStream subscribes to private user data (fills, order updates, balance changes).
func (wf *WSFeedManager) startUserStream(ctx context.Context) error {
	handlers := exchange.UserStreamHandlers{
		OnOrderUpdate: func(evt *types.OrderEvent) {
			wf.bus.EmitOrderUpdate(evt)
		},
		OnAccountUpdate: func(evt *types.AccountEvent) {
			wf.bus.EmitBalanceUpdate(evt)
		},
		OnError: func(err error) {
			log.Printf("[WSFeed] User stream error: %v", err)
			wf.mu.Lock()
			wf.connected = false
			wf.mu.Unlock()
			wf.bus.EmitWSDisconnect(err)
		},
	}

	return wf.exch.SubscribeUserStream(ctx, handlers)
}

// handleDepthUpdate converts a raw Depth into an OrderBookUpdate event.
func (wf *WSFeedManager) handleDepthUpdate(depth *exchange.Depth) {
	bids, asks := parseDepthLevels(depth)

	var bestBid, bestAsk float64
	if len(bids) > 0 {
		bestBid = bids[0].Price
	}
	if len(asks) > 0 {
		bestAsk = asks[0].Price
	}

	wf.bus.EmitOrderBookUpdate(bestBid, bestAsk, bids, asks)
}

// parseDepthLevels converts exchange Depth (string arrays) to typed PriceLevels.
func parseDepthLevels(depth *exchange.Depth) (bids, asks []core.PriceLevel) {
	bids = make([]core.PriceLevel, 0, len(depth.Bids))
	for _, b := range depth.Bids {
		if len(b) < 2 {
			continue
		}
		price := parseFloat(b[0])
		qty := parseFloat(b[1])
		if price > 0 && qty > 0 {
			bids = append(bids, core.PriceLevel{Price: price, Qty: qty})
		}
	}

	asks = make([]core.PriceLevel, 0, len(depth.Asks))
	for _, a := range depth.Asks {
		if len(a) < 2 {
			continue
		}
		price := parseFloat(a[0])
		qty := parseFloat(a[1])
		if price > 0 && qty > 0 {
			asks = append(asks, core.PriceLevel{Price: price, Qty: qty})
		}
	}

	return bids, asks
}

// parseFloat parses a string to float64. Returns 0 on error.
func parseFloat(s string) float64 {
	v, _ := strconv.ParseFloat(s, 64)
	return v
}
