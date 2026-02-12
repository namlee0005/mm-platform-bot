package engine

// Config is the full engine configuration, loadable from JSON / MongoDB
type Config struct {
	Symbol     string `json:"symbol" bson:"symbol"`
	BaseAsset  string `json:"base_asset" bson:"base_asset"`
	QuoteAsset string `json:"quote_asset" bson:"quote_asset"`

	Spread        SpreadConfig    `json:"spread" bson:"spread"`
	Depth         DepthConfig     `json:"depth" bson:"depth"`
	Inventory     InventoryConfig `json:"inventory" bson:"inventory"`
	Risk          RiskConfig      `json:"risk" bson:"risk"`
	Shock         ShockConfig     `json:"shock" bson:"shock"`
	Execution     ExecutionConfig `json:"execution" bson:"execution"`
	AntiAbuse     AntiAbuseConfig `json:"anti_abuse" bson:"anti_abuse"`
	Reporting     ReportingConfig `json:"reporting" bson:"reporting"`
	ModeOverrides ModeOverrides   `json:"mode_overrides" bson:"mode_overrides"`
}

type SpreadConfig struct {
	BaseSpreadBps    float64 `json:"base_spread_bps" bson:"base_spread_bps"`
	MinSpreadBps     float64 `json:"min_spread_bps" bson:"min_spread_bps"`
	MaxSpreadBps     float64 `json:"max_spread_bps" bson:"max_spread_bps"`
	VolMultiplierCap float64 `json:"vol_multiplier_cap" bson:"vol_multiplier_cap"`
}

type DepthConfig struct {
	NumLevels           int       `json:"num_levels" bson:"num_levels"`
	OffsetsBps          []int     `json:"offsets_bps" bson:"offsets_bps"`
	SizeMult            []float64 `json:"size_mult" bson:"size_mult"`
	QuotePerOrder       float64   `json:"quote_per_order" bson:"quote_per_order"`
	TargetDepthNotional float64   `json:"target_depth_notional" bson:"target_depth_notional"`
	MinOrdersPerSide    int       `json:"min_orders_per_side" bson:"min_orders_per_side"`
}

type InventoryConfig struct {
	TargetRatio        float64 `json:"target_ratio" bson:"target_ratio"`
	Deadzone           float64 `json:"deadzone" bson:"deadzone"`
	K                  float64 `json:"k" bson:"k"`
	MaxSkewBps         int     `json:"max_skew_bps" bson:"max_skew_bps"`
	MinOffsetBps       int     `json:"min_offset_bps" bson:"min_offset_bps"`
	DSkewMaxBpsPerTick int     `json:"d_skew_max_bps_per_tick" bson:"d_skew_max_bps_per_tick"`
	SizeTiltCap        float64 `json:"size_tilt_cap" bson:"size_tilt_cap"`
	ImbalanceThreshold float64 `json:"imbalance_threshold" bson:"imbalance_threshold"`
	RecoveryTarget     float64 `json:"recovery_target" bson:"recovery_target"`
}

type RiskConfig struct {
	DrawdownLimitPct     float64 `json:"drawdown_limit_pct" bson:"drawdown_limit_pct"`
	DrawdownWarnPct      float64 `json:"drawdown_warn_pct" bson:"drawdown_warn_pct"`
	DrawdownAction       string  `json:"drawdown_action" bson:"drawdown_action"` // "reduce" or "pause"
	MaxFillsPerMin       float64 `json:"max_fills_per_min" bson:"max_fills_per_min"`
	MinOrderLifetimeMs   int     `json:"min_order_lifetime_ms" bson:"min_order_lifetime_ms"`
	CooldownAfterFillMs  int     `json:"cooldown_after_fill_ms" bson:"cooldown_after_fill_ms"`
	DefensiveCooldownSec int     `json:"defensive_cooldown_sec" bson:"defensive_cooldown_sec"`
}

type ShockConfig struct {
	PriceMovePct       float64 `json:"price_move_pct" bson:"price_move_pct"`
	PriceMoveWindowSec int     `json:"price_move_window_sec" bson:"price_move_window_sec"`
	VolSpikeMultiplier float64 `json:"vol_spike_multiplier" bson:"vol_spike_multiplier"`
	SweepDepthPct      float64 `json:"sweep_depth_pct" bson:"sweep_depth_pct"`
	SweepMinDepth      float64 `json:"sweep_min_depth" bson:"sweep_min_depth"` // Min depth ($) to not trigger sweep, default 10
}

type ExecutionConfig struct {
	RefreshBaseSec        int     `json:"refresh_base_sec" bson:"refresh_base_sec"`
	RefreshJitterPct      float64 `json:"refresh_jitter_pct" bson:"refresh_jitter_pct"`
	RepriceThresholdBps   int     `json:"reprice_threshold_bps" bson:"reprice_threshold_bps"`
	InvDevThreshold       float64 `json:"inv_dev_threshold" bson:"inv_dev_threshold"`
	MaxOrderAgeSec        int     `json:"max_order_age_sec" bson:"max_order_age_sec"`
	PriceToleranceBps     int     `json:"price_tolerance_bps" bson:"price_tolerance_bps"`
	QtyTolerancePct       float64 `json:"qty_tolerance_pct" bson:"qty_tolerance_pct"`
	TickIntervalMs        int     `json:"tick_interval_ms" bson:"tick_interval_ms"`
	RateLimitOrdersPerSec int     `json:"rate_limit_orders_per_sec" bson:"rate_limit_orders_per_sec"`
	BatchSize             int     `json:"batch_size" bson:"batch_size"`
}

