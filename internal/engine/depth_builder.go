package engine

import (
	"fmt"
	"math"
	"math/rand"

	"mm-platform-engine/internal/utils"
)

// DepthBuilder constructs the order ladder with anti-abuse protections.
type DepthBuilder struct {
	cfg *Config

	// Sticky pricing state - prevents unnecessary order churn
	lastMid         float64
	lastMode        Mode
	lastBatchID     string
	cachedPrices    map[string]float64 // key: "BUY_0" or "SELL_1", value: cached price
	cachedQtys      map[string]float64 // key: same, value: cached qty base (before jitter)
	stickyThreshBps float64            // threshold to regenerate prices (default 30 bps)
}

func NewDepthBuilder(cfg *Config) *DepthBuilder {
	return &DepthBuilder{
		cfg:             cfg,
		cachedPrices:    make(map[string]float64),
		cachedQtys:      make(map[string]float64),
		stickyThreshBps: 30.0, // regenerate when mid moves >30 bps
	}
}

// BuildLadder generates desired orders for both sides.
//
// Algorithm:
//
//	price_i = mid * (1 ± distance_i) + skew_offset
//	size_i  = (quote_per_order * size_mult[i] / price_i) * (1 ± size_tilt)
//	+ random jitter on price (micro_offset + price_jitter_ticks)
//	+ random jitter on qty (qty_jitter_pct)
//
// In RECOVERY mode with prefer_one_sided=true:
//   - If invDev > 0 (too much base): only quote asks (sell to rebalance)
//   - If invDev < 0 (too much quote): only quote bids (buy to rebalance)
//   - The disfavored side is still quoted but at wider offsets and smaller sizes
func (db *DepthBuilder) BuildLadder(
	snap *Snapshot,
	spreadBps float64,
	skewBps float64,
	sizeTilt float64,
	mode Mode,
	invDev float64,
	timestamp int64,
) []DesiredOrder {
	mid := snap.Mid
	if mid <= 0 {
		return nil
	}

	// Check if we should regenerate jitter (sticky pricing)
	// Regenerate when: first run, mid moves >30 bps, or mode changes
	regenerate := false
	if db.lastMid <= 0 {
		regenerate = true
	} else {
		midMoveBps := math.Abs(mid-db.lastMid) / db.lastMid * 10000.0
		if midMoveBps > db.stickyThreshBps {
			regenerate = true
		}
	}
	if db.lastMode != mode {
		regenerate = true // mode change → resize orders
	}

	if regenerate {
		db.lastMid = mid
		db.lastMode = mode
		db.lastBatchID = fmt.Sprintf("%d_%04d", timestamp, rand.Intn(10000))
		db.cachedPrices = make(map[string]float64)
		db.cachedQtys = make(map[string]float64)
	}

	offsets := db.cfg.Depth.OffsetsBps
	sizeMults := db.cfg.Depth.SizeMult
	nLevels := len(offsets)
	if nLevels == 0 {
		return nil
	}
	if len(sizeMults) < nLevels {
		// Pad with zeros
		for len(sizeMults) < nLevels {
			sizeMults = append(sizeMults, 0)
		}
	}

	// Mode-based size multiplier
	modeSizeMult := 1.0
	switch mode {
	case ModeDefensive:
		modeSizeMult = db.cfg.ModeOverrides.Defensive.SizeMult
	case ModeRecovery:
		modeSizeMult = db.cfg.ModeOverrides.Recovery.SizeMult
	}

	orders := make([]DesiredOrder, 0, nLevels*2)
	batchID := db.lastBatchID
	if batchID == "" {
		batchID = fmt.Sprintf("%d_%04d", timestamp, rand.Intn(10000))
	}

	// Determine one-sided preference in RECOVERY
	preferOneSided := mode == ModeRecovery && db.cfg.ModeOverrides.Recovery.PreferOneSided
	// If invDev > 0 → favor sells; if < 0 → favor buys
	favorSells := invDev > 0

	for i := 0; i < nLevels; i++ {
		baseOffsetBps := float64(offsets[i])
		baseSizeMult := sizeMults[i] * modeSizeMult

		if baseSizeMult <= 0 {
			continue
		}

		// --- BID side ---
		bidSkipOneSided := preferOneSided && favorSells
		bidOffsetBps := baseOffsetBps + skewBps // pushed far when skew > 0
		bidSizeFactor := 1.0

		if bidSkipOneSided {
			// In recovery (too much base): still quote bids but reduced
			bidOffsetBps *= 1.5 // push further away
			bidSizeFactor = 0.3 // reduce size to 30%
		}

		bidOffsetBps = math.Max(bidOffsetBps, float64(db.cfg.Inventory.MinOffsetBps))
		bidOffsetBps = math.Max(bidOffsetBps, spreadBps) // never tighter than spread

		// Apply size tilt: negative tilt → increase bids
		bidTiltMult := 1.0 - sizeTilt // sizeTilt > 0 when too much base → reduce bids
		bidTiltMult = math.Max(0.1, math.Min(bidTiltMult, 1.5))

		bidKey := fmt.Sprintf("BUY_%d", i)
		var bidPrice float64
		if cached, ok := db.cachedPrices[bidKey]; ok && !regenerate {
			bidPrice = cached
		} else {
			bidPrice = db.applyPriceJitter(
				utils.RoundDown(mid*(1.0-bidOffsetBps/10000.0), snap.TickSize),
				snap.TickSize,
				-1, // bids jitter downward
			)
			db.cachedPrices[bidKey] = bidPrice
		}

		bidBaseQty := db.cfg.Depth.QuotePerOrder * baseSizeMult * bidSizeFactor * bidTiltMult
		var bidQty float64
		if cached, ok := db.cachedQtys[bidKey]; ok && !regenerate {
			bidQty = cached
		} else {
			bidQty = db.applyQtyJitter(
				utils.FloorToStep(bidBaseQty/bidPrice, snap.StepSize),
			)
			db.cachedQtys[bidKey] = bidQty
		}

		if bidPrice > 0 && bidQty > 0 && bidPrice*bidQty >= snap.MinNotional {
			orders = append(orders, DesiredOrder{
				Side:       "BUY",
				Price:      bidPrice,
				Qty:        bidQty,
				LevelIndex: i,
				Tag:        fmt.Sprintf("MM_%s_L%d_BID", batchID, i),
			})
		}

		// --- ASK side ---
		askSkipOneSided := preferOneSided && !favorSells
		askOffsetBps := baseOffsetBps - skewBps // pulled close when skew > 0
		askSizeFactor := 1.0

		if askSkipOneSided {
			askOffsetBps *= 1.5
			askSizeFactor = 0.3
		}

		askOffsetBps = math.Max(askOffsetBps, float64(db.cfg.Inventory.MinOffsetBps))
		askOffsetBps = math.Max(askOffsetBps, spreadBps)

		// Apply size tilt: positive tilt → increase asks
		askTiltMult := 1.0 + sizeTilt
		askTiltMult = math.Max(0.1, math.Min(askTiltMult, 1.5))

		askKey := fmt.Sprintf("SELL_%d", i)
		var askPrice float64
		if cached, ok := db.cachedPrices[askKey]; ok && !regenerate {
			askPrice = cached
		} else {
			askPrice = db.applyPriceJitter(
				utils.RoundUp(mid*(1.0+askOffsetBps/10000.0), snap.TickSize),
				snap.TickSize,
				1, // asks jitter upward
			)
			db.cachedPrices[askKey] = askPrice
		}

		askBaseQty := db.cfg.Depth.QuotePerOrder * baseSizeMult * askSizeFactor * askTiltMult
		var askQty float64
		if cached, ok := db.cachedQtys[askKey]; ok && !regenerate {
			askQty = cached
		} else {
			askQty = db.applyQtyJitter(
				utils.FloorToStep(askBaseQty/askPrice, snap.StepSize),
			)
			db.cachedQtys[askKey] = askQty
		}

		if askPrice > 0 && askQty > 0 && askPrice*askQty >= snap.MinNotional {
			orders = append(orders, DesiredOrder{
				Side:       "SELL",
				Price:      askPrice,
				Qty:        askQty,
				LevelIndex: i,
				Tag:        fmt.Sprintf("MM_%s_L%d_ASK", batchID, i),
			})
		}

		// Ensure bid < ask at this level
		if len(orders) >= 2 {
			lastBid := &orders[len(orders)-2]
			lastAsk := &orders[len(orders)-1]
			if lastBid.Side == "BUY" && lastAsk.Side == "SELL" && lastBid.Price >= lastAsk.Price {
				lastBid.Price = lastAsk.Price - snap.TickSize
				if lastBid.Price <= 0 {
					// Remove both
					orders = orders[:len(orders)-2]
				}
			}
		}
	}

	return orders
}

