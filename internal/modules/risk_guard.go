package modules

import (
	"math"
	"sync"
	"time"
)

// RiskConfig contains configuration for risk management
type RiskConfig struct {
	DrawdownLimitPct     float64 `json:"drawdown_limit_pct" bson:"drawdown_limit_pct"`
	DrawdownWarnPct      float64 `json:"drawdown_warn_pct" bson:"drawdown_warn_pct"`
	DrawdownAction       string  `json:"drawdown_action" bson:"drawdown_action"` // "reduce" or "pause"
	MaxFillsPerMin       float64 `json:"max_fills_per_min" bson:"max_fills_per_min"`
	MinOrderLifetimeMs   int     `json:"min_order_lifetime_ms" bson:"min_order_lifetime_ms"`
	CooldownAfterFillMs  int     `json:"cooldown_after_fill_ms" bson:"cooldown_after_fill_ms"`
	DefensiveCooldownSec int     `json:"defensive_cooldown_sec" bson:"defensive_cooldown_sec"`
}

// NAVPoint is a point in the NAV time series for drawdown calculation
type NAVPoint struct {
	Timestamp time.Time
	NAV       float64
}

// RiskGuard tracks drawdown, fill rate, and manages mode transitions.
type RiskGuard struct {
	mu  sync.RWMutex
	cfg *RiskConfig

	// NAV history for drawdown calculation (24h rolling window)
	navHistory []NAVPoint
	peakNAV    float64

	// Fill tracking for rate limiting
	fillTimestamps []time.Time

	// Defensive mode tracking
	defensiveEnteredAt time.Time
	lastFillAt         time.Time
}

func NewRiskGuard(cfg *RiskConfig) *RiskGuard {
	return &RiskGuard{
		cfg:            cfg,
		navHistory:     make([]NAVPoint, 0, 8640), // ~1 point per 10s for 24h
		fillTimestamps: make([]time.Time, 0, 1024),
	}
}

// RecordNAV records a NAV observation for drawdown tracking
func (rg *RiskGuard) RecordNAV(nav float64, ts time.Time) {
	rg.mu.Lock()
	defer rg.mu.Unlock()

	rg.navHistory = append(rg.navHistory, NAVPoint{Timestamp: ts, NAV: nav})

	// Update peak
	if nav > rg.peakNAV {
		rg.peakNAV = nav
	}

	// Trim to 24h window
	cutoff := ts.Add(-24 * time.Hour)
	for len(rg.navHistory) > 0 && rg.navHistory[0].Timestamp.Before(cutoff) {
		rg.navHistory = rg.navHistory[1:]
	}

	// Recompute peak from remaining window
	if len(rg.navHistory) > 0 {
		rg.peakNAV = 0
		for _, p := range rg.navHistory {
			if p.NAV > rg.peakNAV {
				rg.peakNAV = p.NAV
			}
		}
	}
}

// Drawdown24h returns the max drawdown fraction over the 24h window.
func (rg *RiskGuard) Drawdown24h() float64 {
	rg.mu.RLock()
	defer rg.mu.RUnlock()

	if rg.peakNAV <= 0 || len(rg.navHistory) == 0 {
		return 0
	}

	currentNAV := rg.navHistory[len(rg.navHistory)-1].NAV
	if currentNAV >= rg.peakNAV {
		return 0
	}

	return (rg.peakNAV - currentNAV) / rg.peakNAV
}

// RecordFill records a fill event for rate limiting
func (rg *RiskGuard) RecordFill(ts time.Time) {
	rg.mu.Lock()
	defer rg.mu.Unlock()

	rg.fillTimestamps = append(rg.fillTimestamps, ts)
	rg.lastFillAt = ts

	// Trim to 5 min window
	cutoff := ts.Add(-5 * time.Minute)
	for len(rg.fillTimestamps) > 0 && rg.fillTimestamps[0].Before(cutoff) {
		rg.fillTimestamps = rg.fillTimestamps[1:]
	}
}

// FillsPerMin returns the current fill rate (fills in last minute)
func (rg *RiskGuard) FillsPerMin() float64 {
	rg.mu.RLock()
	defer rg.mu.RUnlock()

	now := time.Now()
	cutoff := now.Add(-1 * time.Minute)
	count := 0
	for i := len(rg.fillTimestamps) - 1; i >= 0; i-- {
		if rg.fillTimestamps[i].Before(cutoff) {
			break
		}
		count++
	}
	return float64(count)
}

// IsInFillCooldown returns true if a fill happened recently
func (rg *RiskGuard) IsInFillCooldown() bool {
	rg.mu.RLock()
	defer rg.mu.RUnlock()

	if rg.lastFillAt.IsZero() {
		return false
	}
	elapsed := time.Since(rg.lastFillAt)
	return elapsed < time.Duration(rg.cfg.CooldownAfterFillMs)*time.Millisecond
}

// IsFillRateExceeded returns true if fill rate exceeds the configured limit
func (rg *RiskGuard) IsFillRateExceeded() bool {
	return rg.FillsPerMin() >= rg.cfg.MaxFillsPerMin
}

// ShouldPause returns true if drawdown exceeds the limit
func (rg *RiskGuard) ShouldPause() bool {
	dd := rg.Drawdown24h()
	return dd >= rg.cfg.DrawdownLimitPct
}

// ShouldWarn returns true if drawdown exceeds the warning threshold
func (rg *RiskGuard) ShouldWarn() bool {
	dd := rg.Drawdown24h()
	return dd >= rg.cfg.DrawdownWarnPct
}

// EnterDefensive records the time when defensive mode was entered
func (rg *RiskGuard) EnterDefensive() {
	rg.mu.Lock()
	defer rg.mu.Unlock()
	rg.defensiveEnteredAt = time.Now()
}

// IsDefensiveCooldownExpired returns true if the defensive cooldown has elapsed
func (rg *RiskGuard) IsDefensiveCooldownExpired() bool {
	rg.mu.RLock()
	defer rg.mu.RUnlock()

	if rg.defensiveEnteredAt.IsZero() {
		return true
	}
	elapsed := time.Since(rg.defensiveEnteredAt)
	return elapsed >= time.Duration(rg.cfg.DefensiveCooldownSec)*time.Second
}

// DrawdownReduceMultiplier returns a size multiplier for "reduce" mode.
func (rg *RiskGuard) DrawdownReduceMultiplier() float64 {
	dd := rg.Drawdown24h()
	if dd < rg.cfg.DrawdownWarnPct {
		return 1.0
	}
	if dd >= rg.cfg.DrawdownLimitPct {
		return 0.1 // minimum 10% size
	}
	range_ := rg.cfg.DrawdownLimitPct - rg.cfg.DrawdownWarnPct
	if range_ <= 0 {
		return 0.1
	}
	progress := (dd - rg.cfg.DrawdownWarnPct) / range_
	return math.Max(0.1, 1.0-0.9*progress)
}
