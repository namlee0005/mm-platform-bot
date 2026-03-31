package bot

import (
	"mm-platform-engine/internal/config"
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
	DrawdownWarnPct     float64 `json:"drawdown_warn_pct"`
	DrawdownReducePct   float64 `json:"drawdown_reduce_pct"`
	RecoveryHours       float64 `json:"recovery_hours"`
	MaxRecoverySizeMult float64 `json:"max_recovery_size_mult"`

	// Callbacks
	OnModeChange strategy.ModeChangeCallback `json:"-"`
	OnBalanceLow strategy.BalanceLowCallback `json:"-"`
}
