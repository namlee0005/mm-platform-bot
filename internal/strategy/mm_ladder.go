package strategy

import (
	"context"
	"fmt"
	"log"
	"time"

	"mm-platform-engine/internal/core"
	"mm-platform-engine/internal/modules"
)

// MMLadderConfig is the configuration for MMLadderStrategy
type MMLadderConfig struct {
	// Spread config
	BaseSpreadBps    float64 `json:"base_spread_bps"`
	MinSpreadBps     float64 `json:"min_spread_bps"`
	MaxSpreadBps     float64 `json:"max_spread_bps"`
	VolMultiplierCap float64 `json:"vol_multiplier_cap"`

	// Depth config
	NumLevels           int       `json:"num_levels"`
	OffsetsBps          []int     `json:"offsets_bps"`
	SizeMult            []float64 `json:"size_mult"`
	QuotePerOrder       float64   `json:"quote_per_order"`
	TargetDepthNotional float64   `json:"target_depth_notional"`
	MinOrdersPerSide    int       `json:"min_orders_per_side"`

	// Inventory config
	TargetRatio        float64 `json:"target_ratio"`
	Deadzone           float64 `json:"deadzone"`
	K                  float64 `json:"k"`
	MaxSkewBps         int     `json:"max_skew_bps"`
	MinOffsetBps       int     `json:"min_offset_bps"`
	DSkewMaxBpsPerTick int     `json:"d_skew_max_bps_per_tick"`
	SizeTiltCap        float64 `json:"size_tilt_cap"`
	ImbalanceThreshold float64 `json:"imbalance_threshold"`
	RecoveryTarget     float64 `json:"recovery_target"`

	// Risk config
	DrawdownLimitPct     float64 `json:"drawdown_limit_pct"`
	DrawdownWarnPct      float64 `json:"drawdown_warn_pct"`
	DrawdownAction       string  `json:"drawdown_action"`
	MaxFillsPerMin       float64 `json:"max_fills_per_min"`
	CooldownAfterFillMs  int     `json:"cooldown_after_fill_ms"`
	DefensiveCooldownSec int     `json:"defensive_cooldown_sec"`

	// Shock config
	PriceMovePct       float64 `json:"price_move_pct"`
	PriceMoveWindowSec int     `json:"price_move_window_sec"`
	VolSpikeMultiplier float64 `json:"vol_spike_multiplier"`
	SweepDepthPct      float64 `json:"sweep_depth_pct"`
	SweepMinDepth      float64 `json:"sweep_min_depth"`

	// Anti-abuse config
	MicroOffsetTicks int     `json:"micro_offset_ticks"`
	QtyJitterPct     float64 `json:"qty_jitter_pct"`
	PriceJitterTicks int     `json:"price_jitter_ticks"`

	// Mode overrides
	DefensiveSpreadMult float64 `json:"defensive_spread_mult"`
	DefensiveSizeMult   float64 `json:"defensive_size_mult"`
	RecoverySpreadMult  float64 `json:"recovery_spread_mult"`
	RecoverySizeMult    float64 `json:"recovery_size_mult"`
	PreferOneSided      bool    `json:"prefer_one_sided"`
}

// MMLadderStrategy implements a two-sided market maker strategy
// with full module support (price engine, spread engine, etc.)
type MMLadderStrategy struct {
	cfg *MMLadderConfig

	// Modules
	priceEngine  *modules.PriceEngine
	spreadEngine *modules.SpreadEngine
	invCtrl      *modules.InventoryController
	riskGuard    *modules.RiskGuard
	depthBuilder *modules.DepthBuilder
	stateMachine *modules.StateMachine
	reporter     *modules.Reporter
}

