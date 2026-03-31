package bot

import (
	"mm-platform-engine/internal/config"
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

// SpikeMakerV2BotConfig wraps the v2 strategy config with bot-level settings
type SpikeMakerV2BotConfig struct {
	*config.SimpleConfig

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

	// V2: Compliance mode — "capital" (default) or "compliance"
	ComplianceMode string `json:"compliance_mode"`
}
