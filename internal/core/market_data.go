package core

import (
	"fmt"
	"math"
	"strconv"
	"sync"
	"time"

	"mm-platform-engine/internal/exchange"
)

const defaultStaleness = 10 * time.Second

// MarketDataCache caches market info (tick size, step size, etc.) and builds snapshots.
// Thread-safe: WS goroutines write, tick() reads.
type MarketDataCache struct {
	mu sync.RWMutex

	// Cached market constraints
	tickSize    float64
	stepSize    float64
	minNotional float64
	maxOrderQty float64 // Maximum quantity per single order

	// Last known good prices (fallback when orderbook is empty)
	lastMid     float64
	lastBestBid float64
	lastBestAsk float64

	// Last trade price (preferred over mid when available)
	lastTradePrice float64
	useLastTrade   bool // If true, prefer lastTradePrice over calculated mid

	// Last update time
	lastUpdate time.Time

	// WS-cached data with staleness tracking
	cachedDepth    *exchange.Depth
	lastDepthTime  time.Time
	lastTickerTime time.Time
	cachedTrades   []exchange.Trade
	lastTradesTime time.Time
	stalenessLimit time.Duration
}

// NewMarketDataCache creates a new market data cache
func NewMarketDataCache() *MarketDataCache {
	return &MarketDataCache{
		minNotional:    5.0, // Default fallback
		stalenessLimit: defaultStaleness,
	}
}

// ── WS cache update methods (called from WS goroutines) ──

// UpdateDepth stores a fresh orderbook from WS.
func (m *MarketDataCache) UpdateDepth(depth *exchange.Depth) {
	m.mu.Lock()
	m.cachedDepth = depth
	m.lastDepthTime = time.Now()
	m.mu.Unlock()
}

// UpdateRecentTrades stores recent public trades from WS.
func (m *MarketDataCache) UpdateRecentTrades(trades []exchange.Trade) {
	m.mu.Lock()
	m.cachedTrades = trades
	m.lastTradesTime = time.Now()
	// Also update last trade price from most recent trade
	if len(trades) > 0 {
		m.lastTradePrice = trades[len(trades)-1].Price
		m.lastTickerTime = time.Now()
	}
	m.mu.Unlock()
}

// ── WS cache read methods (called from tick) ──

// GetCachedDepth returns cached depth and whether it is fresh.
func (m *MarketDataCache) GetCachedDepth() (*exchange.Depth, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.cachedDepth == nil {
		return nil, false
	}
	fresh := time.Since(m.lastDepthTime) < m.stalenessLimit
	return m.cachedDepth, fresh
}

// GetCachedTrades returns cached trades and whether they are fresh.
func (m *MarketDataCache) GetCachedTrades() ([]exchange.Trade, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.cachedTrades == nil {
		return nil, false
	}
	fresh := time.Since(m.lastTradesTime) < m.stalenessLimit
	return m.cachedTrades, fresh
}

// IsTickerFresh returns true if last trade price was updated recently.
func (m *MarketDataCache) IsTickerFresh() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.lastTradePrice > 0 && time.Since(m.lastTickerTime) < m.stalenessLimit
}

// ── Existing methods (now thread-safe) ──

// UpdateFromExchangeInfo updates cached market info from exchange info response
func (m *MarketDataCache) UpdateFromExchangeInfo(info *exchange.ExchangeInfo, symbol string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, sym := range info.Symbols {
		if sym.Symbol == symbol {
			m.tickSize = math.Pow10(-sym.QuoteAssetPrecision)
			m.stepSize = math.Pow10(-sym.BaseAssetPrecision)

			for _, f := range sym.Filters {
				if f.FilterType == "MIN_NOTIONAL" || f.FilterType == "NOTIONAL" {
					if f.MinNotional != "" {
						if val, err := strconv.ParseFloat(f.MinNotional, 64); err == nil {
							m.minNotional = val
						} else {
							fmt.Printf("[MarketData] WARNING: failed to parse minNotional '%s': %v\n", f.MinNotional, err)
						}
					}
				}
				if f.FilterType == "LOT_SIZE" {
					if f.MaxQty != "" {
						if val, err := strconv.ParseFloat(f.MaxQty, 64); err == nil {
							m.maxOrderQty = val
						}
					}
				}
			}

			if m.minNotional <= 0 {
				m.minNotional = 5.0
			}
			if m.maxOrderQty <= 0 {
				m.maxOrderQty = 1000000
			}

			m.lastUpdate = time.Now()
			return nil
		}
	}

	return fmt.Errorf("symbol %s not found in exchange info", symbol)
}

