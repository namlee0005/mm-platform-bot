package strategy

import (
	"math"
	"sync"

	"github.com/shopspring/decimal"
)

// EWMAMidTracker maintains an Exponential Weighted Moving Average of the Mid-Price.
// This is highly resistant to spoofing on low-liquidity books.
type EWMAMidTracker struct {
	mu           sync.Mutex
	ewmaPrice    decimal.Decimal
	lastUpdate   time.Time
	halflife     time.Duration
	isInitialized bool
}

func NewEWMAMidTracker(halflife time.Duration) *EWMAMidTracker {
	return &EWMAMidTracker{
		halflife: halflife,
	}
}

// Update incorporates a new mid-price observation into the EWMA.
func (t *EWMAMidTracker) Update(mid decimal.Decimal, now time.Time) decimal.Decimal {
	t.mu.Lock()
	defer t.mu.Unlock()

	if !t.isInitialized {
		t.ewmaPrice = mid
		t.lastUpdate = now
		t.isInitialized = true
		return t.ewmaPrice
	}

	dt := now.Sub(t.lastUpdate).Seconds()
	if dt <= 0 {
		return t.ewmaPrice
	}

	// lambda = ln(2) / halflife
	halflifeSec := t.halflife.Seconds()
	if halflifeSec <= 0 {
		halflifeSec = 60.0 // fallback 60s
	}
	lambda := math.Ln2 / halflifeSec
	weight := math.Exp(-lambda * dt)
	decWeight := decimal.NewFromFloat(weight)
	invWeight := decimal.NewFromInt(1).Sub(decWeight)

	// EWMA_new = EWMA_old * weight + Mid_new * (1 - weight)
	t.ewmaPrice = t.ewmaPrice.Mul(decWeight).Add(mid.Mul(invWeight))
	t.lastUpdate = now
	return t.ewmaPrice
}


// OFITracker accumulates order-flow imbalance from the trade tape.
// Internal state is float64 (hot path). Normalized() is the only public exit
// point and returns decimal.Decimal. No float64 OFI escapes this struct.
type OFITracker struct {
	mu         sync.Mutex
	cumulative float64
	runningMax float64
}

const ofiRunningMaxFloor = 1e-6

// Add records a trade. Bid aggressor adds positive flow; Ask subtracts.
func (t *OFITracker) Add(trade Trade) {
	qty, _ := trade.Qty.Float64()
	t.mu.Lock()
	defer t.mu.Unlock()
	if trade.Side == Bid {
		t.cumulative += qty
	} else {
		t.cumulative -= qty
	}
	if abs := math.Abs(t.cumulative); abs > t.runningMax {
		t.runningMax = abs
	}
}

// Normalized returns OFI in [-1, +1] as decimal.Decimal.
// This is the ONLY exit point. Downstream functions must accept decimal.Decimal,
// never float64, for OFI values — enforced at code review.
func (t *OFITracker) Normalized() decimal.Decimal {
	t.mu.Lock()
	defer t.mu.Unlock()
	denom := t.runningMax
	if denom < ofiRunningMaxFloor {
		denom = ofiRunningMaxFloor
	}
	norm := t.cumulative / denom
	if norm > 1.0 {
		norm = 1.0
	} else if norm < -1.0 {
		norm = -1.0
	}
	return decimal.NewFromFloat(norm)
}

// Reset clears accumulated state. Call at session start.
func (t *OFITracker) Reset() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.cumulative = 0
	t.runningMax = 0
}

// ComputeFairValue blends micro-price with the OFI adjustment.
// Formula: fair_value = micro_price + ofi_normalized * ofi_alpha * tick_size
// ofiNorm must be the decimal.Decimal from OFITracker.Normalized().
func ComputeFairValue(microPrice, ofiNorm decimal.Decimal, cfg FairValueConfig) decimal.Decimal {
	adjustment := ofiNorm.Mul(cfg.OFIAlpha).Mul(cfg.TickSize)
	return microPrice.Add(adjustment)
}