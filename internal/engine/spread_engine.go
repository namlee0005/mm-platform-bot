package engine

import "math"

// SpreadEngine computes the effective spread based on base spread,
// volatility, inventory deviation, and current mode.
type SpreadEngine struct {
	cfg *Config
}

func NewSpreadEngine(cfg *Config) *SpreadEngine {
	return &SpreadEngine{cfg: cfg}
}

// ComputeSpread returns the effective half-spread in bps.
// target_spread = base_spread * vol_mult * inv_mult * mode_mult
// Clamped to [min_spread, max_spread].
func (se *SpreadEngine) ComputeSpread(volMult float64, invDev float64, mode Mode) float64 {
	base := se.cfg.Spread.BaseSpreadBps

	// Volatility multiplier (already computed by PriceEngine)
	vm := volMult
	if vm < 1.0 {
		vm = 1.0
	}
	if vm > se.cfg.Spread.VolMultiplierCap {
		vm = se.cfg.Spread.VolMultiplierCap
	}

	// Inventory multiplier: widen spread when heavily imbalanced
	// Scale linearly: 1.0 at deadzone, up to 2.0 at max imbalance
	im := 1.0
	absInvDev := math.Abs(invDev)
	if absInvDev > se.cfg.Inventory.Deadzone {
		excessDev := absInvDev - se.cfg.Inventory.Deadzone
		// Linearly scale: at 50% imbalance → 2x spread
		im = 1.0 + excessDev*2.0
		if im > 2.0 {
			im = 2.0
		}
	}

	// Mode multiplier
	mm := 1.0
	switch mode {
	case ModeDefensive:
		mm = se.cfg.ModeOverrides.Defensive.SpreadMult
	case ModeRecovery:
		mm = se.cfg.ModeOverrides.Recovery.SpreadMult
	}

	spread := base * vm * im * mm

	// Clamp
	if spread < se.cfg.Spread.MinSpreadBps {
		spread = se.cfg.Spread.MinSpreadBps
	}
	if spread > se.cfg.Spread.MaxSpreadBps {
		spread = se.cfg.Spread.MaxSpreadBps
	}

	return spread
}
