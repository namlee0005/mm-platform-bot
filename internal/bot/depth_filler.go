package bot

import (
	"mm-platform-engine/internal/core"
	"mm-platform-engine/internal/exchange"
	"mm-platform-engine/internal/store"
	"mm-platform-engine/internal/strategy"
)

// DepthFillerConfig wraps the strategy config with bot-level settings
type DepthFillerConfig struct {
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
	MinDepthPct         float64 `json:"min_depth_pct"`         // Min depth % from mid (e.g., 5)
	MaxDepthPct         float64 `json:"max_depth_pct"`         // Max depth % from mid (e.g., 50)
	NumLevels           int     `json:"num_levels"`            // Levels per side
	TargetDepthNotional float64 `json:"target_depth_notional"` // Notional per side
	TimeSleepMs         int     `json:"time_sleep_ms"`         // Sleep between orders
	RemoveThresholdPct  float64 `json:"remove_threshold_pct"`  // Remove when price within %
	LadderRegenBps      float64 `json:"ladder_regen_bps"`      // Regen when mid moves bps
}

// NewDepthFiller creates a new DepthFiller bot
func NewDepthFiller(
	cfg *DepthFillerConfig,
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
	strategyCfg := &strategy.DepthFillerConfig{
		MinDepthPct:         cfg.MinDepthPct,
		MaxDepthPct:         cfg.MaxDepthPct,
		NumLevels:           cfg.NumLevels,
		TargetDepthNotional: cfg.TargetDepthNotional,
		TimeSleepMs:         cfg.TimeSleepMs,
		RemoveThresholdPct:  cfg.RemoveThresholdPct,
		LadderRegenBps:      cfg.LadderRegenBps,
	}

	// Create strategy
	strat := strategy.NewDepthFillerStrategy(strategyCfg)

	// Create and return base bot
	return core.NewBaseBot(baseCfg, strat, exch, redis, mongo)
}