type AntiAbuseConfig struct {
	MicroOffsetTicks int     `json:"micro_offset_ticks" bson:"micro_offset_ticks"`
	QtyJitterPct     float64 `json:"qty_jitter_pct" bson:"qty_jitter_pct"`
	PriceJitterTicks int     `json:"price_jitter_ticks" bson:"price_jitter_ticks"`
	FillCooldownMs   int     `json:"fill_cooldown_ms" bson:"fill_cooldown_ms"`
	MaxFillsPerMin   int     `json:"max_fills_per_min" bson:"max_fills_per_min"`
}

type ReportingConfig struct {
	MetricsIntervalSec  int    `json:"metrics_interval_sec" bson:"metrics_interval_sec"`
	DailySummaryHourUTC int    `json:"daily_summary_hour_utc" bson:"daily_summary_hour_utc"`
	WeeklySummaryDay    string `json:"weekly_summary_day" bson:"weekly_summary_day"`
}

type ModeOverrides struct {
	Defensive ModeMultipliers `json:"defensive" bson:"defensive"`
	Recovery  RecoveryConfig  `json:"recovery" bson:"recovery"`
}

type ModeMultipliers struct {
	SpreadMult  float64 `json:"spread_mult" bson:"spread_mult"`
	SizeMult    float64 `json:"size_mult" bson:"size_mult"`
	RefreshMult float64 `json:"refresh_mult" bson:"refresh_mult"`
}

type RecoveryConfig struct {
	SpreadMult     float64 `json:"spread_mult" bson:"spread_mult"`
	SizeMult       float64 `json:"size_mult" bson:"size_mult"`
	PreferOneSided bool    `json:"prefer_one_sided" bson:"prefer_one_sided"`
}

// DefaultConfig returns a config with sensible defaults
func DefaultConfig() *Config {
	return &Config{
		Spread: SpreadConfig{
			BaseSpreadBps:    30,
			MinSpreadBps:     10,
			MaxSpreadBps:     300,
			VolMultiplierCap: 3.0,
		},
		Depth: DepthConfig{
			NumLevels:           5,
			OffsetsBps:          []int{15, 30, 60, 100, 180},
			SizeMult:            []float64{1.0, 0.8, 0.6, 0.4, 0.2},
			QuotePerOrder:       50.0,
			TargetDepthNotional: 100000,
			MinOrdersPerSide:    20,
		},
		Inventory: InventoryConfig{
			TargetRatio:        0.5,
			Deadzone:           0.02,
			K:                  2.0,
			MaxSkewBps:         200,
			MinOffsetBps:       5,
			DSkewMaxBpsPerTick: 50,
			SizeTiltCap:        0.3,
			ImbalanceThreshold: 0.20,
			RecoveryTarget:     0.05,
		},
		Risk: RiskConfig{
			DrawdownLimitPct:     0.05,
			DrawdownWarnPct:      0.03,
			DrawdownAction:       "pause",
			MaxFillsPerMin:       20,
			MinOrderLifetimeMs:   500,
			CooldownAfterFillMs:  200,
			DefensiveCooldownSec: 60,
		},
		Shock: ShockConfig{
			PriceMovePct:       2.0,
			PriceMoveWindowSec: 10,
			VolSpikeMultiplier: 3.0,
			SweepDepthPct:      2.0,  // 2% range from mid to check depth
			SweepMinDepth:      10.0, // $10 minimum depth for low-cap tokens
		},
		Execution: ExecutionConfig{
			RefreshBaseSec:        60,
			RefreshJitterPct:      0.15,
			RepriceThresholdBps:   30,
			InvDevThreshold:       0.05,
			MaxOrderAgeSec:        120,
			PriceToleranceBps:     30,   // 30 bps tolerance to avoid excessive amends
			QtyTolerancePct:       0.10, // 10% qty tolerance
			TickIntervalMs:        5000,
			RateLimitOrdersPerSec: 10,
			BatchSize:             20,
		},
		AntiAbuse: AntiAbuseConfig{
			MicroOffsetTicks: 2,
			QtyJitterPct:     0.05,
			PriceJitterTicks: 3,
			FillCooldownMs:   300,
			MaxFillsPerMin:   30,
		},
		Reporting: ReportingConfig{
			MetricsIntervalSec:  10,
			DailySummaryHourUTC: 0,
			WeeklySummaryDay:    "Mon",
		},
		ModeOverrides: ModeOverrides{
			Defensive: ModeMultipliers{
				SpreadMult:  2.0,
				SizeMult:    0.5,
				RefreshMult: 0.5,
			},
			Recovery: RecoveryConfig{
				SpreadMult:     1.2,
				SizeMult:       1.0,
				PreferOneSided: true,
			},
		},
	}
}
