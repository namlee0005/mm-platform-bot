package modules

import (
	"fmt"
	"math"
	"math/rand"

	"mm-platform-engine/internal/core"
)

// DepthConfig contains configuration for depth building
type DepthConfig struct {
	NumLevels           int       `json:"num_levels" bson:"num_levels"`
	OffsetsBps          []int     `json:"offsets_bps" bson:"offsets_bps"`
	SizeMult            []float64 `json:"size_mult" bson:"size_mult"`
	QuotePerOrder       float64   `json:"quote_per_order" bson:"quote_per_order"`
	TargetDepthNotional float64   `json:"target_depth_notional" bson:"target_depth_notional"`
	MinOrdersPerSide    int       `json:"min_orders_per_side" bson:"min_orders_per_side"`
}

// AntiAbuseConfig contains configuration for anti-abuse measures
type AntiAbuseConfig struct {
	MicroOffsetTicks int     `json:"micro_offset_ticks" bson:"micro_offset_ticks"`
	QtyJitterPct     float64 `json:"qty_jitter_pct" bson:"qty_jitter_pct"`
	PriceJitterTicks int     `json:"price_jitter_ticks" bson:"price_jitter_ticks"`
	FillCooldownMs   int     `json:"fill_cooldown_ms" bson:"fill_cooldown_ms"`
	MaxFillsPerMin   int     `json:"max_fills_per_min" bson:"max_fills_per_min"`
}

// RecoveryConfig contains recovery mode configuration
type RecoveryConfig struct {
	SpreadMult     float64 `json:"spread_mult" bson:"spread_mult"`
	SizeMult       float64 `json:"size_mult" bson:"size_mult"`
	PreferOneSided bool    `json:"prefer_one_sided" bson:"prefer_one_sided"`
}

// DepthBuilder constructs the order ladder with anti-abuse protections.
type DepthBuilder struct {
	depthCfg  DepthConfig
	antiAbuse AntiAbuseConfig
	invCfg    *InventoryConfig

	// Mode overrides
	defensiveSizeMult float64
	recoverySizeMult  float64
	preferOneSided    bool

	// Sticky pricing state
	lastMid         float64
	lastMode        core.Mode
	lastBatchID     string
	cachedPrices    map[string]float64
	cachedQtys      map[string]float64
	stickyThreshBps float64
}

// NewDepthBuilder creates a new depth builder
func NewDepthBuilder(
	depthCfg DepthConfig,
	antiAbuse AntiAbuseConfig,
	invCfg *InventoryConfig,
	defensiveSizeMult, recoverySizeMult float64,
	preferOneSided bool,
) *DepthBuilder {
	return &DepthBuilder{
		depthCfg:          depthCfg,
		antiAbuse:         antiAbuse,
		invCfg:            invCfg,
		defensiveSizeMult: defensiveSizeMult,
		recoverySizeMult:  recoverySizeMult,
		preferOneSided:    preferOneSided,
		cachedPrices:      make(map[string]float64),
		cachedQtys:        make(map[string]float64),
		stickyThreshBps:   30.0,
	}
}

