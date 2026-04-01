package strategy

import (
	"github.com/shopspring/decimal"
)

// CalculateInventorySkew returns the skew amount in ticks based on current inventory.
// A positive skew means we are LONG and should shift quotes DOWN (lower bids to buy less, lower asks to sell more).
// A negative skew means we are SHORT and should shift quotes UP.
func CalculateInventorySkew(netPosition decimal.Decimal, cfg InventoryConfig) float64 {
	if cfg.MaxInventory.IsZero() {
		return 0.0
	}

	// inventory_ratio = net_position / max_inventory   ∈ [-1.0, +1.0]
	// clamp the ratio between -1 and 1
	ratio := netPosition.Div(cfg.MaxInventory).InexactFloat64()
	if ratio > 1.0 {
		ratio = 1.0
	} else if ratio < -1.0 {
		ratio = -1.0
	}

	return ratio * float64(cfg.MaxSkewTicks)
}

// ShouldEmergencyCancel returns true if the position exceeds the emergency cap on a specific side.
func ShouldEmergencyCancel(netPosition decimal.Decimal, cfg InventoryConfig, isBid bool) bool {
	if !cfg.EmergencySideCancel || cfg.EmergencyCap.IsZero() {
		return false
	}

	// If Long beyond cap -> cancel ALL bids
	if netPosition.GreaterThanOrEqual(cfg.EmergencyCap) && isBid {
		return true
	}

	// If Short beyond cap -> cancel ALL asks
	if netPosition.LessThanOrEqual(cfg.EmergencyCap.Neg()) && !isBid {
		return true
	}

	return false
}
