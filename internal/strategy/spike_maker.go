package strategy

import (
	"context"
	"fmt"
	"log"
	"math"
	"sort"
	"time"

	"mm-platform-engine/internal/config"
	"mm-platform-engine/internal/core"
)

// ──────────────────────────────────────────────────────────────────────────────
// Config
// ──────────────────────────────────────────────────────────────────────────────

// SpikeMakerConfig is the configuration for SpikeMakerStrategy
type SpikeMakerConfig struct {
	*config.SimpleConfig

	// Risk
	DrawdownWarnPct     float64 `json:"drawdown_warn_pct"`
	DrawdownReducePct   float64 `json:"drawdown_reduce_pct"`
	MaxRecoverySizeMult float64 `json:"max_recovery_size_mult"`
	RelistToleranceBps  float64 `json:"relist_tolerance_bps"`
}

// ──────────────────────────────────────────────────────────────────────────────
// Strategy
// ──────────────────────────────────────────────────────────────────────────────

// SpikeMakerStrategy is a two-sided market maker with spike-adaptive depth
// generation and toxicity-based fill response.
type SpikeMakerStrategy struct {
	cfg *SpikeMakerConfig

	// Market info
	tickSize    float64
	stepSize    float64
	minNotional float64
	maxOrderQty float64

	// Risk
	peakNAV        float64
	navHistory     []navSnapshot
	currentMode    SpikeMakerMode
	modeChangedAt  time.Time
	recoveryTarget float64

	// Fair price (VWAP)
	lastVWAP     float64
	lastVWAPTime time.Time

	// EWM volatility tracker
	volEwmVar    float64
	volLastMid   float64
	volInited    bool
	volTickCount int
	lastRet      float64 // most recent log return (for spike score)

	// Toxicity-based fill response
	fillHistory      []fillRecord
	lastFillToxicity int    // 0=normal, 1=moderate, 2=high
	fillRebuildSide  string // "BUY" or "SELL"
	lastFillAt       time.Time
}

type SpikeMakerMode string

const (
	smModeNormal   SpikeMakerMode = "NORMAL"
	smModeWarning  SpikeMakerMode = "WARNING"
	smModeRecovery SpikeMakerMode = "RECOVERY"
	smModePaused   SpikeMakerMode = "PAUSED"
)

// fillRecord tracks a single fill for toxicity scoring
type fillRecord struct {
	side      string
	price     float64
	qty       float64
	timestamp time.Time
	mid       float64
}

// rebuildScope controls which levels converge() may touch
type rebuildScope struct {
	side      string  // "BUY", "SELL"
	maxLevels int     // only diff top N inner levels
	mid       float64 // current mid for distance sorting
}

// ──────────────────────────────────────────────────────────────────────────────
// Constructor
// ──────────────────────────────────────────────────────────────────────────────

func NewSpikeMakerStrategy(cfg *SpikeMakerConfig) *SpikeMakerStrategy {
	if cfg.NumLevels == 0 {
		cfg.NumLevels = 10
	}
	if cfg.DepthBps == 0 {
		cfg.DepthBps = 200
	}
	if cfg.DrawdownLimitPct == 0 {
		cfg.DrawdownLimitPct = 0.05
	}
	if cfg.DrawdownWarnPct == 0 {
		cfg.DrawdownWarnPct = 0.03
	}
	if cfg.DrawdownReducePct == 0 {
		cfg.DrawdownReducePct = 0.02
	}
	if cfg.MaxRecoverySizeMult == 0 {
		cfg.MaxRecoverySizeMult = 0.3
	}
	if cfg.RelistToleranceBps == 0 {
		cfg.RelistToleranceBps = 25.0
	}

	return &SpikeMakerStrategy{
		cfg:         cfg,
		navHistory:  make([]navSnapshot, 0, 1000),
		currentMode: smModeNormal,
	}
}

func (s *SpikeMakerStrategy) Name() string { return "SpikeMaker" }

