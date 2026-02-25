package modules

import (
	"math"

	"mm-platform-engine/internal/core"
)

// InventoryConfig contains configuration for inventory control
type InventoryConfig struct {
	TargetRatio        float64 `json:"target_ratio" bson:"target_ratio"`
	Deadzone           float64 `json:"deadzone" bson:"deadzone"`
	K                  float64 `json:"k" bson:"k"`
	MaxSkewBps         int     `json:"max_skew_bps" bson:"max_skew_bps"`
	MinOffsetBps       int     `json:"min_offset_bps" bson:"min_offset_bps"`
	DSkewMaxBpsPerTick int     `json:"d_skew_max_bps_per_tick" bson:"d_skew_max_bps_per_tick"`
	SizeTiltCap        float64 `json:"size_tilt_cap" bson:"size_tilt_cap"`
	ImbalanceThreshold float64 `json:"imbalance_threshold" bson:"imbalance_threshold"`
	RecoveryTarget     float64 `json:"recovery_target" bson:"recovery_target"`
}

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
// Only active in RECOVERY mode.
func (ic *InventoryController) ComputeSizeTilt(invDev float64, mode core.Mode) float64 {
	if mode != core.ModeRecovery {
		return 0
	}

	cap := ic.cfg.SizeTiltCap
	if cap <= 0 {
		return 0
	}

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

// GetTargetRatio returns the target inventory ratio
func (ic *InventoryController) GetTargetRatio() float64 {
	return ic.cfg.TargetRatio
}
