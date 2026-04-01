package strategy

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"sort"
	"time"

	"mm-platform-engine/internal/config"
	"mm-platform-engine/internal/core"
	"mm-platform-engine/internal/exchange"
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

// ──────────────────────────────────────────────────────────────────────────────
// V2.2: Event Logging
// ──────────────────────────────────────────────────────────────────────────────
type navSnapshotV2 struct {
	timestamp time.Time
	nav       float64
}

// StrategyEventLog is a structured JSON log for Python analysis.
type StrategyEventLog struct {
	Timestamp      string  `json:"timestamp"`
	Strategy       string  `json:"strategy"`
	Mode           string  `json:"mode"`
	SpikeScore     float64 `json:"spike_score"`
	Mid            float64 `json:"mid"`
	FairPrice      float64 `json:"fair_price"`
	InvRatio       float64 `json:"inventory_ratio"`
	IsRebuild      bool    `json:"is_rebuild"`
	Reason         string  `json:"reason"`
	BuyVol30s      float64 `json:"buy_vol_30s"`
	SellVol30s     float64 `json:"sell_vol_30s"`
	Toxicity       int     `json:"toxicity"`
	OFI            float64 `json:"ofi"`
	Drawdown       float64 `json:"drawdown"`
	Sigma          float64 `json:"sigma"`
	Gamma          float64 `json:"gamma"`
	Spread         float64 `json:"spread"`
	InvSkew        float64 `json:"inv_skew"`
	WeightL0       float64 `json:"weight_l0"`
	WeightLast     float64 `json:"weight_last"`
	NumDesired     int     `json:"num_desired"`
	NumCancel      int     `json:"num_cancel"`
	NumCreate      int     `json:"num_create"`
	MicroFillCount int     `json:"micro_fill_count"`
	LatencyMs      float64 `json:"latency_ms"`
	ElapsedMs      float64 `json:"elapsed_ms"`
}

type SpikeMakerModeV2 string

const (
	smModeNormalV2   SpikeMakerModeV2 = "NORMAL"
	smModeWarningV2  SpikeMakerModeV2 = "WARNING"
	smModeRecoveryV2 SpikeMakerModeV2 = "RECOVERY"
	smModePausedV2   SpikeMakerModeV2 = "PAUSED"
)

// rebuildScopeV2 controls which levels convergeV2() may touch.
// If nil, it's a "Full" rebuild.
type rebuildScopeV2 struct {
	side      string  // "BUY", "SELL"
	maxLevels int     // only diff top N inner levels
	mid       float64 // current mid for distance sorting
}

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
	navHistory     []navSnapshotV2
	currentMode    SpikeMakerModeV2
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
	fillHistory      *fillRingBuffer
	lastFillToxicity int
	fillRebuildSide  string
	lastFillAt       time.Time

	// V2: Dynamic requote cooldown
	lastRequoteAt time.Time
	lastSpikeS    float64 // cached spike score for cooldown calc

	// V2: Emergency pause (tox=2)
	emergencyPauseUntil time.Time

	// Max open orders cap (logged once)
	maxOpenOrdersLogged bool

	// V2: Pending-Cancel state tracking
	pendingCancelIds map[string]time.Time

	// V2: Latency Monitoring
	orderSentAt          map[string]time.Time
	lastOrderLatencyMs   float64
	latencySpreadPenalty float64

	// V2: Micro-fill (sub-$2) frequency tracking
	microFillCount30s int

	// V2: Queue Position Tracking (FIFO)
	// OrderID -> SizeAheadAtPrice
	sizeAheadMap map[string]float64

	// Risk
	lastPeakUpdateAt time.Time
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
		cfg.RelistToleranceBps = 25
	}
	if cfg.ComplianceMode == "" {
		cfg.ComplianceMode = "capital"
	}

	return &SpikeMakerV2Strategy{
		cfg:              cfg,
		navHistory:       make([]navSnapshotV2, 0, 1000),
		currentMode:      smModeNormalV2,
		fillHistory:      newFillRingBuffer(500),
		pendingCancelIds: make(map[string]time.Time),
		orderSentAt:      make(map[string]time.Time),
		sizeAheadMap:     make(map[string]float64),
	}
}

type fillRecordV2 struct {
	side           string
	price          float64
	qty            float64
	timestamp      time.Time
	mid            float64
	slippageLogged bool
}