// ──────────────────────────────────────────────────────────────────────────────
// Init
// ──────────────────────────────────────────────────────────────────────────────

func (s *SpikeMakerStrategy) Init(ctx context.Context, snap *core.Snapshot, balance *core.BalanceState) error {
	s.tickSize = snap.TickSize
	s.stepSize = snap.StepSize
	s.minNotional = snap.MinNotional
	s.maxOrderQty = snap.MaxOrderQty

	if s.tickSize <= 0 {
		s.tickSize = 1e-8
	}
	if s.stepSize <= 0 {
		s.stepSize = 1e-8
	}
	if s.minNotional <= 0 {
		s.minNotional = 5.0
	}
	if s.maxOrderQty <= 0 {
		s.maxOrderQty = 1_000_000
	}

	if s.cfg.InitBase == 0 && s.cfg.InitQuote == 0 {
		s.cfg.InitBase = balance.TotalBase()
		s.cfg.InitQuote = balance.TotalQuote()
		log.Printf("[%s] Auto-detected init inventory: base=%.2f, quote=%.2f",
			s.Name(), s.cfg.InitBase, s.cfg.InitQuote)
	}

	log.Printf("[%s] Initialized: tickSize=%.8f, stepSize=%.8f, minNotional=%.2f, maxOrderQty=%.0f",
		s.Name(), s.tickSize, s.stepSize, s.minNotional, s.maxOrderQty)
	return nil
}

// ──────────────────────────────────────────────────────────────────────────────
// Tick — main loop
// ──────────────────────────────────────────────────────────────────────────────