// NewMMLadderStrategy creates a new MMLadderStrategy with default config
func NewMMLadderStrategy(cfg *MMLadderConfig) *MMLadderStrategy {
	// Set defaults
	if cfg.BaseSpreadBps == 0 {
		cfg.BaseSpreadBps = 30
	}
	if cfg.MinSpreadBps == 0 {
		cfg.MinSpreadBps = 10
	}
	if cfg.MaxSpreadBps == 0 {
		cfg.MaxSpreadBps = 300
	}
	if cfg.VolMultiplierCap == 0 {
		cfg.VolMultiplierCap = 3.0
	}
	if cfg.NumLevels == 0 {
		cfg.NumLevels = 5
	}
	if len(cfg.OffsetsBps) == 0 {
		cfg.OffsetsBps = []int{15, 30, 60, 100, 180}
	}
	if len(cfg.SizeMult) == 0 {
		cfg.SizeMult = []float64{1.0, 0.8, 0.6, 0.4, 0.2}
	}
	if cfg.QuotePerOrder == 0 {
		cfg.QuotePerOrder = 50.0
	}
	if cfg.TargetDepthNotional == 0 {
		cfg.TargetDepthNotional = 100000
	}
	if cfg.TargetRatio == 0 {
		cfg.TargetRatio = 0.5
	}
	if cfg.Deadzone == 0 {
		cfg.Deadzone = 0.02
	}
	if cfg.K == 0 {
		cfg.K = 2.0
	}
	if cfg.MaxSkewBps == 0 {
		cfg.MaxSkewBps = 200
	}
	if cfg.MinOffsetBps == 0 {
		cfg.MinOffsetBps = 5
	}
	if cfg.DSkewMaxBpsPerTick == 0 {
		cfg.DSkewMaxBpsPerTick = 50
	}
	if cfg.SizeTiltCap == 0 {
		cfg.SizeTiltCap = 0.3
	}
	if cfg.ImbalanceThreshold == 0 {
		cfg.ImbalanceThreshold = 0.20
	}
	if cfg.RecoveryTarget == 0 {
		cfg.RecoveryTarget = 0.05
	}
	if cfg.DrawdownLimitPct == 0 {
		cfg.DrawdownLimitPct = 0.05
	}
	if cfg.DrawdownWarnPct == 0 {
		cfg.DrawdownWarnPct = 0.03
	}
	if cfg.DrawdownAction == "" {
		cfg.DrawdownAction = "pause"
	}
	if cfg.MaxFillsPerMin == 0 {
		cfg.MaxFillsPerMin = 20
	}
	if cfg.CooldownAfterFillMs == 0 {
		cfg.CooldownAfterFillMs = 200
	}
	if cfg.DefensiveCooldownSec == 0 {
		cfg.DefensiveCooldownSec = 60
	}
	if cfg.PriceMovePct == 0 {
		cfg.PriceMovePct = 2.0
	}
	if cfg.PriceMoveWindowSec == 0 {
		cfg.PriceMoveWindowSec = 10
	}
	if cfg.VolSpikeMultiplier == 0 {
		cfg.VolSpikeMultiplier = 3.0
	}
	if cfg.SweepDepthPct == 0 {
		cfg.SweepDepthPct = 2.0
	}
	if cfg.SweepMinDepth == 0 {
		cfg.SweepMinDepth = 10.0
	}
	if cfg.MicroOffsetTicks == 0 {
		cfg.MicroOffsetTicks = 2
	}
	if cfg.QtyJitterPct == 0 {
		cfg.QtyJitterPct = 0.05
	}
	if cfg.PriceJitterTicks == 0 {
		cfg.PriceJitterTicks = 3
	}
	if cfg.DefensiveSpreadMult == 0 {
		cfg.DefensiveSpreadMult = 2.0
	}
	if cfg.DefensiveSizeMult == 0 {
		cfg.DefensiveSizeMult = 0.5
	}
	if cfg.RecoverySpreadMult == 0 {
		cfg.RecoverySpreadMult = 1.2
	}
	if cfg.RecoverySizeMult == 0 {
		cfg.RecoverySizeMult = 1.0
	}

	// Create modules
	shockCfg := &modules.ShockConfig{
		PriceMovePct:       cfg.PriceMovePct,
		PriceMoveWindowSec: cfg.PriceMoveWindowSec,
		VolSpikeMultiplier: cfg.VolSpikeMultiplier,
		SweepDepthPct:      cfg.SweepDepthPct,
		SweepMinDepth:      cfg.SweepMinDepth,
	}

	invCfg := &modules.InventoryConfig{
		TargetRatio:        cfg.TargetRatio,
		Deadzone:           cfg.Deadzone,
		K:                  cfg.K,
		MaxSkewBps:         cfg.MaxSkewBps,
		MinOffsetBps:       cfg.MinOffsetBps,
		DSkewMaxBpsPerTick: cfg.DSkewMaxBpsPerTick,
		SizeTiltCap:        cfg.SizeTiltCap,
		ImbalanceThreshold: cfg.ImbalanceThreshold,
		RecoveryTarget:     cfg.RecoveryTarget,
	}

	riskCfg := &modules.RiskConfig{
		DrawdownLimitPct:     cfg.DrawdownLimitPct,
		DrawdownWarnPct:      cfg.DrawdownWarnPct,
		DrawdownAction:       cfg.DrawdownAction,
		MaxFillsPerMin:       cfg.MaxFillsPerMin,
		CooldownAfterFillMs:  cfg.CooldownAfterFillMs,
		DefensiveCooldownSec: cfg.DefensiveCooldownSec,
	}

	spreadCfg := modules.SpreadConfig{
		BaseSpreadBps:    cfg.BaseSpreadBps,
		MinSpreadBps:     cfg.MinSpreadBps,
		MaxSpreadBps:     cfg.MaxSpreadBps,
		VolMultiplierCap: cfg.VolMultiplierCap,
	}

	defensiveMult := modules.ModeMultipliers{
		SpreadMult: cfg.DefensiveSpreadMult,
		SizeMult:   cfg.DefensiveSizeMult,
	}
	recoveryMult := modules.ModeMultipliers{
		SpreadMult: cfg.RecoverySpreadMult,
		SizeMult:   cfg.RecoverySizeMult,
	}

	depthCfg := modules.DepthConfig{
		NumLevels:           cfg.NumLevels,
		OffsetsBps:          cfg.OffsetsBps,
		SizeMult:            cfg.SizeMult,
		QuotePerOrder:       cfg.QuotePerOrder,
		TargetDepthNotional: cfg.TargetDepthNotional,
		MinOrdersPerSide:    cfg.MinOrdersPerSide,
	}

	antiAbuse := modules.AntiAbuseConfig{
		MicroOffsetTicks: cfg.MicroOffsetTicks,
		QtyJitterPct:     cfg.QtyJitterPct,
		PriceJitterTicks: cfg.PriceJitterTicks,
	}

	reportingCfg := &modules.ReportingConfig{
		MetricsIntervalSec:  10,
		DailySummaryHourUTC: 0,
	}

	return &MMLadderStrategy{
		cfg:          cfg,
		priceEngine:  modules.NewPriceEngine(shockCfg),
		spreadEngine: modules.NewSpreadEngine(spreadCfg, invCfg, defensiveMult, recoveryMult),
		invCtrl:      modules.NewInventoryController(invCfg),
		riskGuard:    modules.NewRiskGuard(riskCfg),
		depthBuilder: modules.NewDepthBuilder(depthCfg, antiAbuse, invCfg, cfg.DefensiveSizeMult, cfg.RecoverySizeMult, cfg.PreferOneSided),
		stateMachine: modules.NewStateMachine(),
		reporter:     modules.NewReporter(reportingCfg),
	}
}

