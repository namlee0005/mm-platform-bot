package modules

import (
	"fmt"
	"math"
	"sync"
	"time"

	"mm-platform-engine/internal/core"
)

// ShockConfig contains configuration for shock detection
type ShockConfig struct {
	PriceMovePct       float64 `json:"price_move_pct" bson:"price_move_pct"`
	PriceMoveWindowSec int     `json:"price_move_window_sec" bson:"price_move_window_sec"`
	VolSpikeMultiplier float64 `json:"vol_spike_multiplier" bson:"vol_spike_multiplier"`
	SweepDepthPct      float64 `json:"sweep_depth_pct" bson:"sweep_depth_pct"`
	SweepMinDepth      float64 `json:"sweep_min_depth" bson:"sweep_min_depth"`
}

// PriceEngine computes mid, realized volatility, and shock detection.
type PriceEngine struct {
	mu  sync.RWMutex
	cfg *ShockConfig

	// Price history ring buffer for vol calculation
	prices    []pricePoint
	maxPoints int

	// EWM volatility state
	ewmVar    float64
	ewmAlpha  float64 // decay factor (e.g. 0.06 for ~30-sample half-life)
	ewmInited bool
	lastMid   float64

	// Shock detection: recent mid prices for window check
	recentMids []pricePoint

	// Startup grace period - don't detect shocks until we have enough data
	startTime time.Time
	tickCount int
}

type pricePoint struct {
	ts    time.Time
	price float64
}

func NewPriceEngine(cfg *ShockConfig) *PriceEngine {
	return &PriceEngine{
		cfg:        cfg,
		prices:     make([]pricePoint, 0, 512),
		maxPoints:  512,
		ewmAlpha:   0.06, // ~30-sample half-life at 5s tick → ~2.5 min effective
		recentMids: make([]pricePoint, 0, 128),
		startTime:  time.Now(),
		tickCount:  0,
	}
}

// Update records a new mid price observation
func (pe *PriceEngine) Update(mid float64, ts time.Time) {
	pe.mu.Lock()
	defer pe.mu.Unlock()

	pe.tickCount++

	pp := pricePoint{ts: ts, price: mid}

	// Append to ring buffers
	if len(pe.prices) >= pe.maxPoints {
		pe.prices = pe.prices[1:]
	}
	pe.prices = append(pe.prices, pp)

	if len(pe.recentMids) >= 128 {
		pe.recentMids = pe.recentMids[1:]
	}
	pe.recentMids = append(pe.recentMids, pp)

	// Update EWM variance
	if pe.ewmInited && pe.lastMid > 0 {
		ret := math.Log(mid / pe.lastMid)
		pe.ewmVar = (1-pe.ewmAlpha)*pe.ewmVar + pe.ewmAlpha*ret*ret
	} else {
		pe.ewmInited = true
	}
	pe.lastMid = mid
}

// RealizedVol returns the annualized realized volatility estimate.
func (pe *PriceEngine) RealizedVol() float64 {
	pe.mu.RLock()
	defer pe.mu.RUnlock()
	return math.Sqrt(pe.ewmVar)
}

// VolMultiplier returns a spread multiplier based on current vol vs baseline.
// Returns value in [1.0, cap].
func (pe *PriceEngine) VolMultiplier(cap float64) float64 {
	pe.mu.RLock()
	defer pe.mu.RUnlock()

	if !pe.ewmInited || pe.ewmVar <= 0 {
		return 1.0
	}

	currentVol := math.Sqrt(pe.ewmVar)

	// Compute baseline vol from full history
	if len(pe.prices) < 10 {
		return 1.0
	}
	var sumSq float64
	var count int
	for i := 1; i < len(pe.prices); i++ {
		if pe.prices[i-1].price <= 0 {
			continue
		}
		ret := math.Log(pe.prices[i].price / pe.prices[i-1].price)
		sumSq += ret * ret
		count++
	}
	if count == 0 {
		return 1.0
	}
	baselineVol := math.Sqrt(sumSq / float64(count))
	if baselineVol <= 1e-12 {
		return 1.0
	}

	mult := currentVol / baselineVol
	if mult < 1.0 {
		mult = 1.0
	}
	if mult > cap {
		mult = cap
	}
	return mult
}

// ShockInfo contains details about detected shock for monitoring
type ShockInfo struct {
	Detected     bool
	Reason       string  // "price_spike", "vol_spike", "sweep", or ""
	PriceMovePct float64 // actual price move %
	PriceThresh  float64 // threshold %
	VolRatio     float64 // currentVol / baselineVol
	VolThresh    float64 // threshold multiplier
	BidDepth     float64 // $ depth on bid side
	AskDepth     float64 // $ depth on ask side
	DepthThresh  float64 // minimum expected $
}

