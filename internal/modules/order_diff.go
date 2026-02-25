package modules

import (
	"math"
	"time"

	"mm-platform-engine/internal/core"
)

// ExecutionConfig contains configuration for execution engine
type ExecutionConfig struct {
	RefreshBaseSec        int     `json:"refresh_base_sec" bson:"refresh_base_sec"`
	RefreshJitterPct      float64 `json:"refresh_jitter_pct" bson:"refresh_jitter_pct"`
	RepriceThresholdBps   int     `json:"reprice_threshold_bps" bson:"reprice_threshold_bps"`
	InvDevThreshold       float64 `json:"inv_dev_threshold" bson:"inv_dev_threshold"`
	MaxOrderAgeSec        int     `json:"max_order_age_sec" bson:"max_order_age_sec"`
	PriceToleranceBps     int     `json:"price_tolerance_bps" bson:"price_tolerance_bps"`
	QtyTolerancePct       float64 `json:"qty_tolerance_pct" bson:"qty_tolerance_pct"`
	TickIntervalMs        int     `json:"tick_interval_ms" bson:"tick_interval_ms"`
	RateLimitOrdersPerSec int     `json:"rate_limit_orders_per_sec" bson:"rate_limit_orders_per_sec"`
	BatchSize             int     `json:"batch_size" bson:"batch_size"`
	MinOrderLifetimeMs    int     `json:"min_order_lifetime_ms" bson:"min_order_lifetime_ms"`
}

// OrderDiffEngine computes the minimal set of place/amend/cancel operations
// to move from the current live orders to the desired orders.
type OrderDiffEngine struct {
	priceToleranceBps float64
	qtyTolerancePct   float64
	minOrderLifetime  time.Duration
}

func NewOrderDiffEngine(cfg *ExecutionConfig) *OrderDiffEngine {
	return &OrderDiffEngine{
		priceToleranceBps: float64(cfg.PriceToleranceBps),
		qtyTolerancePct:   cfg.QtyTolerancePct,
		minOrderLifetime:  time.Duration(cfg.MinOrderLifetimeMs) * time.Millisecond,
	}
}

// NewOrderDiffEngineSimple creates an OrderDiffEngine with simple parameters
func NewOrderDiffEngineSimple(priceToleranceBps float64, qtyTolerancePct float64, minLifetimeMs int) *OrderDiffEngine {
	return &OrderDiffEngine{
		priceToleranceBps: priceToleranceBps,
		qtyTolerancePct:   qtyTolerancePct,
		minOrderLifetime:  time.Duration(minLifetimeMs) * time.Millisecond,
	}
}

// Diff computes the minimal order diff.
func (de *OrderDiffEngine) Diff(desired []core.DesiredOrder, live []core.LiveOrder) []core.OrderDiff {
	diffs := make([]core.OrderDiff, 0, len(desired)+len(live))

	// Index live orders by (side, level)
	type key struct {
		side  string
		level int
	}
	liveMap := make(map[key]*core.LiveOrder, len(live))
	liveMatched := make(map[string]bool, len(live))

	for i := range live {
		k := key{side: live[i].Side, level: live[i].LevelIndex}
		if existing, ok := liveMap[k]; ok {
			if live[i].PlacedAt.After(existing.PlacedAt) {
				diffs = append(diffs, core.OrderDiff{
					Action: core.DiffCancel,
					Live:   existing,
					Reason: "duplicate_level_older",
				})
				liveMatched[existing.OrderID] = true
				liveMap[k] = &live[i]
			} else {
				diffs = append(diffs, core.OrderDiff{
					Action: core.DiffCancel,
					Live:   &live[i],
					Reason: "duplicate_level_older",
				})
				liveMatched[live[i].OrderID] = true
			}
		} else {
			liveMap[k] = &live[i]
		}
	}

	// Match desired to live
	for i := range desired {
		d := &desired[i]
		k := key{side: d.Side, level: d.LevelIndex}

		if l, ok := liveMap[k]; ok {
			liveMatched[l.OrderID] = true

			if de.withinTolerance(d, l) {
				continue // keep as-is
			}

			if time.Since(l.PlacedAt) < de.minOrderLifetime {
				continue // too young to amend
			}

			reason := de.amendReason(d, l)

			diffs = append(diffs, core.OrderDiff{
				Action:  core.DiffAmend,
				Desired: d,
				Live:    l,
				Reason:  reason,
			})
		} else {
			diffs = append(diffs, core.OrderDiff{
				Action:  core.DiffPlace,
				Desired: d,
				Reason:  "new_level",
			})
		}
	}

	// Cancel unmatched live orders
	for i := range live {
		if !liveMatched[live[i].OrderID] {
			if time.Since(live[i].PlacedAt) < de.minOrderLifetime {
				continue
			}
			diffs = append(diffs, core.OrderDiff{
				Action: core.DiffCancel,
				Live:   &live[i],
				Reason: "no_matching_desired",
			})
		}
	}

	return diffs
}

func (de *OrderDiffEngine) amendReason(d *core.DesiredOrder, l *core.LiveOrder) string {
	reasons := []string{}

	if d.Price > 0 {
		priceDiffBps := math.Abs(d.Price-l.Price) / d.Price * 10000.0
		if priceDiffBps > de.priceToleranceBps {
			reasons = append(reasons, "price_diff")
		}
	}

	effectiveQty := l.RemainingQty
	if effectiveQty <= 0 {
		effectiveQty = l.Qty
	}
	if d.Qty > 0 {
		qtyDiffPct := math.Abs(d.Qty-effectiveQty) / d.Qty
		if qtyDiffPct > de.qtyTolerancePct {
			reasons = append(reasons, "qty_diff")
		}
	}

	if len(reasons) == 0 {
		return "unknown"
	}
	result := reasons[0]
	for i := 1; i < len(reasons); i++ {
		result += "+" + reasons[i]
	}
	return result
}

func (de *OrderDiffEngine) withinTolerance(d *core.DesiredOrder, l *core.LiveOrder) bool {
	if d.Price > 0 {
		priceDiffBps := math.Abs(d.Price-l.Price) / d.Price * 10000.0
		if priceDiffBps > de.priceToleranceBps {
			return false
		}
	}

	effectiveQty := l.RemainingQty
	if effectiveQty <= 0 {
		effectiveQty = l.Qty
	}
	if d.Qty > 0 {
		qtyDiffPct := math.Abs(d.Qty-effectiveQty) / d.Qty
		if qtyDiffPct > de.qtyTolerancePct {
			return false
		}
	}

	return true
}

// CountByAction counts diff operations by type
func CountByAction(diffs []core.OrderDiff) (places, amends, cancels int) {
	for _, d := range diffs {
		switch d.Action {
		case core.DiffPlace:
			places++
		case core.DiffAmend:
			amends++
		case core.DiffCancel:
			cancels++
		}
	}
	return
}
