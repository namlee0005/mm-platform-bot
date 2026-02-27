package bot

import (
	"mm-platform-engine/internal/core"
	"mm-platform-engine/internal/exchange"
	"mm-platform-engine/internal/store"
	"mm-platform-engine/internal/strategy"
)

// SimpleMakerConfig wraps the strategy config with bot-level settings
type SimpleMakerConfig struct {
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

	// Strategy settings
	SpreadBps           float64 `json:"spread_bps"`
	NumLevels           int     `json:"num_levels"`
	TargetDepthNotional float64 `json:"target_depth_notional"`
	DepthBps            float64 `json:"depth_bps"`
	PriceJitterPct      float64 `json:"price_jitter_pct"`
	SizeJitterPct       float64 `json:"size_jitter_pct"`
	MinBalanceToTrade   float64 `json:"min_balance_to_trade"`
	LadderRegenBps      float64 `json:"ladder_regen_bps"`
	LevelGapTicksMax    int     `json:"level_gap_ticks_max"`
	TargetRatio         float64 `json:"target_ratio"`
	RatioK              float64 `json:"ratio_k"`

	// Risk settings
	DrawdownLimitPct    float64 `json:"drawdown_limit_pct"`     // Max drawdown before pause (default 5%)
	DrawdownWarnPct     float64 `json:"drawdown_warn_pct"`      // Warning threshold (default 3%)
	DrawdownReducePct   float64 `json:"drawdown_reduce_pct"`    // Start reducing size (default 2%)
	RecoveryHours       float64 `json:"recovery_hours"`         // Target recovery time (default 48h)
	MaxRecoverySizeMult float64 `json:"max_recovery_size_mult"` // Min size during recovery (default 30%)

	// Debug settings
	DebugCancelSleep bool `json:"debug_cancel_sleep"` // Sleep 30s after cancel for WebSocket debug
}

// NewSimpleMaker creates a new SimpleMaker bot using BaseBot + SimpleLadderStrategy.
// This is a thin factory that wires up the components.
func NewSimpleMaker(
	cfg *SimpleMakerConfig,
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
	strategyCfg := &strategy.SimpleLadderConfig{
		SpreadBps:           cfg.SpreadBps,
		NumLevels:           cfg.NumLevels,
		TargetDepthNotional: cfg.TargetDepthNotional,
		DepthBps:            cfg.DepthBps,
		PriceJitterPct:      cfg.PriceJitterPct,
		SizeJitterPct:       cfg.SizeJitterPct,
		MinBalanceToTrade:   cfg.MinBalanceToTrade,
		LadderRegenBps:      cfg.LadderRegenBps,
		LevelGapTicksMax:    cfg.LevelGapTicksMax,
		TargetRatio:         cfg.TargetRatio,
		RatioK:              cfg.RatioK,
		// Risk settings
		DrawdownLimitPct:    cfg.DrawdownLimitPct,
		DrawdownWarnPct:     cfg.DrawdownWarnPct,
		DrawdownReducePct:   cfg.DrawdownReducePct,
		RecoveryHours:       cfg.RecoveryHours,
		MaxRecoverySizeMult: cfg.MaxRecoverySizeMult,
		// Debug settings
		DebugCancelSleep: cfg.DebugCancelSleep,
	}

	// Create strategy
	strat := strategy.NewSimpleLadderStrategy(strategyCfg)

	// Create and return base bot
	return core.NewBaseBot(baseCfg, strat, exch, redis, mongo)
}
