package engine

import (
	"context"
	"log"
	"time"

	"mm-platform-engine/internal/types"
)

// restFallbackLoop runs a REST polling loop when WS is disconnected.
// Stopped via context cancellation when WS reconnects.
// Only one instance runs at a time (guarded by engine.fallbackRunning).
func (e *Engine) restFallbackLoop(ctx context.Context) {
	interval := time.Duration(e.cfg.FallbackPollIntervalMs) * time.Millisecond
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	defer func() {
		e.fallbackRunning.Store(false)
		log.Printf("[FALLBACK] REST polling stopped")
	}()

	log.Printf("[FALLBACK] REST polling started (every %v)", interval)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			e.restFallbackRefresh(ctx)
			e.bus.Emit(Event{
				Type:      EventTimerTick,
				Timestamp: time.Now(),
			})
		}
	}
}

// refreshMarketData fetches orderbook + ticker via REST.
// Called every backstop tick since we don't have WS public streams.
func (e *Engine) refreshMarketData(ctx context.Context) {
	depth, err := e.exch.GetDepth(ctx, e.cfg.Symbol)
	if err != nil {
		log.Printf("[ENGINE] GetDepth failed: %v", err)
		return
	}
	bids, asks := parseDepthLevels(depth)
	var bestBid, bestAsk float64
	if len(bids) > 0 {
		bestBid = bids[0].Price
	}
	if len(asks) > 0 {
		bestAsk = asks[0].Price
	}
	e.state.ApplyOrderBookUpdate(&OrderBookUpdateData{
		BestBid: bestBid, BestAsk: bestAsk, Bids: bids, Asks: asks,
	})

	if ticker, err := e.exch.GetTicker(ctx, e.cfg.Symbol); err == nil && ticker > 0 {
		e.state.lastTradePrice = ticker
	}
}

// restFallbackRefresh fetches current state via REST APIs.
func (e *Engine) restFallbackRefresh(ctx context.Context) {
	log.Printf("[FALLBACK] DEBUG: REST refresh starting")
	depth, err := e.exch.GetDepth(ctx, e.cfg.Symbol)
	if err != nil {
		log.Printf("[FALLBACK] GetDepth failed: %v", err)
	} else {
		bids, asks := parseDepthLevels(depth)
		var bestBid, bestAsk float64
		if len(bids) > 0 {
			bestBid = bids[0].Price
		}
		if len(asks) > 0 {
			bestAsk = asks[0].Price
		}
		e.state.ApplyOrderBookUpdate(&OrderBookUpdateData{
			BestBid: bestBid, BestAsk: bestAsk, Bids: bids, Asks: asks,
		})
	}

	if ticker, err := e.exch.GetTicker(ctx, e.cfg.Symbol); err == nil && ticker > 0 {
		e.state.lastTradePrice = ticker
	}

	acct, err := e.exch.GetAccount(ctx)
	if err != nil {
		log.Printf("[FALLBACK] GetAccount failed: %v", err)
	} else {
		e.state.UpdateFromRESTAccount(acct)
	}

	orders, err := e.exch.GetOpenOrders(ctx, e.cfg.Symbol)
	if err != nil {
		log.Printf("[FALLBACK] GetOpenOrders failed: %v", err)
	} else {
		e.state.SyncOrdersFromREST(orders)
	}

	log.Printf("[FALLBACK] DEBUG: REST refresh done — bid=%.8f ask=%.8f orders=%d",
		e.state.bestBid, e.state.bestAsk, e.state.OrderCount())
}

// orderHistorySyncLoop periodically fetches closed orders from exchange API
// and saves filled orders to MongoDB for durable order history.
func (e *Engine) orderHistorySyncLoop(ctx context.Context) {
	if e.mongo == nil {
		return
	}

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	// Get latest order timestamp from MongoDB as starting point
	lastSync, err := e.mongo.GetLatestOrderTimestamp(ctx, e.cfg.Symbol)
	if err != nil {
		lastSync = time.Now().Add(-24 * time.Hour) // fallback: look back 24h
		log.Printf("[ENGINE] No previous orders in DB, syncing from %v", lastSync.Format("15:04:05"))
	} else {
		log.Printf("[ENGINE] Order history sync starting from %v", lastSync.Format("15:04:05"))
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			orders, err := e.exch.FetchClosedOrders(ctx, e.cfg.Symbol, lastSync, 50)
			if err != nil {
				log.Printf("[ENGINE] Order history sync error: %v", err)
				continue
			}

			saved := 0
			for _, o := range orders {
				if err := e.mongo.InsertFilledOrder(ctx, &types.OrderEvent{
					OrderID:            o.OrderID,
					ClientOrderID:      o.ClientOrderID,
					Symbol:             o.Symbol,
					Side:               o.Side,
					Status:             o.Status,
					Price:              o.Price,
					Quantity:           o.Quantity,
					ExecutedQty:        o.ExecutedQty,
					CumulativeQuoteQty: o.CumulativeQuoteQty,
					Timestamp:          o.Timestamp,
				}); err != nil {
					log.Printf("[ENGINE] Failed to save filled order %s: %v", o.OrderID, err)
				} else {
					saved++
					if o.Timestamp.After(lastSync) {
						lastSync = o.Timestamp
					}
				}
			}

			if saved > 0 {
				log.Printf("[ENGINE] Synced %d filled orders to DB", saved)
			}
		}
	}
}