func (s *SpikeMakerStrategy) Tick(ctx context.Context, input *core.TickInput) (*core.TickOutput, error) {
	snap := input.Snapshot
	balance := input.Balance

	// Validate market data
	mid := snap.Mid
	if mid <= 0 || snap.BestBid <= 0 || snap.BestAsk <= 0 {
		return &core.TickOutput{Action: core.TickActionCancelAll, Reason: "invalid market data"}, nil
	}

	// ── EWM vol update (track raw mid, not fair price) ──
	s.lastRet = 0
	if s.volInited && s.volLastMid > 0 {
		s.lastRet = math.Log(mid / s.volLastMid)
		const volAlpha = 0.06
		s.volEwmVar = (1-volAlpha)*s.volEwmVar + volAlpha*s.lastRet*s.lastRet
	}
	s.volLastMid = mid
	s.volInited = true
	s.volTickCount++

	// ── Risk state ──
	nav := balance.TotalBase()*mid + balance.TotalQuote()
	drawdown, mode := s.updateRiskState(nav)

	if mode == smModePaused {
		return &core.TickOutput{
			Action: core.TickActionCancelAll,
			Reason: fmt.Sprintf("PAUSED: drawdown %.2f%%", drawdown*100),
		}, nil
	}

	// ── Balance check ──
	avail := balance.TotalBase()*mid + balance.TotalQuote()
	minReq := s.cfg.TargetDepthNotional * 2
	if avail < minReq {
		return &core.TickOutput{
			Action: core.TickActionCancelAll,
			Reason: fmt.Sprintf("insufficient balance: $%.2f < $%.2f", avail, minReq),
		}, nil
	}

	sizeMult := s.calculateSizeMultiplier(drawdown, mode)

	// ── Inventory ──
	inv := calculateInventoryState(
		balance.TotalBase(), balance.TotalQuote(), mid,
		float64(s.cfg.InitBase), float64(s.cfg.InitQuote),
	)

	// ── OFI from orderbook ──
	ofi := s.computeOFI(snap)

	// ── Generate desired orders via v2 spike engine ──
	sigma := math.Sqrt(s.volEwmVar)

	// Best bid/ask sizes from snapshot
	bestBidSize, bestAskSize := 0.0, 0.0
	if len(snap.Bids) > 0 {
		bestBidSize = snap.Bids[0].Qty * snap.Bids[0].Price
	}
	if len(snap.Asks) > 0 {
		bestAskSize = snap.Asks[0].Qty * snap.Asks[0].Price
	}

	result := generateSpikeAdaptiveOrders(SpikeDepthParams{
		BestBid:       snap.BestBid,
		BestAsk:       snap.BestAsk,
		BestBidSize:   bestBidSize,
		BestAskSize:   bestAskSize,
		TickSize:      s.tickSize,
		StepSize:      s.stepSize,
		MinNotional:   s.minNotional,
		MaxOrderQty:   s.maxOrderQty,
		NLevels:       s.cfg.NumLevels,
		TotalDepth:    s.cfg.TargetDepthNotional,
		DepthBps:      s.cfg.DepthBps,
		Sigma:         sigma,
		Ret:           s.lastRet,
		NormalizedOFI: ofi,
		InvRatio:      inv.skew / 0.5, // normalize skew [-0.5,0.5] to [-1,1]
		SizeMult:      sizeMult,
	})

	desired := result.Orders

	// ── Budget constraint ──
	desired = s.applyBudgetConstraint(desired, balance, result.FairPrice)

	// ── Converge with toxicity scope ──
	scope := s.computeRebuildScope(result.FairPrice)
	toCancel, toCreate := s.converge(desired, input.LiveOrders, scope)

	// ── Log ──
	bidN, askN := 0, 0
	for _, d := range desired {
		if d.Side == "BUY" {
			bidN++
		} else {
			askN++
		}
	}

	log.Printf("[%s] fair=%.8f micro=%.8f mid=%.8f S=%.2f ofi=%.2f inv=%.1f%% mode=%s orders=%d (bid=%d ask=%d) cancel=%d create=%d",
		s.Name(), result.FairPrice, result.Microprice, result.Mid,
		result.SpikeScore, ofi, inv.skew*100,
		mode, len(desired), bidN, askN, len(toCancel), len(toCreate))

	if len(toCancel) == 0 && len(toCreate) == 0 {
		return &core.TickOutput{Action: core.TickActionKeep, Reason: "ladder_ok"}, nil
	}

	return &core.TickOutput{
		Action:         core.TickActionAmend,
		OrdersToCancel: toCancel,
		OrdersToAdd:    toCreate,
		Reason: fmt.Sprintf("converge: desired=%d cancel=%d create=%d S=%.1f tox=%d",
			len(desired), len(toCancel), len(toCreate), result.SpikeScore, s.lastFillToxicity),
	}, nil
}

// ──────────────────────────────────────────────────────────────────────────────
// OFI — Order Flow Imbalance from orderbook
// ──────────────────────────────────────────────────────────────────────────────

// computeOFI returns normalized order flow imbalance in [-1, 1].
// Positive = buy pressure (more bid depth), negative = sell pressure.
func (s *SpikeMakerStrategy) computeOFI(snap *core.Snapshot) float64 {
	if snap.Mid <= 0 {
		return 0
	}
	// Sum depth within 1% of mid on each side
	depthRange := snap.Mid * 0.01
	var bidDepth, askDepth float64
	for _, lvl := range snap.Bids {
		if lvl.Price >= snap.Mid-depthRange {
			bidDepth += lvl.Qty * lvl.Price
		}
	}
	for _, lvl := range snap.Asks {
		if lvl.Price <= snap.Mid+depthRange {
			askDepth += lvl.Qty * lvl.Price
		}
	}
	total := bidDepth + askDepth
	if total < 1e-9 {
		return 0
	}
	// Normalize to [-1, 1]: positive = buy pressure
	return (bidDepth - askDepth) / total
}

// ──────────────────────────────────────────────────────────────────────────────
// OnFill — toxicity scoring
// ──────────────────────────────────────────────────────────────────────────────