// EnforceDepthKPI adjusts the ladder to meet depth notional and order count targets.
// If current depth is below target, it adds more levels or increases sizes.
func (db *DepthBuilder) EnforceDepthKPI(orders []DesiredOrder, snap *Snapshot) []DesiredOrder {
	targetNotional := db.cfg.Depth.TargetDepthNotional
	minOrders := db.cfg.Depth.MinOrdersPerSide
	if targetNotional <= 0 && minOrders <= 0 {
		return orders
	}

	// Count current depth within ±2% of mid
	depthRange := snap.Mid * 0.02
	var bidNotional, askNotional float64
	var bidCount, askCount int

	for _, o := range orders {
		if o.Side == "BUY" && o.Price >= snap.Mid-depthRange {
			bidNotional += o.Price * o.Qty
			bidCount++
		}
		if o.Side == "SELL" && o.Price <= snap.Mid+depthRange {
			askNotional += o.Price * o.Qty
			askCount++
		}
	}

	// If depth target is unmet, scale up existing order sizes proportionally
	// (Don't add new levels - that changes the risk profile)
	halfTarget := targetNotional / 2.0
	if halfTarget > 0 {
		if bidNotional > 0 && bidNotional < halfTarget {
			scaleFactor := halfTarget / bidNotional
			if scaleFactor > 3.0 {
				scaleFactor = 3.0 // cap scaling to avoid huge orders
			}
			for i := range orders {
				if orders[i].Side == "BUY" {
					orders[i].Qty *= scaleFactor
				}
			}
		}
		if askNotional > 0 && askNotional < halfTarget {
			scaleFactor := halfTarget / askNotional
			if scaleFactor > 3.0 {
				scaleFactor = 3.0
			}
			for i := range orders {
				if orders[i].Side == "SELL" {
					orders[i].Qty *= scaleFactor
				}
			}
		}
	}

	return orders
}

