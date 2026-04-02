package mas

import (
	"time"

	"mm-platform-engine/internal/core"
	"mm-platform-engine/pkg/exchange"
)

// ToxicityTracker monitors fills to detect adverse selection (informed flow).
type ToxicityTracker struct {
	fills      []core.FillEvent
	config     ToxicityConfig
	pauseUntil map[exchange.Side]time.Time
	counts     map[exchange.Side]int
}

// NewToxicityTracker initializes the tracker.
func NewToxicityTracker(cfg ToxicityConfig) *ToxicityTracker {
	return &ToxicityTracker{
		fills:      make([]core.FillEvent, 0),
		config:     cfg,
		pauseUntil: make(map[exchange.Side]time.Time),
		counts:     make(map[exchange.Side]int),
	}
}

// OnFill records a new fill.
func (t *ToxicityTracker) OnFill(fill *core.FillEvent) {
	t.fills = append(t.fills, *fill)
}

// CheckToxicity analyzes recent fills against current market price to detect adverse moves.
// Should be called on every tick with the current mid or micro price.
func (t *ToxicityTracker) CheckToxicity(currentPrice float64, now time.Time) {
	// Clean up old fills outside rolling window
	cutoff := now.Add(-t.config.RollingWindow)
	validFills := make([]core.FillEvent, 0)
	
	// Reset counts for this evaluation window
	t.counts[exchange.SideBuy] = 0
	t.counts[exchange.SideSell] = 0

	for _, f := range t.fills {
		if f.Timestamp.After(cutoff) {
			validFills = append(validFills, f)
			
			// Check if price moved against the fill
			// Toxic BUY fill: we bought, but price dropped significantly (Adverse Selection)
			// Toxic SELL fill: we sold, but price spiked significantly
			
			if f.Side == string(exchange.SideBuy) {
				// We bought at f.Price. If currentPrice < f.Price - threshold, it's toxic
				drop := f.Price - currentPrice
				// Approximation: 1 tick = price * 0.0001 (1 bps) for this simplistic check, 
				// real implementation should use actual TickSize. Assuming threshold is absolute for now.
				if drop > t.config.ToxicMoveTicks {
					t.counts[exchange.SideBuy]++
				}
			} else if f.Side == string(exchange.SideSell) {
				// We sold at f.Price. If currentPrice > f.Price + threshold, it's toxic
				spike := currentPrice - f.Price
				if spike > t.config.ToxicMoveTicks {
					t.counts[exchange.SideSell]++
				}
			}
		}
	}
	t.fills = validFills

	// Evaluate pauses based on tiers
	t.evaluateSide(exchange.SideBuy, now)
	t.evaluateSide(exchange.SideSell, now)
}

func (t *ToxicityTracker) evaluateSide(side exchange.Side, now time.Time) {
	count := t.counts[side]
	var pauseDuration time.Duration

	if count >= t.config.Tier3Count {
		pauseDuration = t.config.Tier3Pause
	} else if count >= t.config.Tier2Count {
		pauseDuration = t.config.Tier2Pause
	} else if count >= t.config.Tier1Count {
		pauseDuration = t.config.Tier1Pause
	}

	if pauseDuration > 0 {
		newPauseUntil := now.Add(pauseDuration)
		if currentPause, exists := t.pauseUntil[side]; !exists || newPauseUntil.After(currentPause) {
			t.pauseUntil[side] = newPauseUntil
		}
	}
}

// IsPaused returns true if the specified side is currently under a toxicity pause.
func (t *ToxicityTracker) IsPaused(side exchange.Side, now time.Time) bool {
	pauseEnd, exists := t.pauseUntil[side]
	if !exists {
		return false
	}
	return now.Before(pauseEnd)
}
