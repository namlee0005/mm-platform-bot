package bot

import (
	"mm-platform-engine/internal/config"
	"mm-platform-engine/internal/core"
	"mm-platform-engine/internal/exchange"
	"mm-platform-engine/internal/store"
	"mm-platform-engine/internal/strategy"
)

// SpikeMakerBotConfig wraps the strategy config with bot-level settings
type SpikeMakerBotConfig struct {
	*config.SimpleConfig // Embed base config from MongoDB (reuse same config structure)

	// Bot identification
	Exchange       string `json:"exchange"`
	ExchangeID     string `json:"exchange_id"`
	BotID          string `json:"bot_id"`
	BotType        string `json:"bot_type"`
	TickIntervalMs int    `json:"tick_interval_ms"`

	// Risk settings
	DrawdownWarnPct     float64 `json:"drawdown_warn_pct"`
	DrawdownReducePct   float64 `json:"drawdown_reduce_pct"`
	MaxRecoverySizeMult float64 `json:"max_recovery_size_mult"`
}

// NewSpikeMaker creates a new SpikeMaker bot using BaseBot + SpikeMakerStrategy.
func NewSpikeMaker(
	cfg *SpikeMakerBotConfig,
	exch exchange.Exchange,
	redis *store.RedisStore,
	mongo *store.MongoStore,
) *core.BaseBot {
	baseCfg := &core.BaseBotConfig{
		Symbol:               cfg.Symbol,
		BaseAsset:            cfg.BaseAsset,
		QuoteAsset:           cfg.QuoteAsset,
		Exchange:             cfg.Exchange,
		ExchangeID:           cfg.ExchangeID,
		BotID:                cfg.BotID,
		BotType:              cfg.BotType,
		TickIntervalMs:       cfg.TickIntervalMs,
		SyncOrdersIntervalMs: 0, // Sync every tick
	}

	strategyCfg := &strategy.SpikeMakerConfig{
		SimpleConfig:        cfg.SimpleConfig,
		DrawdownWarnPct:     cfg.DrawdownWarnPct,
		DrawdownReducePct:   cfg.DrawdownReducePct,
		MaxRecoverySizeMult: cfg.MaxRecoverySizeMult,
	}

	strat := strategy.NewSpikeMakerStrategy(strategyCfg)

	return core.NewBaseBot(baseCfg, strat, exch, redis, mongo)
}
