package bot

import (
	"mm-platform-engine/internal/config"
	"mm-platform-engine/internal/core"
	"mm-platform-engine/internal/exchange"
	"mm-platform-engine/internal/store"
	"mm-platform-engine/internal/strategy"
)

// SimpleMakerConfig wraps the strategy config with bot-level settings
type SimpleMakerConfig struct {
	*config.SimpleConfig // Embed base config from MongoDB

	// Bot identification (not in MongoDB config)
	Exchange       string `json:"exchange"`
	ExchangeID     string `json:"exchange_id"`
	BotID          string `json:"bot_id"`
	BotType        string `json:"bot_type"`
	TickIntervalMs int    `json:"tick_interval_ms"`

	// Strategy-specific settings (not in MongoDB)
	PriceJitterPct      float64 `json:"price_jitter_pct"`
	SizeJitterPct       float64 `json:"size_jitter_pct"`
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

	// Create strategy config - just pass embedded config + strategy-specific fields
	strategyCfg := &strategy.SimpleLadderConfig{
		SimpleConfig:        cfg.SimpleConfig, // Pass embedded MongoDB config directly
		PriceJitterPct:      cfg.PriceJitterPct,
		SizeJitterPct:       cfg.SizeJitterPct,
		DrawdownWarnPct:     cfg.DrawdownWarnPct,
		DrawdownReducePct:   cfg.DrawdownReducePct,
		RecoveryHours:       cfg.RecoveryHours,
		MaxRecoverySizeMult: cfg.MaxRecoverySizeMult,
		DebugCancelSleep:    cfg.DebugCancelSleep,
	}

	// Create strategy
	strat := strategy.NewSimpleLadderStrategy(strategyCfg)

	// Create and return base bot
	return core.NewBaseBot(baseCfg, strat, exch, redis, mongo)
}
