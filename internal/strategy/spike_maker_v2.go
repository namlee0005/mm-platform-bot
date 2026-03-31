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

// SpikeMakerV2Config is the configuration for SpikeMakerV2Strategy.
type SpikeMakerV2Config struct {
	*config.SimpleConfig

	// Risk
	DrawdownWarnPct     float64 `json:"drawdown_warn_pct"`
	DrawdownReducePct   float64 `json:"drawdown_reduce_pct"`
	MaxRecoverySizeMult float64 `json:"max_recovery_size_mult"`
	RelistToleranceBps  float64 `json:"relist_tolerance_bps"`

	// V2: Compliance mode — "capital" (default) or "compliance"
	ComplianceMode string `json:"compliance_mode"`
}

// ──────────────────────────────────────────────────────────────────────────────
// Strategy
// ──────────────────────────────────────────────────────────────────────────────

// SpikeMakerV2Strategy is a two-sided market maker with spike-adaptive depth
// generation, toxicity-based fill response, dynamic requote cooldown,
// ninja protection, and compliance toggle.
type SpikeMakerV2Strategy struct {
	cfg *SpikeMakerV2Config

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
	lastRet      float64

	// Toxicity-based fill response
	fillHistory      []fillRecord
	lastFillToxicity int
	fillRebuildSide  string
	lastFillAt       time.Time

	// V2: Dynamic requote cooldown
	lastRequoteAt time.Time
	lastSpikeS    float64 // cached spike score for cooldown calc

	// V2: Emergency pause (tox=2)
	emergencyPauseUntil time.Time
}

// ──────────────────────────────────────────────────────────────────────────────
// Constructor
// ──────────────────────────────────────────────────────────────────────────────

func NewSpikeMakerV2Strategy(cfg *SpikeMakerV2Config) *SpikeMakerV2Strategy {
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
	if cfg.ComplianceMode == "" {
		cfg.ComplianceMode = "capital"
	}

	return &SpikeMakerV2Strategy{
		cfg:         cfg,
		navHistory:  make([]navSnapshot, 0, 1000),
		currentMode: smModeNormal,
	}
}

func (s *SpikeMakerV2Strategy) Name() string { return "SpikeMakerV2" }

// ──────────────────────────────────────────────────────────────────────────────
// Init
// ──────────────────────────────────────────────────────────────────────────────

func (s *SpikeMakerV2Strategy) Init(ctx context.Context, snap *core.Snapshot, balance *core.BalanceState) error {
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

	log.Printf("[%s] Initialized: tickSize=%.8f, stepSize=%.8f, minNotional=%.2f, maxOrderQty=%.0f, compliance=%s",
		s.Name(), s.tickSize, s.stepSize, s.minNotional, s.maxOrderQty, s.cfg.ComplianceMode)
	return nil
}

// ──────────────────────────────────────────────────────────────────────────────
// Tick — main loop
// ──────────────────────────────────────────────────────────────────────────────

