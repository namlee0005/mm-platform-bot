package engine

import (
	"time"

	"mm-platform-engine/internal/core"
	"mm-platform-engine/internal/exchange"
	"mm-platform-engine/internal/types"
)

// EngineState holds all mutable state for the engine.
// It is owned exclusively by the engine goroutine — no mutex needed.
type EngineState struct {
	// Market data (updated from WS orderbook events)
	bestBid float64
	bestAsk float64
	bids    []core.PriceLevel // sorted best→worst
	asks    []core.PriceLevel // sorted best→worst

	// Last trade price (from public trades or ticker)
	lastTradePrice float64

	// Market constraints (loaded once at startup via REST)
	tickSize    float64
	stepSize    float64
	minNotional float64
	maxOrderQty float64

	// Balance (reuse existing tracker — mutexes are harmless, accessed single-threaded)
	balanceTracker *core.BalanceTracker

	// Orders (reuse existing tracker)
	orderTracker *core.OrderTracker

	// Recent public trades — rolling buffer for VWAP calculation
	// Capped at maxRecentTrades, oldest evicted first
	recentTrades    []exchange.Trade
	maxRecentTrades int

	// Connection state
	wsConnected bool

	// Recalc tracking
	lastRecalcMid  float64
	lastRecalcTime time.Time

	// Mode
	mode core.Mode

	// Config source
	useLastTradePrice bool

	// Track recently filled orders to ignore late NEW events
	filledOrders    map[string]int64   // orderID → fill timestamp (ms)
	confirmedOrders map[string]bool    // orderID → WS confirmed
	lastExecQty     map[string]float64 // orderID → last known ExecutedQty (for delta calc)
	lastGCTime      int64              // last GC timestamp (ms)
}

// NewEngineState creates a new EngineState with initialized trackers.
func NewEngineState(baseAsset, quoteAsset string, useLastTradePrice bool) *EngineState {
	return &EngineState{
		balanceTracker:    core.NewBalanceTracker(baseAsset, quoteAsset),
		orderTracker:      core.NewOrderTracker(),
		mode:              core.ModeNormal,
		useLastTradePrice: useLastTradePrice,
		recentTrades:      make([]exchange.Trade, 0, 200),
		maxRecentTrades:   200,
		filledOrders:      make(map[string]int64),
		lastExecQty:       make(map[string]float64),
		confirmedOrders:   make(map[string]bool),
	}
}

// SetMarketInfo sets immutable market constraints (called once at startup).
func (s *EngineState) SetMarketInfo(tickSize, stepSize, minNotional, maxOrderQty float64) {
	s.tickSize = tickSize
	s.stepSize = stepSize
	s.minNotional = minNotional
	s.maxOrderQty = maxOrderQty
}

// BuildSnapshot creates a Snapshot from current state for strategy.Tick().
func (s *EngineState) BuildSnapshot() *core.Snapshot {
	mid := 0.0
	if s.bestBid > 0 && s.bestAsk > 0 {
		mid = (s.bestBid + s.bestAsk) / 2
	}
	if s.useLastTradePrice && s.lastTradePrice > 0 {
		mid = s.lastTradePrice
	}
	// Fallback: if mid is still 0 but we have a last trade price
	if mid <= 0 && s.lastTradePrice > 0 {
		mid = s.lastTradePrice
	}

	return &core.Snapshot{
		BestBid:     s.bestBid,
		BestAsk:     s.bestAsk,
		Mid:         mid,
		TickSize:    s.tickSize,
		StepSize:    s.stepSize,
		MinNotional: s.minNotional,
		MaxOrderQty: s.maxOrderQty,
		Bids:        s.bids,
		Asks:        s.asks,
		Timestamp:   time.Now(),
	}
}

// ApplyOrderBookUpdate updates market data from a WS orderbook event.
func (s *EngineState) ApplyOrderBookUpdate(data *OrderBookUpdateData) {
	s.bestBid = data.BestBid
	s.bestAsk = data.BestAsk
	if data.Bids != nil {
		s.bids = data.Bids
	}
	if data.Asks != nil {
		s.asks = data.Asks
	}
}

// ApplyPublicTrade updates last trade price and accumulates in rolling buffer.
func (s *EngineState) ApplyPublicTrade(data *PublicTradeData) {
	if data.Price <= 0 {
		return
	}
	s.lastTradePrice = data.Price

	// Accumulate in rolling buffer for VWAP
	trade := exchange.Trade{
		Price:        data.Price,
		Quantity:     data.Quantity,
		Timestamp:    time.Now(),
		IsBuyerMaker: data.Side == "SELL",
	}
	if len(s.recentTrades) >= s.maxRecentTrades {
		// Evict oldest — shift left
		copy(s.recentTrades, s.recentTrades[1:])
		s.recentTrades[len(s.recentTrades)-1] = trade
	} else {
		s.recentTrades = append(s.recentTrades, trade)
	}
}

// GetRecentTrades returns a copy of the recent trades buffer.
func (s *EngineState) GetRecentTrades() []exchange.Trade {
	result := make([]exchange.Trade, len(s.recentTrades))
	copy(result, s.recentTrades)
	return result
}