// Name returns the strategy name
func (s *MMLadderStrategy) Name() string {
	return "MMLadder"
}

// Init initializes the strategy with market state
func (s *MMLadderStrategy) Init(ctx context.Context, snap *core.Snapshot, balance *core.BalanceState) error {
	log.Printf("[%s] Initialized: mid=%.6f, tickSize=%.8f, stepSize=%.8f",
		s.Name(), snap.Mid, snap.TickSize, snap.StepSize)
	return nil
}

// Tick executes one strategy cycle
func (s *MMLadderStrategy) Tick(ctx context.Context, input *core.TickInput) (*core.TickOutput, error) {
	now := time.Now()
	snap := input.Snapshot
	balance := input.Balance

	// Validate market data
	if snap.Mid <= 0 || snap.BestBid >= snap.BestAsk {
		s.stateMachine.ForceMode(core.ModePaused, "invalid market data")
		return &core.TickOutput{
			Action: core.TickActionCancelAll,
			Reason: "invalid market data",
		}, nil
	}

	// Update price engine
	s.priceEngine.Update(snap.Mid, now)
	shockInfo := s.priceEngine.DetectShock()
	if !shockInfo.Detected {
		shockInfo = s.priceEngine.DetectSweep(snap)
	}

	// Calculate inventory
	baseFree := balance.BaseFree
	quoteFree := balance.QuoteFree
	baseLocked := balance.BaseLocked
	quoteLocked := balance.QuoteLocked

	baseValue := (baseFree + baseLocked) * snap.Mid
	quoteValue := quoteFree + quoteLocked
	totalValue := baseValue + quoteValue
	if totalValue < 1e-8 {
		return &core.TickOutput{
			Action: core.TickActionCancelAll,
			Reason: "zero NAV",
		}, nil
	}
	invRatio := baseValue / totalValue
	invDev := invRatio - s.cfg.TargetRatio

	// Record NAV for drawdown tracking
	s.riskGuard.RecordNAV(totalValue, now)

	// Evaluate state machine
	mode, _ := s.stateMachine.Evaluate(shockInfo, invDev, s.riskGuard, s.invCtrl, s.priceEngine)

	// If PAUSED, cancel all orders
	if mode == core.ModePaused {
		return &core.TickOutput{
			Action:  core.TickActionCancelAll,
			NewMode: mode,
			Reason:  "paused mode",
		}, nil
	}

	// Check fill cooldown
	if s.riskGuard.IsInFillCooldown() {
		return &core.TickOutput{
			Action:  core.TickActionKeep,
			NewMode: mode,
			Reason:  "fill cooldown",
		}, nil
	}

	// Compute skew and tilt
	skewBps := s.invCtrl.ComputeSkew(invDev)
	sizeTilt := s.invCtrl.ComputeSizeTilt(invDev, mode)

	// Compute spread
	volMult := s.priceEngine.VolMultiplier(s.cfg.VolMultiplierCap)
	spreadBps := s.spreadEngine.ComputeSpread(volMult, invDev, mode)

	// Apply drawdown-based size reduction
	drawdownMult := 1.0
	if s.cfg.DrawdownAction == "reduce" {
		drawdownMult = s.riskGuard.DrawdownReduceMultiplier()
	}

	// Build desired ladder
	desired := s.depthBuilder.BuildLadder(snap, spreadBps, skewBps, sizeTilt, mode, invDev, now.UnixMilli())

	// Apply drawdown multiplier
	if drawdownMult < 1.0 {
		for i := range desired {
			desired[i].Qty *= drawdownMult
		}
	}

	// Enforce depth KPI
	desired = s.depthBuilder.EnforceDepthKPI(desired, snap)

	// Apply budget constraints
	baseTotal := baseFree + baseLocked
	quoteTotal := quoteFree + quoteLocked
	desired = s.depthBuilder.ApplyBudgetConstraints(desired, baseTotal, quoteTotal)

	log.Printf("[%s] mode=%s mid=%.6f spread=%.1fbps skew=%.1fbps tilt=%.2f inv=%.2f%% dd=%.2f%% orders=%d",
		s.Name(), mode, snap.Mid, spreadBps, skewBps, sizeTilt,
		invRatio*100, s.riskGuard.Drawdown24h()*100, len(desired))

	return &core.TickOutput{
		Action:        core.TickActionReplace,
		DesiredOrders: desired,
		NewMode:       mode,
		Reason:        "tick cycle",
		Metrics: map[string]float64{
			"mid":       snap.Mid,
			"spread":    spreadBps,
			"skew":      skewBps,
			"inv_ratio": invRatio,
			"drawdown":  s.riskGuard.Drawdown24h(),
		},
	}, nil
}

