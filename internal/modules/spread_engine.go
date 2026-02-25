package modules

import (
	"math"

	"mm-platform-engine/internal/core"
)

// SpreadConfig contains configuration for spread calculation
type SpreadConfig struct {
	BaseSpreadBps    float64 `json:"base_spread_bps" bson:"base_spread_bps"`
	MinSpreadBps     float64 `json:"min_spread_bps" bson:"min_spread_bps"`
	MaxSpreadBps     float64 `json:"max_spread_bps" bson:"max_spread_bps"`
	VolMultiplierCap float64 `json:"vol_multiplier_cap" bson:"vol_multiplier_cap"`
}

// ModeMultipliers contains spread/size multipliers for a mode
type ModeMultipliers struct {
	SpreadMult  float64 `json:"spread_mult" bson:"spread_mult"`
	SizeMult    float64 `json:"size_mult" bson:"size_mult"`
	RefreshMult float64 `json:"refresh_mult" bson:"refresh_mult"`
}

// SpreadEngine computes the effective spread based on base spread,
// volatility, inventory deviation, and current mode.
type SpreadEngine struct {
	spreadCfg SpreadConfig
	invCfg    *InventoryConfig

	defensiveMult ModeMultipliers
	recoveryMult  ModeMultipliers
}

// NewSpreadEngine creates a new spread engine
func NewSpreadEngine(
	spreadCfg SpreadConfig,
	invCfg *InventoryConfig,
	defensiveMult, recoveryMult ModeMultipliers,
) *SpreadEngine {
	return &SpreadEngine{
		spreadCfg:     spreadCfg,
		invCfg:        invCfg,
		defensiveMult: defensiveMult,
		recoveryMult:  recoveryMult,
	}
}

// ComputeSpread returns the effective half-spread in bps.
// target_spread = base_spread * vol_mult * inv_mult * mode_mult
// Clamped to [min_spread, max_spread].
func (se *SpreadEngine) ComputeSpread(volMult float64, invDev float64, mode core.Mode) float64 {
	base := se.spreadCfg.BaseSpreadBps

	// Volatility multiplier (already computed by PriceEngine)
	vm := volMult
	if vm < 1.0 {
		vm = 1.0
	}
	if vm > se.spreadCfg.VolMultiplierCap {
		vm = se.spreadCfg.VolMultiplierCap
	}

	// Inventory multiplier: widen spread when heavily imbalanced
	im := 1.0
	if se.invCfg != nil {
		absInvDev := math.Abs(invDev)
		if absInvDev > se.invCfg.Deadzone {
			excessDev := absInvDev - se.invCfg.Deadzone
			im = 1.0 + excessDev*2.0
			if im > 2.0 {
				im = 2.0
			}
		}
	}

	// Mode multiplier
	mm := 1.0
	switch mode {
	case core.ModeDefensive:
		mm = se.defensiveMult.SpreadMult
	case core.ModeRecovery:
		mm = se.recoveryMult.SpreadMult
	}

	spread := base * vm * im * mm

	// Clamp
	if spread < se.spreadCfg.MinSpreadBps {
		spread = se.spreadCfg.MinSpreadBps
	}
	if spread > se.spreadCfg.MaxSpreadBps {
		spread = se.spreadCfg.MaxSpreadBps
	}

	return spread
}