func (s *SpikeMakerV2Strategy) Tick(ctx context.Context, input *core.TickInput) (*core.TickOutput, error) {
	snap := input.Snapshot
	balance := input.Balance

	// Validate market data
	mid := snap.Mid
	if mid <= 0 || snap.BestBid <= 0 || snap.BestAsk <= 0 {
		return &core.TickOutput{Action: core.TickActionCancelAll, Reason: "invalid market data"}, nil
	}

	// ── V2: Emergency pause check (tox=2 → 5s full stop) ──
	if !s.emergencyPauseUntil.IsZero() && time.Now().Before(s.emergencyPauseUntil) {
		return &core.TickOutput{
			Action: core.TickActionCancelAll,
			Reason: fmt.Sprintf("emergency_pause: tox=2, resume in %.1fs", time.Until(s.emergencyPauseUntil).Seconds()),
		}, nil
	}

	// ── EWM vol update ──
	s.lastRet = 0
	if s.volInited && s.volLastMid > 0 && mid > 0 {
		ret := math.Log(mid / s.volLastMid)
		if !math.IsNaN(ret) && !math.IsInf(ret, 0) {
			s.lastRet = ret
			const volAlpha = 0.06
			s.volEwmVar = (1-volAlpha)*s.volEwmVar + volAlpha*ret*ret
		}
	}
	s.volLastMid = mid
	s.volInited = true
	s.volTickCount++

	const minVolVar = 1e-8
	if s.volEwmVar < minVolVar {
		s.volEwmVar = minVolVar
	}

	// ── Risk state ──
	nav := balance.TotalBase()*mid + balance.TotalQuote()
	drawdown, mode := s.updateRiskStateV2(nav)

	if mode == smModePaused {
		return &core.TickOutput{
			Action: core.TickActionCancelAll,
			Reason: fmt.Sprintf("PAUSED: drawdown %.2f%%", drawdown*100),
		}, nil
	}

	// ── V2: Dynamic requote cooldown ──
	sigma := math.Sqrt(s.volEwmVar)
	S := computeSpikeScore(s.lastRet, sigma)
	s.lastSpikeS = S

	if s.shouldSkipRequote(S) {
		return &core.TickOutput{Action: core.TickActionKeep, Reason: "cooldown"}, nil
	}
	s.lastRequoteAt = time.Now()

	// ── Balance check ──
	avail := balance.TotalBase()*mid + balance.TotalQuote()
	minReq := s.cfg.TargetDepthNotional * 2
	if avail < minReq {
		return &core.TickOutput{
			Action: core.TickActionCancelAll,
			Reason: fmt.Sprintf("insufficient balance: $%.2f < $%.2f", avail, minReq),
		}, nil
	}

	sizeMult := s.calculateSizeMultV2(drawdown, mode)

	// ── Inventory ──
	inv := calculateInventoryState(
		balance.TotalBase(), balance.TotalQuote(), mid,
		float64(s.cfg.InitBase), float64(s.cfg.InitQuote),
	)

	// ── OFI from orderbook ──
	ofi := s.computeOFIV2(snap)

	// ── V2: Accumulated volumes for Ninja Protection ──
	accBuyVol, accSellVol := s.computeAccVolumes30s()

	// Best bid/ask sizes from snapshot
	bestBidSize, bestAskSize := 0.0, 0.0
	if len(snap.Bids) > 0 {
		bestBidSize = snap.Bids[0].Qty * snap.Bids[0].Price
	}
	if len(snap.Asks) > 0 {
		bestAskSize = snap.Asks[0].Qty * snap.Asks[0].Price
	}

	// ── Generate desired orders via V2 13-step engine ──
	result := generateSpikeAdaptiveOrdersV2(SpikeDepthV2Params{
		BestBid:        snap.BestBid,
		BestAsk:        snap.BestAsk,
		BestBidSize:    bestBidSize,
		BestAskSize:    bestAskSize,
		TickSize:       s.tickSize,
		StepSize:       s.stepSize,
		MinNotional:    s.minNotional,
		MaxOrderQty:    s.maxOrderQty,
		NLevels:        s.cfg.NumLevels,
		TotalDepth:     s.cfg.TargetDepthNotional,
		DepthBps:       s.cfg.DepthBps,
		SpreadMinBps:   s.cfg.SpreadMinBps,
		Sigma:          sigma,
		Ret:            s.lastRet,
		NormalizedOFI:  ofi,
		InvRatio:       inv.skew / 0.5,
		SizeMult:       sizeMult,
		AccBuyVol30s:   accBuyVol,
		AccSellVol30s:  accSellVol,
		ComplianceMode: s.cfg.ComplianceMode,
	})

	desired := result.Orders

	// ── Budget constraint ──
	desired = s.applyBudgetConstraintV2(desired, balance, result.FairPrice)

	// ── Converge with toxicity scope ──
	scope := s.computeRebuildScopeV2(result.FairPrice)
	toCancel, toCreate := s.convergeV2(desired, input.LiveOrders, scope)

	// ── Log ──
	bidN, askN := 0, 0
	for _, d := range desired {
		if d.Side == "BUY" {
			bidN++
		} else {
			askN++
		}
	}

	log.Printf("[%s] fair=%.8f micro=%.8f mid=%.8f S=%.2f ofi=%.2f inv=%.1f%% mode=%s ninja=(buy$%.1f sell$%.1f) orders=%d (bid=%d ask=%d) cancel=%d create=%d | gamma=%.2f spread=%.0fbps skew=%.3f bidScale=%.2f askScale=%.2f w0=%.3f wN=%.3f",
		s.Name(), result.FairPrice, result.Microprice, result.Mid,
		result.SpikeScore, ofi, inv.skew*100,
		mode, accBuyVol, accSellVol, len(desired), bidN, askN, len(toCancel), len(toCreate),
		result.Gamma, result.Spread*10000, result.InvSkew, result.BidScale, result.AskScale, result.WeightL0, result.WeightLast)

	if len(toCancel) == 0 && len(toCreate) == 0 {
		return &core.TickOutput{Action: core.TickActionKeep, Reason: "ladder_ok"}, nil
	}

	// Safety: don't cancel all with no replacements
	liveCount := len(input.LiveOrders)
	if len(toCreate) == 0 && len(toCancel) >= liveCount && liveCount > 0 {
		log.Printf("[%s] WARNING: converge wants to cancel all %d orders with 0 replacements — keeping", s.Name(), liveCount)
		return &core.TickOutput{Action: core.TickActionKeep, Reason: "safety_no_orphan"}, nil
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
// V2: Dynamic Requote Cooldown
// ──────────────────────────────────────────────────────────────────────────────

// getModeCooldownMs returns fixed cooldown per risk mode.
func (s *SpikeMakerV2Strategy) getModeCooldownMs() float64 {
	switch s.currentMode {
	case smModeNormal:
		return 1000
	case smModeWarning:
		return 5000
	case smModeRecovery:
		return 30000
	case smModePaused:
		return 60000
	}
	return 1000
}

// shouldSkipRequote checks if cooldown should prevent requoting this tick.
func (s *SpikeMakerV2Strategy) shouldSkipRequote(S float64) bool {
	if s.lastRequoteAt.IsZero() {
		return false // first tick always proceeds
	}

	// Bypass cooldown on fill with toxicity < 2
	if !s.lastFillAt.IsZero() && s.lastFillAt.After(s.lastRequoteAt) && s.lastFillToxicity < 2 {
		return false
	}

	spikeCooldownMs := 100.0 * (1.0 + 4.5*math.Pow(S, 0.7))
	modeCooldownMs := s.getModeCooldownMs()
	cooldownMs := math.Max(spikeCooldownMs, modeCooldownMs)
	cooldownMs = math.Min(cooldownMs, 60000)

	elapsed := time.Since(s.lastRequoteAt).Milliseconds()
	return float64(elapsed) < cooldownMs
}

// ──────────────────────────────────────────────────────────────────────────────
// V2: Accumulated volumes for Ninja Protection
// ──────────────────────────────────────────────────────────────────────────────

func (s *SpikeMakerV2Strategy) computeAccVolumes30s() (buyVol, sellVol float64) {
	cutoff := time.Now().Add(-30 * time.Second)
	for _, f := range s.fillHistory {
		if f.timestamp.After(cutoff) {
			notional := f.price * f.qty
			if f.side == "BUY" {
				buyVol += notional
			} else {
				sellVol += notional
			}
		}
	}
	return
}

// ──────────────────────────────────────────────────────────────────────────────
// OFI — Order Flow Imbalance from orderbook
// ──────────────────────────────────────────────────────────────────────────────

func (s *SpikeMakerV2Strategy) computeOFIV2(snap *core.Snapshot) float64 {
	if snap.Mid <= 0 {
		return 0
	}
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
	return (bidDepth - askDepth) / total
}

// ──────────────────────────────────────────────────────────────────────────────
// OnFill — V2 toxicity scoring with depth % and fill frequency
// ──────────────────────────────────────────────────────────────────────────────

func (s *SpikeMakerV2Strategy) OnFill(event *core.FillEvent) {
	// No-op: all fill handling is done in OnOrderUpdate
}

func (s *SpikeMakerV2Strategy) OnOrderUpdate(event *core.OrderEvent) {
	if event.Status != "FILLED" && event.Status != "PARTIALLY_FILLED" {
		return
	}

	fillQty := event.ExecutedQty
	notional := event.Price * fillQty
	now := time.Now()

	if event.Status == "PARTIALLY_FILLED" {
		log.Printf("[%s] ⚡ PARTIAL FILL %s @ %.8f filled=%.4f/%.4f ($%.2f) order=%s",
			s.Name(), event.Side, event.Price,
			fillQty, event.Quantity, notional, event.OrderID)
	} else {
		log.Printf("[%s] ✅ FILLED %s @ %.8f qty=%.4f ($%.2f) order=%s",
			s.Name(), event.Side, event.Price,
			fillQty, notional, event.OrderID)
	}

	// Record fill (capped at 500 entries + 60s window)
	s.fillHistory = append(s.fillHistory, fillRecord{
		side:      event.Side,
		price:     event.Price,
		qty:       fillQty,
		timestamp: now,
		mid:       s.volLastMid,
	})
	cutoff := now.Add(-60 * time.Second)
	for len(s.fillHistory) > 0 && s.fillHistory[0].timestamp.Before(cutoff) {
		s.fillHistory = s.fillHistory[1:]
	}
	if len(s.fillHistory) > 500 {
		s.fillHistory = s.fillHistory[len(s.fillHistory)-500:]
	}

	s.fillRebuildSide = event.Side
	s.lastFillAt = now

	// ── Compute toxicity (V2: 4 signals) ──
	toxicity := 0

	// Signal 1: spike score
	spikeS := computeSpikeScore(s.lastRet, math.Sqrt(s.volEwmVar))
	if spikeS >= 3.0 {
		toxicity = 2
	} else if spikeS >= 1.5 {
		toxicity = 1
	}

	// Signal 2: adverse price movement since fill
	if s.volLastMid > 0 && event.Price > 0 {
		var adverseMove float64
		if event.Side == "BUY" {
			adverseMove = (event.Price - s.volLastMid) / event.Price
		} else {
			adverseMove = (s.volLastMid - event.Price) / event.Price
		}
		if adverseMove > 0.005 {
			toxicity = 2
		} else if adverseMove > 0.002 && toxicity < 1 {
			toxicity = 1
		}
	}

	// Signal 3 (V2): same-side fills as % of total depth in 30s
	if s.cfg.TargetDepthNotional > 0 {
		var sameSideNotional float64
		window30s := now.Add(-30 * time.Second)
		for _, f := range s.fillHistory {
			if f.side == event.Side && f.timestamp.After(window30s) {
				sameSideNotional += f.price * f.qty
			}
		}
		depthPct := sameSideNotional / s.cfg.TargetDepthNotional
		if depthPct >= 0.03 {
			toxicity = 2
		} else if depthPct >= 0.01 && toxicity < 1 {
			toxicity = 1
		}
	}

	// Signal 4 (V2 NEW): fill frequency — fills in last 10s
	window10s := now.Add(-10 * time.Second)
	fillCount10s := 0
	for _, f := range s.fillHistory {
		if f.timestamp.After(window10s) {
			fillCount10s++
		}
	}
	if fillCount10s > 5 {
		toxicity = 2
	} else if fillCount10s > 3 && toxicity < 1 {
		toxicity = 1
	}

	s.lastFillToxicity = toxicity

	// V2: tox=2 triggers 5s emergency pause
	if toxicity >= 2 {
		s.emergencyPauseUntil = now.Add(5 * time.Second)
		log.Printf("[%s] 🚨 EMERGENCY PAUSE 5s: toxicity=%d (S=%.1f, fillFreq=%d/10s, side=%s)",
			s.Name(), toxicity, spikeS, fillCount10s, event.Side)
	} else {
		log.Printf("[%s] toxicity=%d (S=%.1f, fillFreq=%d/10s, side=%s)",
			s.Name(), toxicity, spikeS, fillCount10s, event.Side)
	}
}

func (s *SpikeMakerV2Strategy) UpdateConfig(newCfg interface{}) error {
	log.Printf("[%s] Config update not yet implemented", s.Name())
	return nil
}

func (s *SpikeMakerV2Strategy) UpdatePrevSnapshot(liveOrders []core.LiveOrder, balance *core.BalanceState) {
	// No-op — stateless convergence
}

// ──────────────────────────────────────────────────────────────────────────────
// Converge — V2 scoped order diffing
// ──────────────────────────────────────────────────────────────────────────────

func (s *SpikeMakerV2Strategy) computeRebuildScopeV2(mid float64) *rebuildScope {
	if s.lastFillAt.IsZero() || time.Since(s.lastFillAt) > 10*time.Second {
		return nil // full rebuild
	}
	switch s.lastFillToxicity {
	case 0:
		return &rebuildScope{side: s.fillRebuildSide, maxLevels: 2, mid: mid}
	case 1:
		return &rebuildScope{side: s.fillRebuildSide, maxLevels: 3, mid: mid}
	default:
		// tox=2: emergency pause handles this (cancel all in Tick), but if we
		// somehow get here, do a full rebuild
		return nil
	}
}

func (s *SpikeMakerV2Strategy) convergeV2(desired []core.DesiredOrder, live []core.LiveOrder, scope *rebuildScope) (toCancel []string, toCreate []core.DesiredOrder) {
	tolerance := s.cfg.RelistToleranceBps / 10000.0
	if tolerance <= 0 {
		tolerance = 25.0 / 10000.0
	}

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
		if scope != nil && scope.side != side {
			return
		}

		var inScope []core.LiveOrder
		if scope != nil && scope.mid > 0 {
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
				}
			}
			liveSide = inScope
		}

		used := make([]bool, len(liveSide))

		for _, d := range desiredSide {
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
			} else {
				toCreate = append(toCreate, d)
			}
		}

		for j, lo := range liveSide {
			if !used[j] {
				closestDist := math.MaxFloat64
				closestPrice := 0.0
				for _, d := range desiredSide {
					diff := math.Abs(lo.Price-d.Price) / d.Price
					if diff < closestDist {
						closestDist = diff
						closestPrice = d.Price
					}
				}
				if len(desiredSide) == 0 {
					log.Printf("[%s] converge: CANCEL %s %s @ %.8f (no desired orders on this side)",
						s.Name(), lo.OrderID, side, lo.Price)
				} else {
					log.Printf("[%s] converge: CANCEL %s %s @ %.8f (unmatched, closest desired=%.8f, diff=%.0fbps, tolerance=%.0fbps)",
						s.Name(), lo.OrderID, side, lo.Price, closestPrice, closestDist*10000, tolerance*10000)
				}
				toCancel = append(toCancel, lo.OrderID)
			}
		}
	}

	matchSide(dBuys, lBuys, "BUY")
	matchSide(dSells, lSells, "SELL")

	return toCancel, toCreate
}

