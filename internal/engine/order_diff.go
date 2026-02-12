package engine

import (
	"math"
	"time"
)

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
		minOrderLifetime:  time.Duration(cfg.RateLimitOrdersPerSec) * time.Millisecond,
	}
}

// Diff computes the minimal order diff.
//
// Matching logic:
//  1. For each desired order, find a live order on the same side & level.
//  2. If found and price+qty within tolerance → keep (no action).
//  3. If found but price or qty outside tolerance → amend.
//  4. If no match → place new.
//  5. Any live order with no matching desired order → cancel.
//
// Respects min order lifetime: won't cancel/amend orders younger than threshold.
func (de *OrderDiffEngine) Diff(desired []DesiredOrder, live []LiveOrder) []OrderDiff {
	diffs := make([]OrderDiff, 0, len(desired)+len(live))

	// Index live orders by (side, level)
	type key struct {
		side  string
		level int
	}
	liveMap := make(map[key]*LiveOrder, len(live))
	liveMatched := make(map[string]bool, len(live))

	for i := range live {
		k := key{side: live[i].Side, level: live[i].LevelIndex}
		// If multiple live orders at same (side, level), keep the newest
		if existing, ok := liveMap[k]; ok {
			if live[i].PlacedAt.After(existing.PlacedAt) {
				// Cancel the older one
				diffs = append(diffs, OrderDiff{
					Action: DiffCancel,
					Live:   existing,
					Reason: "duplicate_level_older",
				})
				liveMatched[existing.OrderID] = true
				liveMap[k] = &live[i]
			} else {
				diffs = append(diffs, OrderDiff{
					Action: DiffCancel,
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

			// Check if within tolerance
			if de.withinTolerance(d, l) {
				continue // keep as-is
			}

			// Check min lifetime
			if time.Since(l.PlacedAt) < de.minOrderLifetime {
				continue // too young to amend, skip this tick
			}

			// Determine amend reason
			reason := de.amendReason(d, l)

			// Amend
			diffs = append(diffs, OrderDiff{
				Action:  DiffAmend,
				Desired: d,
				Live:    l,
				Reason:  reason,
			})
		} else {
			// Place new
			diffs = append(diffs, OrderDiff{
				Action:  DiffPlace,
				Desired: d,
				Reason:  "new_level",
			})
		}
	}

	// Cancel unmatched live orders
	for i := range live {
		if !liveMatched[live[i].OrderID] {
			// Check min lifetime before cancelling
			if time.Since(live[i].PlacedAt) < de.minOrderLifetime {
				continue
			}
			diffs = append(diffs, OrderDiff{
				Action: DiffCancel,
				Live:   &live[i],
				Reason: "no_matching_desired",
			})
		}
	}

	return diffs
}

// amendReason returns why an order needs to be amended
func (de *OrderDiffEngine) amendReason(d *DesiredOrder, l *LiveOrder) string {
	reasons := []string{}

	// Check price difference
	if d.Price > 0 {
		priceDiffBps := math.Abs(d.Price-l.Price) / d.Price * 10000.0
		if priceDiffBps > de.priceToleranceBps {
			reasons = append(reasons, "price_diff")
		}
	}

	// Check qty difference
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

// withinTolerance checks if a live order is close enough to the desired order
// that no amend is needed.
func (de *OrderDiffEngine) withinTolerance(d *DesiredOrder, l *LiveOrder) bool {
	// Price check
	if d.Price > 0 {
		priceDiffBps := math.Abs(d.Price-l.Price) / d.Price * 10000.0
		if priceDiffBps > de.priceToleranceBps {
			return false
		}
	}

	// Qty check (compare against remaining qty)
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
func CountByAction(diffs []OrderDiff) (places, amends, cancels int) {
	for _, d := range diffs {
		switch d.Action {
		case DiffPlace:
			places++
		case DiffAmend:
			amends++
		case DiffCancel:
			cancels++
		}
	}
	return
}
