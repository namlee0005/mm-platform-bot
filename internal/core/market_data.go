package core

import (
	"fmt"
	"math"
	"strconv"
	"time"

	"mm-platform-engine/internal/exchange"
)

// MarketDataCache caches market info (tick size, step size, etc.) and builds snapshots
type MarketDataCache struct {
	// Cached market constraints
	tickSize    float64
	stepSize    float64
	minNotional float64

	// Last known good prices (fallback when orderbook is empty)
	lastMid     float64
	lastBestBid float64
	lastBestAsk float64

	// Last update time
	lastUpdate time.Time
}

// NewMarketDataCache creates a new market data cache
func NewMarketDataCache() *MarketDataCache {
	return &MarketDataCache{
		minNotional: 5.0, // Default fallback
	}
}

// UpdateFromExchangeInfo updates cached market info from exchange info response
func (m *MarketDataCache) UpdateFromExchangeInfo(info *exchange.ExchangeInfo, symbol string) error {
	for _, sym := range info.Symbols {
		if sym.Symbol == symbol {
			m.tickSize = math.Pow10(-sym.QuoteAssetPrecision)
			m.stepSize = math.Pow10(-sym.BaseAssetPrecision)

			for _, f := range sym.Filters {
				if f.FilterType == "MIN_NOTIONAL" || f.FilterType == "NOTIONAL" {
					if f.MinNotional != "" {
						m.minNotional, _ = strconv.ParseFloat(f.MinNotional, 64)
					}
				}
			}

			if m.minNotional <= 0 {
				m.minNotional = 5.0
			}

			m.lastUpdate = time.Now()
			return nil
		}
	}

	return fmt.Errorf("symbol %s not found in exchange info", symbol)
}

// BuildSnapshot creates a Snapshot from depth data
func (m *MarketDataCache) BuildSnapshot(depth *exchange.Depth) (*Snapshot, error) {
	var bestBid, bestAsk, mid float64
	var bids, asks []PriceLevel

	// Check if we have valid bid/ask data
	hasBids := len(depth.Bids) > 0
	hasAsks := len(depth.Asks) > 0

	if hasBids && hasAsks {
		// Normal case: both sides available
		bestBid, _ = strconv.ParseFloat(depth.Bids[0][0], 64)
		bestAsk, _ = strconv.ParseFloat(depth.Asks[0][0], 64)
		mid = (bestBid + bestAsk) / 2.0

		// Update cached values
		m.lastBestBid = bestBid
		m.lastBestAsk = bestAsk
		m.lastMid = mid

	} else if hasBids && !hasAsks {
		// Only bids available - use bid as reference
		bestBid, _ = strconv.ParseFloat(depth.Bids[0][0], 64)
		if m.lastBestAsk > 0 {
			bestAsk = m.lastBestAsk
		} else {
			// Estimate ask as bid + min spread (10 bps)
			bestAsk = bestBid * 1.001
		}
		mid = (bestBid + bestAsk) / 2.0
		m.lastBestBid = bestBid
		m.lastMid = mid

	} else if !hasBids && hasAsks {
		// Only asks available - use ask as reference
		bestAsk, _ = strconv.ParseFloat(depth.Asks[0][0], 64)
		if m.lastBestBid > 0 {
			bestBid = m.lastBestBid
		} else {
			// Estimate bid as ask - min spread (10 bps)
			bestBid = bestAsk * 0.999
		}
		mid = (bestBid + bestAsk) / 2.0
		m.lastBestAsk = bestAsk
		m.lastMid = mid

	} else {
		// Both empty - use cached values
		if m.lastMid <= 0 {
			return nil, fmt.Errorf("empty order book and no cached price")
		}
		bestBid = m.lastBestBid
		bestAsk = m.lastBestAsk
		mid = m.lastMid
		// Log warning
		fmt.Printf("[MarketData] WARNING: Using cached mid=%.8f (empty orderbook)\n", mid)
	}

	// Parse bids
	bids = make([]PriceLevel, 0, len(depth.Bids))
	for _, b := range depth.Bids {
		p, _ := strconv.ParseFloat(b[0], 64)
		q, _ := strconv.ParseFloat(b[1], 64)
		bids = append(bids, PriceLevel{Price: p, Qty: q})
	}

	// Parse asks
	asks = make([]PriceLevel, 0, len(depth.Asks))
	for _, a := range depth.Asks {
		p, _ := strconv.ParseFloat(a[0], 64)
		q, _ := strconv.ParseFloat(a[1], 64)
		asks = append(asks, PriceLevel{Price: p, Qty: q})
	}

	return &Snapshot{
		BestBid:     bestBid,
		BestAsk:     bestAsk,
		Mid:         mid,
		TickSize:    m.tickSize,
		StepSize:    m.stepSize,
		MinNotional: m.minNotional,
		Bids:        bids,
		Asks:        asks,
		Timestamp:   time.Now(),
	}, nil
}

// SetLastPrice allows setting the last known price from external source (e.g., trades)
func (m *MarketDataCache) SetLastPrice(price float64) {
	if price > 0 {
		m.lastMid = price
		// Estimate bid/ask with 10bps spread
		m.lastBestBid = price * 0.9995
		m.lastBestAsk = price * 1.0005
	}
}

// GetLastMid returns the last known mid price
func (m *MarketDataCache) GetLastMid() float64 {
	return m.lastMid
}

// GetTickSize returns the cached tick size
func (m *MarketDataCache) GetTickSize() float64 {
	return m.tickSize
}

// GetStepSize returns the cached step size
func (m *MarketDataCache) GetStepSize() float64 {
	return m.stepSize
}

// GetMinNotional returns the cached minimum notional
func (m *MarketDataCache) GetMinNotional() float64 {
	return m.minNotional
}

// RoundToTick rounds price to tick size
func (m *MarketDataCache) RoundToTick(price float64) float64 {
	if m.tickSize <= 0 {
		return price
	}
	return math.Round(price/m.tickSize) * m.tickSize
}

// RoundToStep rounds quantity to step size (floors)
func (m *MarketDataCache) RoundToStep(qty float64) float64 {
	if m.stepSize <= 0 {
		return qty
	}
	return math.Floor(qty/m.stepSize) * m.stepSize
}

// IsValidNotional checks if the order notional meets minimum requirements
func (m *MarketDataCache) IsValidNotional(price, qty float64) bool {
	return price*qty >= m.minNotional
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
