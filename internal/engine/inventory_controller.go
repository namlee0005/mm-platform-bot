package engine

import "math"

// InventoryController computes skew_bps and size_tilt based on
// inventory ratio deviation from target.
type InventoryController struct {
	cfg      *InventoryConfig
	lastSkew float64
}

func NewInventoryController(cfg *InventoryConfig) *InventoryController {
	return &InventoryController{cfg: cfg}
}

// ComputeSkew returns the price skew in basis points.
//
// When invDev > 0 (too much base): skew > 0 → bids pushed far, asks pulled close
// When invDev < 0 (too much quote): skew < 0 → asks pushed far, bids pulled close
//
// Formula:
//
//	if |invDev| <= deadzone → 0
//	raw = K * invDev * 10000
//	clamp to [-maxSkewBps, maxSkewBps]
//	rate-limit change per tick
func (ic *InventoryController) ComputeSkew(invDev float64) float64 {
	if math.Abs(invDev) <= ic.cfg.Deadzone {
		// Decay toward zero smoothly
		if math.Abs(ic.lastSkew) > 0 {
			maxChange := float64(ic.cfg.DSkewMaxBpsPerTick)
			if ic.lastSkew > 0 {
				ic.lastSkew = math.Max(0, ic.lastSkew-maxChange)
			} else {
				ic.lastSkew = math.Min(0, ic.lastSkew+maxChange)
			}
		}
		return ic.lastSkew
	}

	rawSkew := ic.cfg.K * invDev * 10000.0

	// Clamp to max
	maxSkew := float64(ic.cfg.MaxSkewBps)
	skew := math.Max(-maxSkew, math.Min(rawSkew, maxSkew))

	// Rate-limit change per tick
	maxChange := float64(ic.cfg.DSkewMaxBpsPerTick)
	delta := skew - ic.lastSkew
	if math.Abs(delta) > maxChange {
		if delta > 0 {
			skew = ic.lastSkew + maxChange
		} else {
			skew = ic.lastSkew - maxChange
		}
	}

	ic.lastSkew = skew
	return skew
}

// ComputeSizeTilt returns a tilt ratio in [-cap, +cap].
// Positive tilt means increase ASK sizes (sell more) / decrease BID sizes.
// Negative tilt means increase BID sizes (buy more) / decrease ASK sizes.
//
// Only active in RECOVERY mode or when |invDev| > deadzone.
// size_tilt = clamp(invDev * tilt_factor, -cap, cap)
//
// Applied to orders:
//
//	bid_size *= (1 - size_tilt)  // reduce buying when holding too much base
//	ask_size *= (1 + size_tilt)  // increase selling when holding too much base
func (ic *InventoryController) ComputeSizeTilt(invDev float64, mode Mode) float64 {
	if mode != ModeRecovery {
		return 0
	}

	cap := ic.cfg.SizeTiltCap
	if cap <= 0 {
		return 0
	}

	// Linear scaling: at imbalance_threshold → full tilt
	threshold := ic.cfg.ImbalanceThreshold
	if threshold <= 0 {
		threshold = 0.2
	}

	tilt := invDev / threshold * cap
	return math.Max(-cap, math.Min(tilt, cap))
}

// ShouldEnterRecovery returns true if the inventory imbalance exceeds the threshold
func (ic *InventoryController) ShouldEnterRecovery(invDev float64) bool {
	return math.Abs(invDev) > ic.cfg.ImbalanceThreshold
}

// ShouldExitRecovery returns true if the inventory imbalance is within the recovery target
func (ic *InventoryController) ShouldExitRecovery(invDev float64) bool {
	return math.Abs(invDev) <= ic.cfg.RecoveryTarget
}

// GetLastSkew returns the last computed skew value
func (ic *InventoryController) GetLastSkew() float64 {
	return ic.lastSkew
}
