package mas

import (
	"github.com/shopspring/decimal"
)

// ComputeInventoryRatio returns net_position / max_inventory clamped to [-1.0, +1.0].
// max_inventory = MaxInventoryPct * initialBalance.
// Returns 0 if config or balance prevent computation (safe default: no skew).
func ComputeInventoryRatio(netPosition decimal.Decimal, cfg InventoryConfig, initialBalance decimal.Decimal) float64 {
	if cfg.MaxInventoryPct.IsZero() || initialBalance.IsZero() {
		return 0.0
	}
	maxInventory := cfg.MaxInventoryPct.Mul(initialBalance)
	if maxInventory.IsZero() {
		return 0.0
	}
	ratio, _ := netPosition.Div(maxInventory).Float64()
	if ratio > 1.0 {
		ratio = 1.0
	} else if ratio < -1.0 {
		ratio = -1.0
	}
	return ratio
}

// ApplyInventorySkew shifts all order prices by -(ratio * MaxSkewTicks * tickSize).
// Long inventory (ratio > 0): shift DOWN — lower bids (buy less) and asks (sell more).
// Short inventory (ratio < 0): shift UP — raise bids (buy more) and asks (sell less).
// Operates in-place on the slice for efficiency; the slice is already local per tick.
func ApplyInventorySkew(orders []Order, ratio float64, cfg InventoryConfig, tickSize decimal.Decimal) []Order {
	if ratio == 0 || cfg.MaxSkewTicks == 0 || tickSize.IsZero() {
		return orders
	}
	skewTicks := ratio * float64(cfg.MaxSkewTicks)
	skewAmount := tickSize.Mul(decimal.NewFromFloat(skewTicks))
	for i := range orders {
		orders[i].Price = orders[i].Price.Sub(skewAmount)
	}
	return orders
}

// CheckEmergencyCap returns whether to cancel bids and/or asks due to the hard-stop cap.
// emergency_cap = EmergencyCapPct * initialBalance.
// Net long ≥ cap → cancel bids (stop buying more).
// Net short ≤ -cap → cancel asks (stop selling more).
func CheckEmergencyCap(netPosition decimal.Decimal, cfg InventoryConfig, initialBalance decimal.Decimal) (cancelBids, cancelAsks bool) {
	if !cfg.EmergencySideCancel || cfg.EmergencyCapPct.IsZero() || initialBalance.IsZero() {
		return false, false
	}
	emergencyCap := cfg.EmergencyCapPct.Mul(initialBalance)
	if netPosition.GreaterThanOrEqual(emergencyCap) {
		cancelBids = true
	}
	if netPosition.LessThanOrEqual(emergencyCap.Neg()) {
		cancelAsks = true
	}
	return
}