func (s *SpikeMakerStrategy) OnFill(event *core.FillEvent) {
	notional := event.Price * event.Quantity
	log.Printf("[%s] 💰 FILL %s @ %.8f x %.4f ($%.2f) order=%s",
		s.Name(), event.Side, event.Price, event.Quantity, notional, event.OrderID)

	now := time.Now()

	// Record fill
	s.fillHistory = append(s.fillHistory, fillRecord{
		side:      event.Side,
		price:     event.Price,
		qty:       event.Quantity,
		timestamp: now,
		mid:       s.volLastMid,
	})
	// Trim to 60s
	cutoff := now.Add(-60 * time.Second)
	for len(s.fillHistory) > 0 && s.fillHistory[0].timestamp.Before(cutoff) {
		s.fillHistory = s.fillHistory[1:]
	}

	s.fillRebuildSide = event.Side
	s.lastFillAt = now

	// ── Compute toxicity ──
	toxicity := 0

	// Signal 1: spike score
	spikeS := computeSpikeScore(s.lastRet, math.Sqrt(s.volEwmVar))
	if spikeS >= 3.0 {
		toxicity = 2
	} else if spikeS >= 1.5 && toxicity < 1 {
		toxicity = 1
	}

	// Signal 2: adverse price movement since fill
	if s.volLastMid > 0 && event.Price > 0 {
		var adverseMove float64
		if event.Side == "BUY" {
			// Bought → adverse if price dropped after
			adverseMove = (event.Price - s.volLastMid) / event.Price
		} else {
			// Sold → adverse if price rose after
			adverseMove = (s.volLastMid - event.Price) / event.Price
		}
		if adverseMove > 0.005 { // > 0.5%
			toxicity = 2
		} else if adverseMove > 0.002 && toxicity < 1 { // > 0.2%
			toxicity = 1
		}
	}

	// Signal 3: repeated same-side fills in 30s
	sameSideCount := 0
	window30s := now.Add(-30 * time.Second)
	for _, f := range s.fillHistory {
		if f.side == event.Side && f.timestamp.After(window30s) {
			sameSideCount++
		}
	}
	if sameSideCount >= 3 {
		toxicity = 2
	} else if sameSideCount >= 2 && toxicity < 1 {
		toxicity = 1
	}

	s.lastFillToxicity = toxicity
	log.Printf("[%s] Fill toxicity=%d (S=%.1f, sameSide=%d, side=%s)",
		s.Name(), toxicity, spikeS, sameSideCount, event.Side)
}

func (s *SpikeMakerStrategy) OnOrderUpdate(event *core.OrderEvent) {
	if event.Status == "NEW" {
		log.Printf("[%s] Order %s: %s @ %.8f status=%s",
			s.Name(), event.OrderID, event.Side, event.Price, event.Status)
	}
}

func (s *SpikeMakerStrategy) UpdateConfig(newCfg interface{}) error {
	log.Printf("[%s] Config update not yet implemented", s.Name())
	return nil
}

func (s *SpikeMakerStrategy) UpdatePrevSnapshot(liveOrders []core.LiveOrder, balance *core.BalanceState) {
	// No-op — stateless convergence
}

// ──────────────────────────────────────────────────────────────────────────────
// Converge — scoped order diffing with queue priority preservation
// ──────────────────────────────────────────────────────────────────────────────

// computeRebuildScope decides how much of the book to rebuild based on fill toxicity.
func (s *SpikeMakerStrategy) computeRebuildScope(mid float64) *rebuildScope {
	// No recent fill or high toxicity → full rebuild
	if s.lastFillAt.IsZero() || time.Since(s.lastFillAt) > 10*time.Second {
		return nil // full
	}
	switch s.lastFillToxicity {
	case 0: // normal: only top 2 levels on fill side
		return &rebuildScope{side: s.fillRebuildSide, maxLevels: 2, mid: mid}
	case 1: // moderate: top 3 levels
		return &rebuildScope{side: s.fillRebuildSide, maxLevels: 3, mid: mid}
	default: // high: full rebuild
		return nil
	}
}