// BuildSnapshot creates a Snapshot from depth data
func (m *MarketDataCache) BuildSnapshot(depth *exchange.Depth) (*Snapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	var bestBid, bestAsk, mid float64
	var bids, asks []PriceLevel

	hasBids := len(depth.Bids) > 0
	hasAsks := len(depth.Asks) > 0

	if hasBids && hasAsks {
		bestBid, _ = strconv.ParseFloat(depth.Bids[0][0], 64)
		bestAsk, _ = strconv.ParseFloat(depth.Asks[0][0], 64)
		mid = (bestBid + bestAsk) / 2.0
		m.lastBestBid = bestBid
		m.lastBestAsk = bestAsk
		m.lastMid = mid
	} else if hasBids && !hasAsks {
		bestBid, _ = strconv.ParseFloat(depth.Bids[0][0], 64)
		if m.lastBestAsk > 0 {
			bestAsk = m.lastBestAsk
		} else {
			bestAsk = bestBid * 1.001
		}
		mid = (bestBid + bestAsk) / 2.0
		m.lastBestBid = bestBid
		m.lastMid = mid
	} else if !hasBids && hasAsks {
		bestAsk, _ = strconv.ParseFloat(depth.Asks[0][0], 64)
		if m.lastBestBid > 0 {
			bestBid = m.lastBestBid
		} else {
			bestBid = bestAsk * 0.999
		}
		mid = (bestBid + bestAsk) / 2.0
		m.lastBestAsk = bestAsk
		m.lastMid = mid
	} else {
		if m.lastMid <= 0 {
			return nil, fmt.Errorf("empty order book and no cached price")
		}
		bestBid = m.lastBestBid
		bestAsk = m.lastBestAsk
		mid = m.lastMid
		fmt.Printf("[MarketData] WARNING: Using cached mid=%.8f (empty orderbook)\n", mid)
	}

	bids = make([]PriceLevel, 0, len(depth.Bids))
	for _, b := range depth.Bids {
		p, _ := strconv.ParseFloat(b[0], 64)
		q, _ := strconv.ParseFloat(b[1], 64)
		bids = append(bids, PriceLevel{Price: p, Qty: q})
	}

	asks = make([]PriceLevel, 0, len(depth.Asks))
	for _, a := range depth.Asks {
		p, _ := strconv.ParseFloat(a[0], 64)
		q, _ := strconv.ParseFloat(a[1], 64)
		asks = append(asks, PriceLevel{Price: p, Qty: q})
	}

	if m.useLastTrade && m.lastTradePrice > 0 {
		mid = m.lastTradePrice
	}

	return &Snapshot{
		BestBid:     bestBid,
		BestAsk:     bestAsk,
		Mid:         mid,
		TickSize:    m.tickSize,
		StepSize:    m.stepSize,
		MinNotional: m.minNotional,
		MaxOrderQty: m.maxOrderQty,
		Bids:        bids,
		Asks:        asks,
		Timestamp:   time.Now(),
	}, nil
}

// SetLastPrice allows setting the last known price from external source
func (m *MarketDataCache) SetLastPrice(price float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if price > 0 {
		m.lastMid = price
		m.lastBestBid = price * 0.9995
		m.lastBestAsk = price * 1.0005
	}
}

// SetLastTradePrice sets the last trade price
func (m *MarketDataCache) SetLastTradePrice(price float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if price > 0 {
		m.lastTradePrice = price
		m.lastTickerTime = time.Now()
	}
}

// UseLastTradePrice enables using last trade price instead of calculated mid
func (m *MarketDataCache) UseLastTradePrice(enable bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.useLastTrade = enable
}

// GetLastMid returns the last known mid price
func (m *MarketDataCache) GetLastMid() float64 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.lastMid
}

// GetLastTradePrice returns the last trade price
func (m *MarketDataCache) GetLastTradePrice() float64 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.lastTradePrice
}

// GetTickSize returns the cached tick size
func (m *MarketDataCache) GetTickSize() float64 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.tickSize
}

// GetStepSize returns the cached step size
func (m *MarketDataCache) GetStepSize() float64 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.stepSize
}

// GetMinNotional returns the cached minimum notional
func (m *MarketDataCache) GetMinNotional() float64 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.minNotional
}

// RoundToTick rounds price to tick size
func (m *MarketDataCache) RoundToTick(price float64) float64 {
	m.mu.RLock()
	ts := m.tickSize
	m.mu.RUnlock()
	if ts <= 0 {
		return price
	}
	return math.Round(price/ts) * ts
}

// RoundToStep rounds quantity to step size (floors)
func (m *MarketDataCache) RoundToStep(qty float64) float64 {
	m.mu.RLock()
	ss := m.stepSize
	m.mu.RUnlock()
	if ss <= 0 {
		return qty
	}
	return math.Floor(qty/ss) * ss
}

// IsValidNotional checks if the order notional meets minimum requirements
func (m *MarketDataCache) IsValidNotional(price, qty float64) bool {
	m.mu.RLock()
	mn := m.minNotional
	m.mu.RUnlock()
	return price*qty >= mn
}

// CalculateSpread returns spread in basis points
func CalculateSpreadBps(bestBid, bestAsk float64) float64 {
	mid := (bestBid + bestAsk) / 2.0
	if mid <= 0 {
		return 0
	}
	return (bestAsk - bestBid) / mid * 10000.0
}

// CalculateDepthNotional calculates total depth within a price range
func CalculateDepthNotional(bids, asks []PriceLevel, mid, rangePct float64) (bidDepth, askDepth float64) {
	rangeAbs := mid * rangePct

	for _, b := range bids {
		if b.Price >= mid-rangeAbs {
			bidDepth += b.Price * b.Qty
		}
	}

	for _, a := range asks {
		if a.Price <= mid+rangeAbs {
			askDepth += a.Price * a.Qty
		}
	}

	return
}
