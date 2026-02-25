package bot

import (
	"mm-platform-engine/internal/core"
	"mm-platform-engine/internal/exchange"
	"mm-platform-engine/internal/store"
	"mm-platform-engine/internal/strategy"
)

// MMEngineConfig wraps the strategy config with bot-level settings
type MMEngineConfig struct {
	// Bot identification
	Symbol     string `json:"symbol"`
	BaseAsset  string `json:"base_asset"`
	QuoteAsset string `json:"quote_asset"`
	Exchange   string `json:"exchange"`
	ExchangeID string `json:"exchange_id"`
	BotID      string `json:"bot_id"`
	BotType    string `json:"bot_type"`

	// Tick settings
	TickIntervalMs int `json:"tick_interval_ms"`

	// Strategy settings (passed to MMLadderConfig)
	BaseSpreadBps       float64   `json:"base_spread_bps"`
	MinSpreadBps        float64   `json:"min_spread_bps"`
	MaxSpreadBps        float64   `json:"max_spread_bps"`
	VolMultiplierCap    float64   `json:"vol_multiplier_cap"`
	NumLevels           int       `json:"num_levels"`
	OffsetsBps          []int     `json:"offsets_bps"`
	SizeMult            []float64 `json:"size_mult"`
	QuotePerOrder       float64   `json:"quote_per_order"`
	TargetDepthNotional float64   `json:"target_depth_notional"`
	MinOrdersPerSide    int       `json:"min_orders_per_side"`
	TargetRatio         float64   `json:"target_ratio"`
	Deadzone            float64   `json:"deadzone"`
	K                   float64   `json:"k"`
	MaxSkewBps          int       `json:"max_skew_bps"`
	MinOffsetBps        int       `json:"min_offset_bps"`
	DSkewMaxBpsPerTick  int       `json:"d_skew_max_bps_per_tick"`
	SizeTiltCap         float64   `json:"size_tilt_cap"`
	ImbalanceThreshold  float64   `json:"imbalance_threshold"`
	RecoveryTarget      float64   `json:"recovery_target"`

	// Risk settings
	DrawdownLimitPct     float64 `json:"drawdown_limit_pct"`
	DrawdownWarnPct      float64 `json:"drawdown_warn_pct"`
	DrawdownAction       string  `json:"drawdown_action"`
	MaxFillsPerMin       float64 `json:"max_fills_per_min"`
	CooldownAfterFillMs  int     `json:"cooldown_after_fill_ms"`
	DefensiveCooldownSec int     `json:"defensive_cooldown_sec"`

	// Shock settings
	PriceMovePct       float64 `json:"price_move_pct"`
	PriceMoveWindowSec int     `json:"price_move_window_sec"`
	VolSpikeMultiplier float64 `json:"vol_spike_multiplier"`
	SweepDepthPct      float64 `json:"sweep_depth_pct"`
	SweepMinDepth      float64 `json:"sweep_min_depth"`

	// Anti-abuse settings
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

// NewMMEngine creates a new MM Engine bot using BaseBot + MMLadderStrategy.
// This is a thin factory that wires up the components.
func NewMMEngine(
	cfg *MMEngineConfig,
	exch exchange.Exchange,
	redis *store.RedisStore,
	mongo *store.MongoStore,
) *core.BaseBot {
	// Create base bot config
	baseCfg := &core.BaseBotConfig{
		Symbol:         cfg.Symbol,
		BaseAsset:      cfg.BaseAsset,
		QuoteAsset:     cfg.QuoteAsset,
		Exchange:       cfg.Exchange,
		ExchangeID:     cfg.ExchangeID,
		BotID:          cfg.BotID,
		BotType:        cfg.BotType,
		TickIntervalMs: cfg.TickIntervalMs,
	}

	// Create strategy config
	strategyCfg := &strategy.MMLadderConfig{
		BaseSpreadBps:        cfg.BaseSpreadBps,
		MinSpreadBps:         cfg.MinSpreadBps,
		MaxSpreadBps:         cfg.MaxSpreadBps,
		VolMultiplierCap:     cfg.VolMultiplierCap,
		NumLevels:            cfg.NumLevels,
		OffsetsBps:           cfg.OffsetsBps,
		SizeMult:             cfg.SizeMult,
		QuotePerOrder:        cfg.QuotePerOrder,
		TargetDepthNotional:  cfg.TargetDepthNotional,
		MinOrdersPerSide:     cfg.MinOrdersPerSide,
		TargetRatio:          cfg.TargetRatio,
		Deadzone:             cfg.Deadzone,
		K:                    cfg.K,
		MaxSkewBps:           cfg.MaxSkewBps,
		MinOffsetBps:         cfg.MinOffsetBps,
		DSkewMaxBpsPerTick:   cfg.DSkewMaxBpsPerTick,
		SizeTiltCap:          cfg.SizeTiltCap,
		ImbalanceThreshold:   cfg.ImbalanceThreshold,
		RecoveryTarget:       cfg.RecoveryTarget,
		DrawdownLimitPct:     cfg.DrawdownLimitPct,
		DrawdownWarnPct:      cfg.DrawdownWarnPct,
		DrawdownAction:       cfg.DrawdownAction,
		MaxFillsPerMin:       cfg.MaxFillsPerMin,
		CooldownAfterFillMs:  cfg.CooldownAfterFillMs,
		DefensiveCooldownSec: cfg.DefensiveCooldownSec,
		PriceMovePct:         cfg.PriceMovePct,
		PriceMoveWindowSec:   cfg.PriceMoveWindowSec,
		VolSpikeMultiplier:   cfg.VolSpikeMultiplier,
		SweepDepthPct:        cfg.SweepDepthPct,
		SweepMinDepth:        cfg.SweepMinDepth,
		MicroOffsetTicks:     cfg.MicroOffsetTicks,
		QtyJitterPct:         cfg.QtyJitterPct,
		PriceJitterTicks:     cfg.PriceJitterTicks,
		DefensiveSpreadMult:  cfg.DefensiveSpreadMult,
		DefensiveSizeMult:    cfg.DefensiveSizeMult,
		RecoverySpreadMult:   cfg.RecoverySpreadMult,
		RecoverySizeMult:     cfg.RecoverySizeMult,
		PreferOneSided:       cfg.PreferOneSided,
	}

	// Create strategy
	strat := strategy.NewMMLadderStrategy(strategyCfg)

	// Create and return base bot
	return core.NewBaseBot(baseCfg, strat, exch, redis, mongo)
}