func (s *SpikeMakerStrategy) converge(desired []core.DesiredOrder, live []core.LiveOrder, scope *rebuildScope) (toCancel []string, toCreate []core.DesiredOrder) {
	tolerance := s.cfg.RelistToleranceBps / 10000.0
	if tolerance <= 0 {
		tolerance = 25.0 / 10000.0
	}

	// Split by side
	var dBuys, dSells []core.DesiredOrder
	for _, d := range desired {
		if d.Side == "BUY" {
			dBuys = append(dBuys, d)
		} else {
			dSells = append(dSells, d)
		}
	}
	var lBuys, lSells []core.LiveOrder
	for _, lo := range live {
		if lo.Side == "BUY" {
			lBuys = append(lBuys, lo)
		} else {
			lSells = append(lSells, lo)
		}
	}

	matchSide := func(desiredSide []core.DesiredOrder, liveSide []core.LiveOrder, side string) {
		// If scope restricts to a different side, keep everything on this side
		if scope != nil && scope.side != side {
			return // all live orders auto-kept, no creates
		}

		// If scope limits levels, split live into "in scope" and "protected"
		var inScope []core.LiveOrder
		var protectedIDs []string
		if scope != nil && scope.mid > 0 {
			// Sort live by distance from mid (inner first)
			type distOrder struct {
				lo   core.LiveOrder
				dist float64
			}
			var dos []distOrder
			for _, lo := range liveSide {
				dos = append(dos, distOrder{lo: lo, dist: math.Abs(lo.Price - scope.mid)})
			}
			sort.Slice(dos, func(i, j int) bool { return dos[i].dist < dos[j].dist })

			for i, do := range dos {
				if i < scope.maxLevels {
					inScope = append(inScope, do.lo)
				} else {
					protectedIDs = append(protectedIDs, do.lo.OrderID)
				}
			}
			liveSide = inScope
			_ = protectedIDs // protected orders are simply not in liveSide → not cancelled
		}

		used := make([]bool, len(liveSide))

		for _, d := range desiredSide {
			// If scoped, only match desired orders that are "inner" enough
			if scope != nil && scope.mid > 0 {
				dist := math.Abs(d.Price-scope.mid) / scope.mid
				// Rough check: only process desired orders within the scope range
				// Find max distance in inScope
				maxScopeDist := 0.0
				for _, lo := range liveSide {
					dd := math.Abs(lo.Price-scope.mid) / scope.mid
					if dd > maxScopeDist {
						maxScopeDist = dd
					}
				}
				if maxScopeDist > 0 && dist > maxScopeDist*1.5 {
					continue // this desired order is deeper than scope → skip
				}
			}

			bestIdx := -1
			bestDiff := math.MaxFloat64
			for j, lo := range liveSide {
				if used[j] {
					continue
				}
				diff := math.Abs(lo.Price-d.Price) / d.Price
				if diff < bestDiff {
					bestDiff = diff
					bestIdx = j
				}
			}

			if bestIdx >= 0 && bestDiff <= tolerance {
				used[bestIdx] = true
				// Check qty tolerance
				if d.Qty > 0 {
					qtyDiff := math.Abs(liveSide[bestIdx].RemainingQty-d.Qty) / d.Qty
					if qtyDiff > 0.3 {
						toCancel = append(toCancel, liveSide[bestIdx].OrderID)
						toCreate = append(toCreate, d)
						used[bestIdx] = false
					}
				}
			} else {
				toCreate = append(toCreate, d)
			}
		}

		// Cancel unmatched live orders in scope
		for j, lo := range liveSide {
			if !used[j] {
				toCancel = append(toCancel, lo.OrderID)
			}
		}
	}

	matchSide(dBuys, lBuys, "BUY")
	matchSide(dSells, lSells, "SELL")

	return toCancel, toCreate
}

// ──────────────────────────────────────────────────────────────────────────────
// Risk management (drawdown modes)
// ──────────────────────────────────────────────────────────────────────────────