// OnFill handles fill events
func (s *MMLadderStrategy) OnFill(event *core.FillEvent) {
	s.riskGuard.RecordFill(event.Timestamp)

	s.reporter.RecordTrade(modules.TradeLogEntry{
		Timestamp: event.Timestamp,
		Symbol:    event.Symbol,
		Side:      event.Side,
		Price:     event.Price,
		Qty:       event.Quantity,
		Notional:  event.Price * event.Quantity,
		Fee:       event.Commission,
		FeeAsset:  event.CommissionAsset,
		TradeID:   event.TradeID,
	})

	if s.riskGuard.IsFillRateExceeded() {
		s.reporter.Alert("WARNING", fmt.Sprintf(
			"Fill rate exceeded: %.0f/min (limit: %.0f)",
			s.riskGuard.FillsPerMin(), s.cfg.MaxFillsPerMin))
	}
}

// OnOrderUpdate handles order status updates
func (s *MMLadderStrategy) OnOrderUpdate(event *core.OrderEvent) {
	// Log order updates
	log.Printf("[%s] Order %s: %s @ %.8f status=%s",
		s.Name(), event.OrderID, event.Side, event.Price, event.Status)
}

// UpdateConfig updates strategy config at runtime
func (s *MMLadderStrategy) UpdateConfig(newCfg interface{}) error {
	// TODO: implement config update for MM strategy
	log.Printf("[%s] Config update not yet implemented", s.Name())
	return nil
}

// GetMode returns current operating mode
func (s *MMLadderStrategy) GetMode() core.Mode {
	return s.stateMachine.CurrentMode()
}

// GetMetrics returns recent metrics
func (s *MMLadderStrategy) GetMetrics(n int) []modules.EngineMetrics {
	return s.reporter.GetRecentMetrics(n)
}

// GetTradeLog returns sanitized trade log
func (s *MMLadderStrategy) GetTradeLog(limit int) []modules.TradeLogEntry {
	return s.reporter.GetTradeLog(limit)
}

// ForceMode allows manual mode override
func (s *MMLadderStrategy) ForceMode(mode core.Mode, reason string) {
	s.stateMachine.ForceMode(mode, reason)
}
