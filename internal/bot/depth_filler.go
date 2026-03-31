package bot

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
	MinDepthPct         float64 `json:"min_depth_pct"`
	MaxDepthPct         float64 `json:"max_depth_pct"`
	NumLevels           int     `json:"num_levels"`
	TargetDepthNotional float64 `json:"target_depth_notional"`
	TimeSleepMs         int     `json:"time_sleep_ms"`
	RemoveThresholdPct  float64 `json:"remove_threshold_pct"`
	LadderRegenBps      float64 `json:"ladder_regen_bps"`

	// Full balance mode
	UseFullBalance  bool    `json:"use_full_balance"`
	MinOrderSizePct float64 `json:"min_order_size_pct"`
}