func (s *SpikeMakerStrategy) updateRiskState(nav float64) (float64, SpikeMakerMode) {
	now := time.Now()

	s.navHistory = append(s.navHistory, navSnapshot{timestamp: now, nav: nav})
	cutoff := now.Add(-24 * time.Hour)
	for len(s.navHistory) > 0 && s.navHistory[0].timestamp.Before(cutoff) {
		s.navHistory = s.navHistory[1:]
	}

	if s.currentMode == smModeNormal || s.currentMode == smModeWarning {
		if nav > s.peakNAV {
			s.peakNAV = nav
		}
	}
	if s.peakNAV == 0 {
		s.peakNAV = nav
	}

	drawdown := 0.0
	if s.peakNAV > 0 {
		drawdown = (s.peakNAV - nav) / s.peakNAV
	}

	prev := s.currentMode
	if drawdown >= s.cfg.DrawdownLimitPct {
		s.currentMode = smModePaused
	} else if drawdown >= s.cfg.DrawdownReducePct {
		s.currentMode = smModeRecovery
		if prev != smModeRecovery {
			s.recoveryTarget = s.peakNAV * (1 - s.cfg.DrawdownReducePct*0.5)
		}
	} else if drawdown >= s.cfg.DrawdownWarnPct {
		s.currentMode = smModeWarning
	} else {
		s.currentMode = smModeNormal
		if prev == smModeRecovery && nav >= s.recoveryTarget {
			s.peakNAV = nav
		}
	}

	if s.currentMode != prev {
		s.modeChangedAt = now
		log.Printf("[%s] Mode changed: %s -> %s (dd=%.2f%%, NAV=$%.2f, peak=$%.2f)",
			s.Name(), prev, s.currentMode, drawdown*100, nav, s.peakNAV)
	}

	return drawdown, s.currentMode
}

func (s *SpikeMakerStrategy) calculateSizeMultiplier(drawdown float64, mode SpikeMakerMode) float64 {
	switch mode {
	case smModeNormal:
		return 1.0
	case smModeWarning:
		return 0.8 + 0.2*(1-drawdown/s.cfg.DrawdownReducePct)
	case smModeRecovery:
		severity := (drawdown - s.cfg.DrawdownReducePct) / (s.cfg.DrawdownLimitPct - s.cfg.DrawdownReducePct)
		if severity > 1 {
			severity = 1
		}
		mult := 1.0 - severity*(1.0-s.cfg.MaxRecoverySizeMult)
		if mult < s.cfg.MaxRecoverySizeMult {
			mult = s.cfg.MaxRecoverySizeMult
		}
		return mult
	case smModePaused:
		return 0
	}
	return 1.0
}

// ──────────────────────────────────────────────────────────────────────────────
// Budget constraint
// ──────────────────────────────────────────────────────────────────────────────

func (s *SpikeMakerStrategy) applyBudgetConstraint(desired []core.DesiredOrder, balance *core.BalanceState, mid float64) []core.DesiredOrder {
	var bidTotal, askTotal float64
	for _, d := range desired {
		if d.Side == "BUY" {
			bidTotal += d.Price * d.Qty
		} else {
			askTotal += d.Price * d.Qty
		}
	}

	availQuote := balance.QuoteFree + balance.QuoteLocked
	availBase := balance.BaseFree + balance.BaseLocked

	bidScale := 1.0
	if bidTotal > 0 && availQuote < bidTotal {
		bidScale = availQuote / bidTotal
	}
	askScale := 1.0
	if askTotal > 0 && availBase*mid < askTotal {
		askScale = (availBase * mid) / askTotal
	}

	if bidScale < 1 || askScale < 1 {
		result := make([]core.DesiredOrder, 0, len(desired))
		for _, d := range desired {
			scale := bidScale
			if d.Side == "SELL" {
				scale = askScale
			}
			d.Qty = sdeFloorToStep(d.Qty*scale, s.stepSize)
			if d.Qty > 0 && d.Price*d.Qty >= s.minNotional {
				result = append(result, d)
			}
		}
		return result
	}

	return desired
}
