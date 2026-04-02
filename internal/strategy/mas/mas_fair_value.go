package mas

import (
	"math"
	"sync"
	"time"

	"github.com/shopspring/decimal"
)

// ────────────────────────────────────────────────────────────────────────────
// EWMAMidTracker
// ────────────────────────────────────────────────────────────────────────────

// EWMAMidTracker maintains a time-weighted Exponential Moving Average of the
// mid-price. Spoofing-resistant for low-liquidity books because the EWMA
// requires sustained price movement to shift fair value materially.
type EWMAMidTracker struct {
	mu            sync.Mutex
	ewmaPrice     decimal.Decimal
	lastUpdate    time.Time
	halflife      time.Duration
	isInitialized bool
}

// NewEWMAMidTracker creates a tracker with the given halflife.
// A halflife of 30s means a price change 30 seconds ago still contributes 50%
// weight to the current EWMA. Typical range: 15s–120s for low-liquidity tokens.
func NewEWMAMidTracker(halflife time.Duration) *EWMAMidTracker {
	return &EWMAMidTracker{halflife: halflife}
}

// Update incorporates a new mid-price observation. Returns the updated EWMA.
// Uses time-based decay (not tick-based) so variable tick rates don't affect
// the effective halflife.
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

	halflifeSec := t.halflife.Seconds()
	if halflifeSec <= 0 {
		halflifeSec = 60.0 // safe fallback
	}

	// weight = e^(-λ·dt), where λ = ln(2) / halflife
	weight := math.Exp(-math.Ln2 / halflifeSec * dt)
	decWeight := decimal.NewFromFloat(weight)
	invWeight := decimal.NewFromFloat(1 - weight)

	// EWMA_new = EWMA_old * weight + Mid_new * (1 - weight)
	t.ewmaPrice = t.ewmaPrice.Mul(decWeight).Add(mid.Mul(invWeight))
	t.lastUpdate = now
	return t.ewmaPrice
}

// ────────────────────────────────────────────────────────────────────────────
// OFITracker
// ────────────────────────────────────────────────────────────────────────────

// OFI tuning constants. These are architecture-level decisions; adjust via a
// config field if per-symbol tuning is needed in the future.
const (
	// ofiFlowAlpha: EWMA decay factor applied to the signed flow each Update call.
	// Equivalent to a ~10-tick halflife: α = 1 − exp(−ln2/10) ≈ 0.067
	// Effect: a one-sided burst fades to 50% of its magnitude after ~10 ticks.
	ofiFlowAlpha float64 = 0.067

	// ofiPeakDecay: fractional decay applied to the peak per Update call when
	// |ewmaFlow| < peak. Equivalent to a ~100-tick halflife.
	// Effect: after a burst subsides, the normalizer takes ~100 ticks to halve,
	// preventing the next burst from immediately saturating to ±1.0.
	ofiPeakDecay float64 = 0.993

	// ofiPeakFloor: minimum denominator — avoids division-by-zero on cold start.
	ofiPeakFloor float64 = 1e-9
)

// OFITracker accumulates order-flow imbalance using EWMA.
//
// Bug fixes over the naive cumulative approach:
//
//  1. Binary flip (old rolling-window): trades fell off a cliff at the window
//     edge, causing OFI to jump discontinuously. Fix: each Update call applies
//     the EWMA factor to ewmaFlow (flow=0 when no trades → ewmaFlow decays
//     smoothly toward zero at rate ~10 ticks).
//
//  2. Peak-inflation (old static runningMax): after any quiet period, runningMax
//     never decayed, so the normalizer crept toward ofiPeakFloor. The first
//     post-quiet burst then divided a large |ewmaFlow| by a near-floor denominator
//     and immediately saturated to ±1.0. Fix: peak grows instantly on a new
//     extreme but only decays at ofiPeakDecay (~100-tick halflife) when below
//     the current |ewmaFlow|. The peak can never shrink faster than the signal
//     itself, so the normalized ratio stays stable.
type OFITracker struct {
	mu       sync.Mutex
	ewmaFlow float64 // EWMA of signed flow: bid aggressors +, ask aggressors -
	peak     float64 // slowly-decaying maximum of |ewmaFlow|, used as denominator
}

// Update records a single aggressor trade.
// isBid=true for bid-side aggressor (buy trade), false for ask-side (sell trade).
// Called once per trade event per tick from the strategy pipeline.
func (t *OFITracker) Update(isBid bool, qty decimal.Decimal) {
	q, _ := qty.Float64()
	flow := q
	if !isBid {
		flow = -q
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	// Apply EWMA to smooth the raw signal.
	// When no trade arrives in a tick, the caller skips Update; next tick the
	// previous ewmaFlow contributes (1−α) weight — decaying toward zero cleanly.
	t.ewmaFlow = ofiFlowAlpha*flow + (1-ofiFlowAlpha)*t.ewmaFlow

	// Update peak: grow immediately on a new extreme; decay slowly when below.
	abs := math.Abs(t.ewmaFlow)
	if abs >= t.peak {
		t.peak = abs
	} else {
		t.peak *= ofiPeakDecay
	}
}

// Normalized returns OFI in [−1, +1] as decimal.Decimal.
// This is the ONLY public exit point for OFI — downstream code must accept
// decimal.Decimal, never float64, for monetary calculations.
func (t *OFITracker) Normalized() decimal.Decimal {
	t.mu.Lock()
	defer t.mu.Unlock()

	denom := t.peak
	if denom < ofiPeakFloor {
		denom = ofiPeakFloor
	}
	norm := t.ewmaFlow / denom
	if norm > 1.0 {
		norm = 1.0
	} else if norm < -1.0 {
		norm = -1.0
	}
	return decimal.NewFromFloat(norm)
}

// Reset clears accumulated state. Call at session start or after a config reload.
func (t *OFITracker) Reset() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.ewmaFlow = 0
	t.peak = 0
}

// ────────────────────────────────────────────────────────────────────────────
// Fair Value
// ────────────────────────────────────────────────────────────────────────────

// ComputeFairValue blends the EWMA mid-price with an OFI momentum adjustment.
//
//	fair_value = ewma_mid + ofi_normalized × ofi_alpha × tick_size
//
// ofiNorm must come from OFITracker.Normalized(). The OFI term is bounded by
// ±(ofi_alpha × tick_size), typically ±1–3 ticks, so it cannot overwhelm the
// EWMA mid on a single burst.
func ComputeFairValue(ewmaMid, ofiNorm decimal.Decimal, cfg FairValueConfig) decimal.Decimal {
	adjustment := ofiNorm.Mul(cfg.OFIAlpha).Mul(cfg.TickSize)
	return ewmaMid.Add(adjustment)
}
