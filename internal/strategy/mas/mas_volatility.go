package mas

import (
	"math"
	"sync"
)

// VolatilityTracker computes realized volatility via EWMA of squared log-returns.
// Concurrency-safe.
// Lambda: λ = exp(-ln(2)/halflife_seconds). Default 15s → λ ≈ 0.955.
// Spike registers within ~30s (2× halflife) vs. 60s for a 60s rolling window.
type VolatilityTracker struct {
	mu      sync.Mutex
	lambda  float64
	ewmaVar float64
	prevMid float64
	seeded  bool
}

// NewVolatilityTracker constructs a tracker. Pass VolatilityConfig.EWMAHalflifeSeconds.
func NewVolatilityTracker(halflifeSeconds float64) *VolatilityTracker {
	if halflifeSeconds <= 0 {
		halflifeSeconds = 15
	}
	return &VolatilityTracker{
		lambda: math.Exp(-math.Log(2) / halflifeSeconds),
	}
}

// Update ingests a mid-price and advances ewma_var.
// Formula: ewma_var = λ*prev_ewma_var + (1-λ)*log_return²
// First call seeds prevMid only — no return computed (avoids log(x/0)).
func (v *VolatilityTracker) Update(midPrice float64) {
	if midPrice <= 0 {
		return
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if !v.seeded {
		v.prevMid = midPrice
		v.seeded = true
		return
	}
	logReturn := math.Log(midPrice / v.prevMid)
	v.ewmaVar = v.lambda*v.ewmaVar + (1-v.lambda)*logReturn*logReturn
	v.prevMid = midPrice
}

// EWMAVol returns sqrt(ewma_var). Returns 0 before the second Update call.
func (v *VolatilityTracker) EWMAVol() float64 {
	v.mu.Lock()
	defer v.mu.Unlock()
	return math.Sqrt(v.ewmaVar)
}

// EWMAVar returns the raw variance for storage in MASState.EWMAVar.
func (v *VolatilityTracker) EWMAVar() float64 {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.ewmaVar
}

// Reset clears all state. Call at session start.
func (v *VolatilityTracker) Reset() {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.ewmaVar = 0
	v.prevMid = 0
	v.seeded = false
}

// ClassifyRegime applies hysteresis to prevent thrashing at the vol boundary.
// Enter Spike: vol >= cfg.VolThreshold (from Sideways).
// Exit  Spike: vol <  cfg.VolExitThreshold (back to Sideways).
// VolExitThreshold must be < VolThreshold (typically 80% of it).
func ClassifyRegime(vol float64, current VolatilityRegime, cfg VolatilityConfig) VolatilityRegime {
	switch current {
	case RegimeSideways:
		if vol >= cfg.VolThreshold {
			return RegimeSpike
		}
		return RegimeSideways
	case RegimeSpike:
		if vol < cfg.VolExitThreshold {
			return RegimeSideways
		}
		return RegimeSpike
	default:
		return RegimeSideways
	}
}