func (s ShockInfo) String() string {
	if !s.Detected {
		return "no_shock"
	}
	switch s.Reason {
	case "price_spike":
		return fmt.Sprintf("price_spike: %.2f%% > %.2f%% threshold", s.PriceMovePct, s.PriceThresh)
	case "vol_spike":
		return fmt.Sprintf("vol_spike: %.2fx > %.2fx threshold", s.VolRatio, s.VolThresh)
	case "sweep":
		return fmt.Sprintf("sweep: bid=$%.2f ask=$%.2f < $%.2f threshold", s.BidDepth, s.AskDepth, s.DepthThresh)
	default:
		return s.Reason
	}
}

// DetectShock checks for price shocks
func (pe *PriceEngine) DetectShock() ShockInfo {
	pe.mu.RLock()
	defer pe.mu.RUnlock()

	info := ShockInfo{
		PriceThresh: pe.cfg.PriceMovePct,
		VolThresh:   pe.cfg.VolSpikeMultiplier,
	}

	// Skip shock detection during startup grace period (first 3 ticks)
	if pe.tickCount < 3 {
		return info
	}

	if len(pe.recentMids) < 2 {
		return info
	}

	now := pe.recentMids[len(pe.recentMids)-1]
	windowCutoff := now.ts.Add(-time.Duration(pe.cfg.PriceMoveWindowSec) * time.Second)

	// Check price move within window
	var maxMovePct float64
	for i := len(pe.recentMids) - 2; i >= 0; i-- {
		if pe.recentMids[i].ts.Before(windowCutoff) {
			break
		}
		if pe.recentMids[i].price <= 0 {
			continue
		}
		movePct := math.Abs(now.price-pe.recentMids[i].price) / pe.recentMids[i].price * 100.0
		if movePct > maxMovePct {
			maxMovePct = movePct
		}
	}
	info.PriceMovePct = maxMovePct
	if maxMovePct >= pe.cfg.PriceMovePct {
		info.Detected = true
		info.Reason = "price_spike"
		return info
	}

	// Check vol spike
	if pe.ewmInited {
		currentVol := math.Sqrt(pe.ewmVar)
		if len(pe.prices) >= 20 {
			var sumSq float64
			var count int
			for i := 1; i < len(pe.prices); i++ {
				if pe.prices[i-1].price <= 0 {
					continue
				}
				ret := math.Log(pe.prices[i].price / pe.prices[i-1].price)
				sumSq += ret * ret
				count++
			}
			if count > 0 {
				baseVol := math.Sqrt(sumSq / float64(count))
				if baseVol > 1e-12 {
					info.VolRatio = currentVol / baseVol
					if info.VolRatio >= pe.cfg.VolSpikeMultiplier {
						info.Detected = true
						info.Reason = "vol_spike"
						return info
					}
				}
			}
		}
	}

	return info
}

// DetectSweep checks if significant depth near mid was consumed.
func (pe *PriceEngine) DetectSweep(snap *core.Snapshot) ShockInfo {
	pe.mu.RLock()
	defer pe.mu.RUnlock()

	minExpected := pe.cfg.SweepMinDepth
	if minExpected <= 0 {
		minExpected = 10.0
	}

	info := ShockInfo{
		DepthThresh: minExpected,
	}

	// Skip sweep detection during startup grace period
	if pe.tickCount < 6 {
		return info
	}

	if snap.Mid <= 0 || pe.cfg.SweepDepthPct <= 0 {
		return info
	}

	if len(snap.Bids) == 0 && len(snap.Asks) == 0 {
		return info
	}

	sweepRange := snap.Mid * pe.cfg.SweepDepthPct / 100.0

	var bidDepth, askDepth float64
	for _, lvl := range snap.Bids {
		if lvl.Price >= snap.Mid-sweepRange {
			bidDepth += lvl.Qty * lvl.Price
		}
	}
	for _, lvl := range snap.Asks {
		if lvl.Price <= snap.Mid+sweepRange {
			askDepth += lvl.Qty * lvl.Price
		}
	}

	info.BidDepth = bidDepth
	info.AskDepth = askDepth

	if bidDepth < minExpected || askDepth < minExpected {
		info.Detected = true
		info.Reason = "sweep"
	}
	return info
}