// ApplyOrderUpdate processes an order state change.
// Returns (fillDelta, isFull) when a fill occurred.
// fillDelta > 0 means new qty was filled since last update.
// isFull = true means the order is now fully filled.
func (s *EngineState) ApplyOrderUpdate(evt *types.OrderEvent) (fillDelta float64, isFull bool) {
	// Compute fill delta from cumulative ExecutedQty
	if evt.ExecutedQty > 0 {
		prev := s.lastExecQty[evt.OrderID]
		if evt.ExecutedQty > prev {
			fillDelta = evt.ExecutedQty - prev
			s.lastExecQty[evt.OrderID] = evt.ExecutedQty
		}
	}

	switch evt.Status {
	case "NEW":
		// Ignore late NEW for already-filled or confirmed orders
		if _, wasFilled := s.filledOrders[evt.OrderID]; wasFilled {
			return 0, false
		}
		if s.confirmedOrders[evt.OrderID] {
			return 0, false
		}
		s.confirmedOrders[evt.OrderID] = true

		if existing := s.orderTracker.Get(evt.OrderID); existing == nil {
			s.orderTracker.Add(&core.LiveOrder{
				OrderID:       evt.OrderID,
				ClientOrderID: evt.ClientOrderID,
				Side:          evt.Side,
				Price:         evt.Price,
				Qty:           evt.Quantity,
				RemainingQty:  evt.Quantity,
				LevelIndex:    parseLevelFromTag(evt.ClientOrderID),
				PlacedAt:      evt.Timestamp,
			})
		}

	case "PARTIALLY_FILLED":
		s.confirmedOrders[evt.OrderID] = true
		s.orderTracker.UpdateRemaining(evt.OrderID, evt.Quantity-evt.ExecutedQty)

	case "FILLED":
		if _, alreadyTerminal := s.filledOrders[evt.OrderID]; alreadyTerminal {
			return 0, false
		}
		isFull = true
		s.orderTracker.Remove(evt.OrderID)
		delete(s.confirmedOrders, evt.OrderID)
		delete(s.lastExecQty, evt.OrderID)
		s.filledOrders[evt.OrderID] = time.Now().UnixMilli()
		s.gcFilledOrders()

		if evt.Price > 0 {
			s.lastTradePrice = evt.Price
		}

	case "CANCELED", "EXPIRED", "REJECTED":
		if _, alreadyTerminal := s.filledOrders[evt.OrderID]; alreadyTerminal {
			return 0, false
		}
		s.orderTracker.Remove(evt.OrderID)
		delete(s.confirmedOrders, evt.OrderID)
		delete(s.lastExecQty, evt.OrderID)
		s.filledOrders[evt.OrderID] = time.Now().UnixMilli()
		s.gcFilledOrders()
	}

	return fillDelta, isFull
}

// ApplyBalanceUpdate processes a balance event.
func (s *EngineState) ApplyBalanceUpdate(evt *types.AccountEvent) {
	s.balanceTracker.UpdateFromEvent(evt)
}

// UpdateFromRESTAccount updates balance from REST API response.
func (s *EngineState) UpdateFromRESTAccount(acct *exchange.Account) {
	s.balanceTracker.UpdateFromAccount(acct)
}

// SyncOrdersFromREST replaces order tracker state from REST GetOpenOrders.
func (s *EngineState) SyncOrdersFromREST(orders []*exchange.Order) {
	s.orderTracker.Clear()
	for _, o := range orders {
		remaining := o.Quantity - o.ExecutedQty
		if remaining <= 0 {
			continue
		}
		s.orderTracker.Add(&core.LiveOrder{
			OrderID:       o.OrderID,
			ClientOrderID: o.ClientOrderID,
			Side:          o.Side,
			Price:         o.Price,
			Qty:           o.Quantity,
			RemainingQty:  remaining,
			LevelIndex:    parseLevelFromTag(o.ClientOrderID),
			PlacedAt:      time.Now(), // REST doesn't return placement time
		})
	}
}

// GetBalance returns current balance state.
func (s *EngineState) GetBalance() *core.BalanceState {
	return s.balanceTracker.Get()
}

// GetLiveOrders returns a snapshot of all live orders.
func (s *EngineState) GetLiveOrders() []core.LiveOrder {
	return s.orderTracker.GetAll()
}

// OrderCount returns the number of live orders.
func (s *EngineState) OrderCount() int {
	return s.orderTracker.Count()
}

// gcFilledOrders removes entries older than 5 minutes.
// Throttled: runs at most once per 60 seconds.
func (s *EngineState) gcFilledOrders() {
	now := time.Now().UnixMilli()
	if now-s.lastGCTime < 60000 {
		return
	}
	s.lastGCTime = now
	cutoff := now - 300000
	for id, ts := range s.filledOrders {
		if ts < cutoff {
			delete(s.filledOrders, id)
		}
	}
}

// parseLevelFromTag extracts the level index from a client order ID tag.
// Expected format: "mm_B_3" or "mm_S_5" where the last segment is the level.
func parseLevelFromTag(tag string) int {
	if len(tag) < 5 {
		return 0
	}
	// Find last underscore
	lastIdx := -1
	for i := len(tag) - 1; i >= 0; i-- {
		if tag[i] == '_' {
			lastIdx = i
			break
		}
	}
	if lastIdx < 0 || lastIdx >= len(tag)-1 {
		return 0
	}
	level := 0
	for _, c := range tag[lastIdx+1:] {
		if c >= '0' && c <= '9' {
			level = level*10 + int(c-'0')
		} else {
			return 0
		}
	}
	return level
}