// BuildLadder generates desired orders for both sides.
func (db *DepthBuilder) BuildLadder(
	snap *core.Snapshot,
	spreadBps float64,
	skewBps float64,
	sizeTilt float64,
	mode core.Mode,
	invDev float64,
	timestamp int64,
) []core.DesiredOrder {
	mid := snap.Mid
	if mid <= 0 {
		return nil
	}

	// Check if we should regenerate jitter (sticky pricing)
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
		regenerate = true
	}

	if regenerate {
		db.lastMid = mid
		db.lastMode = mode
		db.lastBatchID = fmt.Sprintf("%d_%04d", timestamp, rand.Intn(10000))
		db.cachedPrices = make(map[string]float64)
		db.cachedQtys = make(map[string]float64)
	}

	offsets := db.depthCfg.OffsetsBps
	sizeMults := db.depthCfg.SizeMult
	nLevels := len(offsets)
	if nLevels == 0 {
		return nil
	}
	if len(sizeMults) < nLevels {
		for len(sizeMults) < nLevels {
			sizeMults = append(sizeMults, 0)
		}
	}

	// Mode-based size multiplier
	modeSizeMult := 1.0
	switch mode {
	case core.ModeDefensive:
		modeSizeMult = db.defensiveSizeMult
	case core.ModeRecovery:
		modeSizeMult = db.recoverySizeMult
	}

	orders := make([]core.DesiredOrder, 0, nLevels*2)
	batchID := db.lastBatchID
	if batchID == "" {
		batchID = fmt.Sprintf("%d_%04d", timestamp, rand.Intn(10000))
	}

	// Determine one-sided preference in RECOVERY
	preferOneSided := mode == core.ModeRecovery && db.preferOneSided
	favorSells := invDev > 0

	minOffsetBps := float64(10)
	if db.invCfg != nil {
		minOffsetBps = float64(db.invCfg.MinOffsetBps)
	}

	for i := 0; i < nLevels; i++ {
		baseOffsetBps := float64(offsets[i])
		baseSizeMult := sizeMults[i] * modeSizeMult

		if baseSizeMult <= 0 {
			continue
		}

		// --- BID side ---
		bidSkipOneSided := preferOneSided && favorSells
		bidOffsetBps := baseOffsetBps + skewBps
		bidSizeFactor := 1.0

		if bidSkipOneSided {
			bidOffsetBps *= 1.5
			bidSizeFactor = 0.3
		}

		bidOffsetBps = math.Max(bidOffsetBps, minOffsetBps)
		bidOffsetBps = math.Max(bidOffsetBps, spreadBps)

		// Apply size tilt
		bidTiltMult := 1.0 - sizeTilt
		bidTiltMult = math.Max(0.1, math.Min(bidTiltMult, 1.5))

		bidKey := fmt.Sprintf("BUY_%d", i)
		var bidPrice float64
		if cached, ok := db.cachedPrices[bidKey]; ok && !regenerate {
			bidPrice = cached
		} else {
			bidPrice = db.applyPriceJitter(
				roundDown(mid*(1.0-bidOffsetBps/10000.0), snap.TickSize),
				snap.TickSize,
				-1,
			)
			db.cachedPrices[bidKey] = bidPrice
		}

		bidBaseQty := db.depthCfg.QuotePerOrder * baseSizeMult * bidSizeFactor * bidTiltMult
		var bidQty float64
		if cached, ok := db.cachedQtys[bidKey]; ok && !regenerate {
			bidQty = cached
		} else {
			bidQty = db.applyQtyJitter(
				floorToStep(bidBaseQty/bidPrice, snap.StepSize),
			)
			db.cachedQtys[bidKey] = bidQty
		}

		if bidPrice > 0 && bidQty > 0 && bidPrice*bidQty >= snap.MinNotional {
			orders = append(orders, core.DesiredOrder{
				Side:       "BUY",
				Price:      bidPrice,
				Qty:        bidQty,
				LevelIndex: i,
				Tag:        fmt.Sprintf("MM_%s_L%d_BID", batchID, i),
			})
		}

		// --- ASK side ---
		askSkipOneSided := preferOneSided && !favorSells
		askOffsetBps := baseOffsetBps - skewBps
		askSizeFactor := 1.0

		if askSkipOneSided {
			askOffsetBps *= 1.5
			askSizeFactor = 0.3
		}

		askOffsetBps = math.Max(askOffsetBps, minOffsetBps)
		askOffsetBps = math.Max(askOffsetBps, spreadBps)

		// Apply size tilt
		askTiltMult := 1.0 + sizeTilt
		askTiltMult = math.Max(0.1, math.Min(askTiltMult, 1.5))

		askKey := fmt.Sprintf("SELL_%d", i)
		var askPrice float64
		if cached, ok := db.cachedPrices[askKey]; ok && !regenerate {
			askPrice = cached
		} else {
			askPrice = db.applyPriceJitter(
				roundUp(mid*(1.0+askOffsetBps/10000.0), snap.TickSize),
				snap.TickSize,
				1,
			)
			db.cachedPrices[askKey] = askPrice
		}

		askBaseQty := db.depthCfg.QuotePerOrder * baseSizeMult * askSizeFactor * askTiltMult
		var askQty float64
		if cached, ok := db.cachedQtys[askKey]; ok && !regenerate {
			askQty = cached
		} else {
			askQty = db.applyQtyJitter(
				floorToStep(askBaseQty/askPrice, snap.StepSize),
			)
			db.cachedQtys[askKey] = askQty
		}

		if askPrice > 0 && askQty > 0 && askPrice*askQty >= snap.MinNotional {
			orders = append(orders, core.DesiredOrder{
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
					orders = orders[:len(orders)-2]
				}
			}
		}
	}

	return orders
}

// EnforceDepthKPI adjusts the ladder to meet depth notional targets.
func (db *DepthBuilder) EnforceDepthKPI(orders []core.DesiredOrder, snap *core.Snapshot) []core.DesiredOrder {
	targetNotional := db.depthCfg.TargetDepthNotional
	if targetNotional <= 0 {
		return orders
	}

	depthRange := snap.Mid * 0.02
	var bidNotional, askNotional float64

	for _, o := range orders {
		if o.Side == "BUY" && o.Price >= snap.Mid-depthRange {
			bidNotional += o.Price * o.Qty
		}
		if o.Side == "SELL" && o.Price <= snap.Mid+depthRange {
			askNotional += o.Price * o.Qty
		}
	}

	halfTarget := targetNotional / 2.0
	if halfTarget > 0 {
		if bidNotional > 0 && bidNotional < halfTarget {
			scaleFactor := halfTarget / bidNotional
			if scaleFactor > 3.0 {
				scaleFactor = 3.0
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

// ApplyBudgetConstraints ensures orders fit within available balances.
func (db *DepthBuilder) ApplyBudgetConstraints(orders []core.DesiredOrder, baseFree, quoteFree float64) []core.DesiredOrder {
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
		filtered := make([]core.DesiredOrder, 0, len(orders))
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

func (db *DepthBuilder) applyPriceJitter(price, tickSize float64, direction int) float64 {
	jitterTicks := rand.Intn(db.antiAbuse.PriceJitterTicks + 1)
	microTicks := rand.Intn(db.antiAbuse.MicroOffsetTicks + 1)
	totalTicks := jitterTicks + microTicks

	offset := float64(totalTicks) * tickSize * float64(direction)
	return price + offset
}

func (db *DepthBuilder) applyQtyJitter(qty float64) float64 {
	if db.antiAbuse.QtyJitterPct <= 0 {
		return qty
	}
	jitter := 1.0 - db.antiAbuse.QtyJitterPct + rand.Float64()*2.0*db.antiAbuse.QtyJitterPct
	return qty * jitter
}

// Helper functions
func roundDown(value, step float64) float64 {
	if step <= 0 {
		return value
	}
	return math.Floor(value/step) * step
}

func roundUp(value, step float64) float64 {
	if step <= 0 {
		return value
	}
	return math.Ceil(value/step) * step
}

func floorToStep(value, step float64) float64 {
	if step <= 0 {
		return value
	}
	return math.Floor(value/step) * step
}