// ──────────────────────────────────────────────────────────────────────────────
// Risk management (drawdown modes) — same as V1
// ──────────────────────────────────────────────────────────────────────────────

func (s *SpikeMakerV2Strategy) updateRiskStateV2(nav float64) (float64, SpikeMakerMode) {
	now := time.Now()

	s.navHistory = append(s.navHistory, navSnapshot{timestamp: now, nav: nav})
	cutoff := now.Add(-24 * time.Hour)
	for len(s.navHistory) > 0 && s.navHistory[0].timestamp.Before(cutoff) {
		s.navHistory = s.navHistory[1:]
	}
	if len(s.navHistory) > 100000 {
		s.navHistory = s.navHistory[len(s.navHistory)-100000:]
	}

	if s.currentMode == smModeNormal || s.currentMode == smModeWarning {
		if nav > s.peakNAV {
			s.peakNAV = nav
		}
	}
	if s.peakNAV <= 0 {
		s.peakNAV = nav
	}

	drawdown := 0.0
	if s.peakNAV > 0 && nav > 0 {
		drawdown = (s.peakNAV - nav) / s.peakNAV
		if drawdown < 0 {
			drawdown = 0
		}
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

func (s *SpikeMakerV2Strategy) calculateSizeMultV2(drawdown float64, mode SpikeMakerMode) float64 {
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
// Budget constraint — same as V1
// ──────────────────────────────────────────────────────────────────────────────

func (s *SpikeMakerV2Strategy) applyBudgetConstraintV2(desired []core.DesiredOrder, balance *core.BalanceState, mid float64) []core.DesiredOrder {
	var bidTotal, askTotal float64
	for _, d := range desired {
		if d.Side == "BUY" {
			bidTotal += d.Price * d.Qty
		} else {
			askTotal += d.Price * d.Qty
		}
	}

	availQuote := balance.QuoteFree
	availBase := balance.BaseFree

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