// fillRingBuffer is a circular buffer for fill history to improve efficiency.
type fillRingBuffer struct {
	records []fillRecordV2
	head    int
	size    int
	maxSize int
}

func newFillRingBuffer(maxSize int) *fillRingBuffer {
	return &fillRingBuffer{
		records: make([]fillRecordV2, maxSize),
		maxSize: maxSize,
	}
}

func (r *fillRingBuffer) Add(rec fillRecordV2) {
	r.records[r.head] = rec
	r.head = (r.head + 1) % r.maxSize
	if r.size < r.maxSize {
		r.size++
	}
}

func (r *fillRingBuffer) Get(i int) *fillRecordV2 {
	if i < 0 || i >= r.size {
		return nil
	}
	idx := (r.head - r.size + i + r.maxSize) % r.maxSize
	return &r.records[idx]
}

func (r *fillRingBuffer) Len() int {
	return r.size
}

func (r *fillRingBuffer) Cleanup(cutoff time.Time) {
	for r.size > 0 {
		oldestIdx := (r.head - r.size + r.maxSize) % r.maxSize
		if r.records[oldestIdx].timestamp.Before(cutoff) {
			r.size--
		} else {
			break
		}
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
	start := time.Now()
	snap := input.Snapshot
	balance := input.Balance

	// Validate market data
	mid := snap.Mid
	if mid <= 0 || snap.BestBid <= 0 || snap.BestAsk <= 0 {
		s.logTick(start, "Normal", 0, mid, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, false, "invalid market data", 0, 0, 0, 0)
		return &core.TickOutput{Action: core.TickActionCancelAll, Reason: "invalid market data"}, nil
	}

	// ── V2: Emergency pause check (tox=2 → 5s full stop) ──
	if !s.emergencyPauseUntil.IsZero() && time.Now().Before(s.emergencyPauseUntil) {
		s.logTick(start, "Paused", 0, mid, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, false, "emergency_pause", 0, 0, 0, 0)
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

	if mode == smModePausedV2 {
		s.logTick(start, "Paused", 0, mid, 0, 0, 0, drawdown, 0, 0, 0, 0, 0, 0, false, "drawdown_limit", 0, 0, 0, 0)
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
		s.logTick(start, string(mode), S, mid, 0, 0, 0, drawdown, sigma, 0, 0, 0, 0, 0, true, "cooldown", 0, 0, 0, 0)
		return &core.TickOutput{Action: core.TickActionKeep, Reason: "cooldown"}, nil
	}
	s.lastRequoteAt = time.Now()

	// ── Balance check ──
	avail := balance.TotalBase()*mid + balance.TotalQuote()
	minReq := s.cfg.TargetDepthNotional * 2
	if avail < minReq {
		s.logTick(start, string(mode), S, mid, 0, 0, 0, drawdown, sigma, 0, 0, 0, 0, 0, false, "insufficient balance", 0, 0, 0, 0)
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

	// ── V2: Filter Live Orders by Pending-Cancel state ──
	now := time.Now()
	// Cleanup stale pending cancels (> 15s)
	for id, t := range s.pendingCancelIds {
		if now.Sub(t) > 15*time.Second {
			delete(s.pendingCancelIds, id)
		}
	}
	// Filter live orders
	filteredLive := make([]core.LiveOrder, 0, len(input.LiveOrders))
	for _, lo := range input.LiveOrders {
		if _, pending := s.pendingCancelIds[lo.OrderID]; !pending {
			filteredLive = append(filteredLive, lo)
		}
	}

	// ── V2.2: FIFO Queue Tracking ──
	// Crucial: Update FIFO positions BEFORE calculating queue ratios.
	s.updateQueuePositions(filteredLive, snap)

	// ── V2: Post-fill Slippage Tracking (1s window) ──
	for i := 0; i < s.fillHistory.Len(); i++ {
		f := s.fillHistory.Get(i)
		if f == nil {
			continue
		}
		if !f.slippageLogged && now.Sub(f.timestamp) >= 1*time.Second && now.Sub(f.timestamp) < 5*time.Second {
			slippage := 0.0
			if f.side == "BUY" {
				slippage = (mid - f.price) / f.price
			} else {
				slippage = (f.price - mid) / f.price
			}
			log.Printf("[%s] 📊 1s POST-FILL SLIPPAGE: %s @ %.8f, mid_now=%.8f, slip=%.2f bps",
				s.Name(), f.side, f.price, mid, slippage*10000)
			f.slippageLogged = true
		}
	}

	// ── V2: VWAP Anchor ──
	vwap, vwapCount, vwapTotal := s.computeVWAP(input.RecentTrades)

	// ── V2: Accumulated metrics for Ninja Protection ──
	accBuyVol, accSellVol, accBuyCount, accSellCount, accBuyMicro, accSellMicro := s.computeAccMetrics30s()

	// Best bid/ask sizes from snapshot
	bestBidSize, bestAskSize := 0.0, 0.0
	if len(snap.Bids) > 0 {
		bestBidSize = snap.Bids[0].Qty * snap.Bids[0].Price
	}
	if len(snap.Asks) > 0 {
		bestAskSize = snap.Asks[0].Qty * snap.Asks[0].Price
	}

	// ── Cap levels to exchange max open orders ──
	effectiveLevels := s.cfg.NumLevels
	if snap.MaxOpenOrders > 0 {
		maxPerSide := snap.MaxOpenOrders / 2
		if maxPerSide < effectiveLevels {
			if !s.maxOpenOrdersLogged {
				log.Printf("[%s] ⚠️  Exchange max open orders=%d, capping levels from %d to %d per side",
					s.Name(), snap.MaxOpenOrders, s.cfg.NumLevels, maxPerSide)
				s.maxOpenOrdersLogged = true
			}
			effectiveLevels = maxPerSide
		}
	}

	// ── Generate desired orders via V2 13-step engine ──
	result := generateSpikeAdaptiveOrdersV2(SpikeDepthV2Params{
		BestBid:         snap.BestBid,
		BestAsk:         snap.BestAsk,
		BestBidSize:     bestBidSize,
		BestAskSize:     bestAskSize,
		TickSize:        s.tickSize,
		StepSize:        s.stepSize,
		MinNotional:     s.minNotional,
		MaxOrderQty:     s.maxOrderQty,
		NLevels:         effectiveLevels,
		TotalDepth:      s.cfg.TargetDepthNotional,
		DepthBps:        s.cfg.DepthBps,
		SpreadMinBps:    s.cfg.SpreadMinBps + s.latencySpreadPenalty*10000.0,
		Sigma:           sigma,
		Ret:             s.lastRet,
		NormalizedOFI:   ofi,
		InvRatio:        inv.skew / 0.5,
		SizeMult:        sizeMult,
		VWAP:            vwap,
		AccBuyVol30s:    accBuyVol,
		AccSellVol30s:   accSellVol,
		AccBuyCount30s:  accBuyCount,
		AccSellCount30s: accSellCount,
		AccBuyMicro30s:  accBuyMicro,
		AccSellMicro30s: accSellMicro,
		ComplianceMode:  s.cfg.ComplianceMode,
		LiveOrders:      filteredLive,
		Snapshot:        snap,
		SizeAheadMap:    s.sizeAheadMap,
	})

	desired := result.Orders

	// ── Budget constraint ──
	desired = s.applyBudgetConstraintV2(desired, balance, result.FairPrice)

	// ── Converge with toxicity scope ──
	scope := s.computeRebuildScopeV2(result.FairPrice)
	toCancel, toCreate := s.convergeV2(desired, filteredLive, scope)

	// Update Pending-Cancel tracking
	for _, id := range toCancel {
		s.pendingCancelIds[id] = now
	}

	// ── Log ──
	bidN, askN := 0, 0
	for _, d := range desired {
		if d.Side == "BUY" {
			bidN++
		} else {
			askN++
		}
	}

	log.Printf("[%s] fair=%.8f micro=%.8f mid=%.8f vwap=%.8f(%d/%d) S=%.2f ofi=%.2f inv=%.1f%% mode=%s ninja=(buy$%.1f/%d sell$%.1f/%d) orders=%d (bid=%d ask=%d) cancel=%d create=%d | gamma=%.2f spread=%.0fbps skew=%.3f bidScale=%.2f askScale=%.2f w0=%.3f wN=%.3f",
		s.Name(), result.FairPrice, result.Microprice, result.Mid, vwap, vwapCount, vwapTotal,
		result.SpikeScore, ofi, inv.skew*100,
		mode, accBuyVol, accBuyCount, accSellVol, accSellCount, len(desired), bidN, askN, len(toCancel), len(toCreate),
		result.Gamma, result.Spread*10000, result.InvSkew, result.BidScale, result.AskScale, result.WeightL0, result.WeightLast)

	// ── Log and Return ──
	isRebuild := (scope == nil)
	nMicro := int(accBuyMicro + accSellMicro)

	if len(toCancel) == 0 && len(toCreate) == 0 {
		s.logTick(start, string(mode), result.SpikeScore, mid, result.FairPrice, inv.ratio, ofi, drawdown, sigma, result.Gamma, result.Spread, result.InvSkew, result.WeightL0, result.WeightLast, isRebuild, "ladder_ok", len(desired), 0, 0, nMicro)
		return &core.TickOutput{Action: core.TickActionKeep, Reason: "ladder_ok"}, nil
	}

	// Safety: don't cancel all with no replacements
	liveCount := len(input.LiveOrders)
	if len(toCreate) == 0 && len(toCancel) >= liveCount && liveCount > 0 {
		log.Printf("[%s] WARNING: converge wants to cancel all %d orders with 0 replacements — keeping", s.Name(), liveCount)
		s.logTick(start, string(mode), result.SpikeScore, mid, result.FairPrice, inv.ratio, ofi, drawdown, sigma, result.Gamma, result.Spread, result.InvSkew, result.WeightL0, result.WeightLast, isRebuild, "safety_no_orphan", len(desired), 0, 0, nMicro)
		return &core.TickOutput{Action: core.TickActionKeep, Reason: "safety_no_orphan"}, nil
	}

	s.logTick(start, string(mode), result.SpikeScore, mid, result.FairPrice, inv.ratio, ofi, drawdown, sigma, result.Gamma, result.Spread, result.InvSkew, result.WeightL0, result.WeightLast, isRebuild, "Amend", len(desired), len(toCancel), len(toCreate), nMicro)
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
	case smModeNormalV2:
		return 1000
	case smModeWarningV2:
		return 5000
	case smModeRecoveryV2:
		return 30000
	case smModePausedV2:
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

func (s *SpikeMakerV2Strategy) computeAccMetrics30s() (buyVol, sellVol float64, buyCount, sellCount, buyMicro, sellMicro int) {
	cutoff := time.Now().Add(-30 * time.Second)
	for i := 0; i < s.fillHistory.Len(); i++ {
		f := s.fillHistory.Get(i)
		if f.timestamp.After(cutoff) {
			notional := f.price * f.qty
			isMicro := notional < 2.0

			if f.side == "BUY" {
				buyVol += notional
				buyCount++
				if isMicro {
					buyMicro++
				}
			} else {
				sellVol += notional
				sellCount++
				if isMicro {
					sellMicro++
				}
			}
		}
	}
	s.microFillCount30s = buyMicro + sellMicro
	return
}

// ──────────────────────────────────────────────────────────────────────────────
// V2: VWAP Anchor — compute volume-weighted average price from recent trades
// ──────────────────────────────────────────────────────────────────────────────

const (
	vwapWindowSec   = 180 // 1-minute VWAP window
	vwapMinTrades   = 5   // minimum trades required to use VWAP
	vwapMaxStaleSec = 300 // VWAP becomes stale after 5 minutes
)

func (s *SpikeMakerV2Strategy) computeVWAP(trades []exchange.Trade) (float64, int, int) {
	cutoff := time.Now().Add(-time.Duration(vwapWindowSec) * time.Second)
	var sumPQ, sumQ float64
	var count int

	for i := len(trades) - 1; i >= 0; i-- {
		t := trades[i]
		if t.Timestamp.Before(cutoff) {
			break
		}
		if t.Price > 0 && t.Quantity > 0 {
			// VOLUME FILTER: Exclude tiny trades < $5 to resist micro-spoofing
			if t.Price*t.Quantity < 5.0 {
				continue
			}
			sumPQ += t.Price * t.Quantity
			sumQ += t.Quantity
			count++
		}
	}

	if count < vwapMinTrades || sumQ <= 0 {
		if s.lastVWAP > 0 && time.Since(s.lastVWAPTime).Seconds() < vwapMaxStaleSec {
			return s.lastVWAP, count, len(trades)
		}
		return 0, count, len(trades)
	}

	vwap := sumPQ / sumQ
	s.lastVWAP = vwap
	s.lastVWAPTime = time.Now()
	return vwap, count, len(trades)
}

// ──────────────────────────────────────────────────────────────────────────────
// OFI — Order Flow Imbalance from orderbook
// ──────────────────────────────────────────────────────────────────────────────

func (s *SpikeMakerV2Strategy) computeOFIV2(snap *core.Snapshot) float64 {
	if snap.Mid <= 0 {
		return 0
	}
	// Use 1% (100bps) range for OFI
	depthRange := snap.Mid * 0.01
	var bidDepth, askDepth float64
	for _, lvl := range snap.Bids {
		if lvl.Price >= snap.Mid-depthRange {
			// Distance weight (closer = heavier) to mitigate far-book spoofing
			dist := (snap.Mid - lvl.Price) / depthRange
			weight := 1.0 - dist
			if weight < 0 {
				weight = 0
			}
			bidDepth += lvl.Qty * lvl.Price * weight
		}
	}
	for _, lvl := range snap.Asks {
		if lvl.Price <= snap.Mid+depthRange {
			dist := (lvl.Price - snap.Mid) / depthRange
			weight := 1.0 - dist
			if weight < 0 {
				weight = 0
			}
			askDepth += lvl.Qty * lvl.Price * weight
		}
	}
	total := bidDepth + askDepth
	if total < 1e-9 {
		return 0
	}
	ofi := (bidDepth - askDepth) / total

	// Cap to 0.2 (20% imbalance) to prevent extreme skewing from fake depth
	if ofi > 0.2 {
		ofi = 0.2
	} else if ofi < -0.2 {
		ofi = -0.2
	}
	return ofi
}

// ──────────────────────────────────────────────────────────────────────────────
// OnFill — V2 toxicity scoring with depth % and fill frequency
// ──────────────────────────────────────────────────────────────────────────────

func (s *SpikeMakerV2Strategy) OnFill(event *core.FillEvent) {
	// No-op: all fill handling is done in OnOrderUpdate
}

func (s *SpikeMakerV2Strategy) OnOrderUpdate(event *core.OrderEvent) {
	// Terminal state cleanup for Pending-Cancel tracking
	switch event.Status {
	case "FILLED", "CANCELED", "REJECTED", "EXPIRED":
		delete(s.pendingCancelIds, event.OrderID)

		// Track latency if we have the sent timestamp
		if sentAt, ok := s.orderSentAt[event.OrderID]; ok {
			latency := time.Since(sentAt).Milliseconds()
			s.lastOrderLatencyMs = float64(latency)
			delete(s.orderSentAt, event.OrderID)

			// Update latency-based spread penalty: 1bp per 100ms over 200ms
			if latency > 200 {
				s.latencySpreadPenalty = float64(latency-200) / 100.0 * 0.0001
			} else {
				s.latencySpreadPenalty = 0
			}
		}
	}

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
	s.fillHistory.Add(fillRecordV2{
		side:      event.Side,
		price:     event.Price,
		qty:       fillQty,
		timestamp: now,
		mid:       s.volLastMid,
	})
	s.fillHistory.Cleanup(now.Add(-60 * time.Second))

	// ── V2.2: Micro-Fill Noise Filter ──
	// We ignore rebuild for fills < $2.0 to avoid being "nibbled" by HFT bots.
	if notional < 2.0 {
		log.Printf("[%s] ℹ️ TINY FILL (<$2.0) IGNORED for rebuild: side=%s, price=%.8f, qty=%.4f ($%.2f) order=%s",
			s.Name(), event.Side, event.Price, fillQty, notional, event.OrderID)
		return
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
		for i := 0; i < s.fillHistory.Len(); i++ {
			f := s.fillHistory.Get(i)
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
	for i := 0; i < s.fillHistory.Len(); i++ {
		f := s.fillHistory.Get(i)
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

func (s *SpikeMakerV2Strategy) computeRebuildScopeV2(mid float64) *rebuildScopeV2 {
	if s.lastFillAt.IsZero() || time.Since(s.lastFillAt) > 10*time.Second {
		return nil // full rebuild
	}
	switch s.lastFillToxicity {
	case 0:
		return &rebuildScopeV2{side: s.fillRebuildSide, maxLevels: 2, mid: mid}
	case 1:
		return &rebuildScopeV2{side: s.fillRebuildSide, maxLevels: 3, mid: mid}
	default:
		// tox=2: emergency pause handles this (cancel all in Tick), but if we
		// somehow get here, do a full rebuild
		return nil
	}
}

func (s *SpikeMakerV2Strategy) convergeV2(desired []core.DesiredOrder, live []core.LiveOrder, scope *rebuildScopeV2) (toCancel []string, toCreate []core.DesiredOrder) {
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
		matched := make([]bool, len(desiredSide))

		// Match by LevelIndex + Smart Quoting logic
		softTol := 5.0 / 10000.0
		hardTol := s.cfg.RelistToleranceBps / 10000.0
		if hardTol < softTol {
			hardTol = 10.0 / 10000.0
		}

		for i, d := range desiredSide {
			for j, lo := range liveSide {
				if used[j] {
					continue
				}
				if lo.LevelIndex == d.LevelIndex {
					diff := math.Abs(lo.Price-d.Price) / d.Price

					// Smart Quoting V2:
					// 1. If diff < softTol (5 bps) -> Always keep to preserve queue priority.
					// 2. If diff <= hardTol (10 bps):
					//    a. Keep if quantity is similar (within 10%).
					//    b. OR Keep if we have good queue priority (sizeAhead < 50% of our quantity).
					// 3. Otherwise -> Replace.

					keep := false
					if diff < softTol {
						keep = true
					} else if diff <= hardTol {
						qtyDiff := math.Abs(lo.Qty-d.Qty) / d.Qty
						if qtyDiff < 0.1 {
							keep = true
						} else {
							// Priority check
							if sa, ok := s.sizeAheadMap[lo.OrderID]; ok {
								if sa < lo.Qty*0.5 {
									keep = true
								}
							}
						}
					}

					if keep {
						used[j] = true
						matched[i] = true
					}
					break // only one live per level
				}
			}
		}

		// Unmatched desired → create
		for i, d := range desiredSide {
			if !matched[i] {
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

func (s *SpikeMakerV2Strategy) updateRiskStateV2(nav float64) (float64, SpikeMakerModeV2) {
	now := time.Now()

	s.navHistory = append(s.navHistory, navSnapshotV2{timestamp: now, nav: nav})
	cutoff := now.Add(-24 * time.Hour)
	for len(s.navHistory) > 0 && s.navHistory[0].timestamp.Before(cutoff) {
		s.navHistory = s.navHistory[1:]
	}
	if len(s.navHistory) > 100000 {
		s.navHistory = s.navHistory[len(s.navHistory)-100000:]
	}

	// Update peakNAV and track the last update time
	if nav >= s.peakNAV {
		s.peakNAV = nav
		s.lastPeakUpdateAt = now
	}

	// Stability-based recovery: if no new peak for 1h, allow exiting recovery
	if !s.lastPeakUpdateAt.IsZero() && now.Sub(s.lastPeakUpdateAt) > 1*time.Hour {
		if s.currentMode == smModeRecoveryV2 {
			log.Printf("[%s] 🕒 STABILITY RESET: no new peak for 1h, resetting peakNAV %.2f -> %.2f to exit Recovery mode",
				s.Name(), s.peakNAV, nav)
			s.peakNAV = nav
			s.lastPeakUpdateAt = now
		}
	}

	if s.peakNAV <= 0 {
		s.peakNAV = nav
		s.lastPeakUpdateAt = now
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
		s.currentMode = smModePausedV2
	} else if drawdown >= s.cfg.DrawdownReducePct {
		s.currentMode = smModeRecoveryV2
		if prev != smModeRecoveryV2 {
			s.recoveryTarget = s.peakNAV * (1 - s.cfg.DrawdownReducePct*0.5)
		}
	} else if drawdown >= s.cfg.DrawdownWarnPct {
		s.currentMode = smModeWarningV2
	} else {
		s.currentMode = smModeNormalV2
		if prev == smModeRecoveryV2 && nav >= s.recoveryTarget {
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

func (s *SpikeMakerV2Strategy) calculateSizeMultV2(drawdown float64, mode SpikeMakerModeV2) float64 {
	switch mode {
	case smModeNormalV2:
		return 1.0
	case smModeWarningV2:
		return 0.8 + 0.2*(1-drawdown/s.cfg.DrawdownReducePct)
	case smModeRecoveryV2:
		severity := (drawdown - s.cfg.DrawdownReducePct) / (s.cfg.DrawdownLimitPct - s.cfg.DrawdownReducePct)
		if severity > 1 {
			severity = 1
		}
		mult := 1.0 - severity*(1.0-s.cfg.MaxRecoverySizeMult)
		if mult < s.cfg.MaxRecoverySizeMult {
			mult = s.cfg.MaxRecoverySizeMult
		}
		return mult
	case smModePausedV2:
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

func (s *SpikeMakerV2Strategy) updateQueuePositions(liveOrders []core.LiveOrder, snap *core.Snapshot) {
	// Clean up old orders
	currentIds := make(map[string]bool)
	for _, lo := range liveOrders {
		currentIds[lo.OrderID] = true
	}
	for id := range s.sizeAheadMap {
		if !currentIds[id] {
			delete(s.sizeAheadMap, id)
		}
	}

	// Update existing and init new
	for _, lo := range liveOrders {
		currentSizeAtPrice := 0.0
		if lo.Side == "BUY" {
			for _, b := range snap.Bids {
				if math.Abs(b.Price-lo.Price) < 1e-9 {
					currentSizeAtPrice = b.Qty
					break
				}
			}
		} else {
			for _, a := range snap.Asks {
				if math.Abs(a.Price-lo.Price) < 1e-9 {
					currentSizeAtPrice = a.Qty
					break
				}
			}
		}

		// If new order, init sizeAhead
		if _, ok := s.sizeAheadMap[lo.OrderID]; !ok {
			// We just joined the back of the queue at lo.Price
			// SizeAhead is currentSize - ourQty
			s.sizeAheadMap[lo.OrderID] = math.Max(0, currentSizeAtPrice-lo.RemainingQty)
		} else {
			// Existing order.
			// If currentSize decreased, the decrease is either from fills or cancels ahead of us
			// If currentSize increased, the increase is from orders joined BEHIND us
			// SizeAhead = min(oldSizeAhead, currentSize - ourQty)
			newSizeAhead := math.Max(0, currentSizeAtPrice-lo.RemainingQty)
			if newSizeAhead < s.sizeAheadMap[lo.OrderID] {
				s.sizeAheadMap[lo.OrderID] = newSizeAhead
			}
		}
	}
}

func (s *SpikeMakerV2Strategy) logTick(start time.Time, mode string, S, mid, fair, inv, ofi, drawdown, sigma, gamma, spread, skew, w0, wN float64, isRebuild bool, reason string, nDesired, nCancel, nCreate, nMicro int) {
	elapsed := float64(time.Since(start).Microseconds()) / 1000.0
	accBuyVol, accSellVol, _, _, _, _ := s.computeAccMetrics30s()

	s.logEventJSON(StrategyEventLog{
		Timestamp:      time.Now().UTC().Format(time.RFC3339Nano),
		Strategy:       s.Name(),
		Mode:           mode,
		SpikeScore:     S,
		Mid:            mid,
		FairPrice:      fair,
		InvRatio:       inv,
		IsRebuild:      isRebuild,
		Reason:         reason,
		BuyVol30s:      accBuyVol,
		SellVol30s:     accSellVol,
		Toxicity:       int(s.lastFillToxicity),
		OFI:            ofi,
		Drawdown:       drawdown,
		Sigma:          sigma,
		Gamma:          gamma,
		Spread:         spread,
		InvSkew:        skew,
		WeightL0:       w0,
		WeightLast:     wN,
		NumDesired:     nDesired,
		NumCancel:      nCancel,
		NumCreate:      nCreate,
		MicroFillCount: nMicro,
		LatencyMs:      s.lastOrderLatencyMs,
		ElapsedMs:      elapsed,
	})
}

func (s *SpikeMakerV2Strategy) logEventJSON(logEntry StrategyEventLog) {
	logEntry.Strategy = s.Name()
	jsonData, err := json.Marshal(logEntry)
	if err != nil {
		log.Printf("[%s] ERROR: failed to marshal strategy log: %v", s.Name(), err)
		return
	}
	fmt.Printf("STRATEGY_EVENT: %s\n", string(jsonData))
}