// applyPriceJitter adds random tick-level jitter + micro-offset to avoid
// always quoting at the exact best bid/ask.
func (db *DepthBuilder) applyPriceJitter(price, tickSize float64, direction int) float64 {
	jitterTicks := rand.Intn(db.cfg.AntiAbuse.PriceJitterTicks + 1)
	microTicks := rand.Intn(db.cfg.AntiAbuse.MicroOffsetTicks + 1)
	totalTicks := jitterTicks + microTicks

	offset := float64(totalTicks) * tickSize * float64(direction)
	return price + offset
}

// applyQtyJitter adds random percentage jitter to order quantities
func (db *DepthBuilder) applyQtyJitter(qty float64) float64 {
	if db.cfg.AntiAbuse.QtyJitterPct <= 0 {
		return qty
	}
	jitter := 1.0 - db.cfg.AntiAbuse.QtyJitterPct + rand.Float64()*2.0*db.cfg.AntiAbuse.QtyJitterPct
	return qty * jitter
}

// ApplyBudgetConstraints ensures orders fit within available balances.
// If not, first tries proportional scaling; if scale < 0.5, drops far levels.
func (db *DepthBuilder) ApplyBudgetConstraints(orders []DesiredOrder, baseFree, quoteFree float64) []DesiredOrder {
	var requiredBase, requiredQuote float64
	for _, o := range orders {
		if o.Side == "BUY" {
			requiredQuote += o.Price * o.Qty
		} else {
			requiredBase += o.Qty
		}
	}

	if requiredBase <= baseFree && requiredQuote <= quoteFree {
		return orders
	}

	baseScale := 1.0
	quoteScale := 1.0
	if requiredBase > baseFree && requiredBase > 1e-10 {
		baseScale = baseFree / requiredBase
	}
	if requiredQuote > quoteFree && requiredQuote > 1e-10 {
		quoteScale = quoteFree / requiredQuote
	}
	scale := math.Min(baseScale, quoteScale)

	if scale > 0.5 {
		// Proportional scaling
		for i := range orders {
			orders[i].Qty *= scale
		}
		return orders
	}

	// Drop far levels until budget fits
	maxLevel := 0
	for _, o := range orders {
		if o.LevelIndex > maxLevel {
			maxLevel = o.LevelIndex
		}
	}

	for drop := maxLevel; drop >= 0; drop-- {
		var rb, rq float64
		filtered := make([]DesiredOrder, 0, len(orders))
		for _, o := range orders {
			if o.LevelIndex < drop {
				filtered = append(filtered, o)
				if o.Side == "BUY" {
					rq += o.Price * o.Qty
				} else {
					rb += o.Qty
				}
			}
		}
		if rb <= baseFree && rq <= quoteFree {
			return filtered
		}
	}

	return nil
}
