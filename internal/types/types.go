package types

type MarketData struct {
	BestBid           float64
	BestAsk           float64
	TickSize          float64
	StepSize          float64
	BidMultiplierUp   float64
	AskMultiplierDown float64
	MinNotional       float64
	NowUnixMs         int64
}

// OrderSide represents the side of an order
type OrderSide string

const (
	OrderSideBuy  OrderSide = "BUY"
	OrderSideSell OrderSide = "SELL"
)

type Mode string

const (
	ModePaused Mode = "PAUSED" // Paused mode (no orders)
	ModeNormal Mode = "NORMAL" // Normal operation
	ModeRisk   Mode = "RISK"   // Risk mode (widened spreads, reduced sizes)
)

type Action string

const (
	ActionKeep      Action = "KEEP"       // Keep existing orders
	ActionReplace   Action = "REPLACE"    // Replace existing orders
	ActionCancelAll Action = "CANCEL_ALL" // Cancel all orders
)

type OrderIntent struct {
	Side       OrderSide `json:"side"`        // Order side (BUY/SELL)
	Price      float64   `json:"price"`       // Order price
	Qty        float64   `json:"qty"`         // Order quantity
	LevelIndex int       `json:"level_index"` // Ladder level index
	Tag        string    `json:"tag"`         // Order tag for identification
}

type ReplacePlan struct {
	State  Mode          `json:"state"`  // Current engine state
	Action Action        `json:"action"` // Action to take
	Reason string        `json:"reason"` // Reason for the action
	Orders []OrderIntent `json:"orders"` // New orders to place (if replacing)
}

// BalanceState represents the current balance state
type BalanceState struct {
	BaseFree    float64 `json:"base_free"`    // Available base asset
	QuoteFree   float64 `json:"quote_free"`   // Available quote asset
	BaseLocked  float64 `json:"base_locked"`  // Locked base asset (in orders)
	QuoteLocked float64 `json:"quote_locked"` // Locked quote asset (in orders)
}

// RollingMetrics represents rolling window metrics for market activity
type RollingMetrics struct {
	FillsPerMin      float64 `json:"fills_per_min"`      // Number of fills per minute
	BidFillsNotional float64 `json:"bid_fills_notional"` // Notional value of bid fills
	AskFillsNotional float64 `json:"ask_fills_notional"` // Notional value of ask fills
	HitImbalance     float64 `json:"hit_imbalance"`      // Ratio of bid/ask fills (with epsilon)
	TtfP50Sec        float64 `json:"ttf_p50_sec"`        // Median time-to-fill in seconds
	InvDriftPerHour  float64 `json:"inv_drift_per_hour"` // Inventory drift rate per hour
	CancelPerMin     float64 `json:"cancel_per_min"`     // Number of cancels per minute
}

type EngineState struct {
	LastMid          float64 // Last mid price
	LastInvDev       float64 // Last inventory deviation
	LastSkewBps      float64 // Last skew in basis points
	LastMode         Mode    // Last operating mode
	OldestOrderAgeMs int64   // Age of oldest order in milliseconds
	LastRefreshMs    int64   // Last refresh timestamp in milliseconds
}

// TradingConfigUpdate represents trading config from MongoDB for hot reload
type TradingConfigUpdate struct {
	Type       string `bson:"type"`
	Symbol     string `bson:"symbol"`
	BaseAsset  string `bson:"base_asset"`
	QuoteAsset string `bson:"quote_asset"`

	// Inventory target
	TargetRatio float64 `bson:"target_ratio"`

	// Ladder configuration
	OffsetsBps    []int     `bson:"offsets_bps"`
	SizeMult      []float64 `bson:"size_mult"`
	QuotePerOrder float64   `bson:"quote_per_order"`

	// Skew parameters
	Deadzone           float64 `bson:"deadzone"`
	K                  float64 `bson:"k"`
	MaxSkewBps         int     `bson:"max_skew_bps"`
	MinOffsetBps       int     `bson:"min_offset_bps"`
	DSkewMaxBpsPerTick int     `bson:"d_skew_max_bps_per_tick"`

	// Replace thresholds
	ReplaceThresholds ReplaceThresholds `bson:"replace_thresholds"`

	// Risk thresholds
	RiskThresholds RiskThresholds `bson:"risk_thresholds"`

	// Risk actions
	RiskActions RiskActions `bson:"risk_actions"`

	// Refresh configuration
	RefreshBaseSec   int     `bson:"refresh_base_sec"`
	RefreshJitterPct float64 `bson:"refresh_jitter_pct"`
}

type ReplaceThresholds struct {
	RepriceThresholdBps int     `bson:"reprice_threshold_bps"`
	InvDevThreshold     float64 `bson:"inv_dev_threshold"`
	MaxOrderAgeSec      int     `bson:"max_order_age_sec"`
}

type RiskThresholds struct {
	TtfFastSec       float64 `bson:"ttf_fast_sec"`
	FillSpikePerMin  float64 `bson:"fill_spike_per_min"`
	ImbHigh          float64 `bson:"imb_high"`
	ImbLow           float64 `bson:"imb_low"`
	DriftFastPerHour float64 `bson:"drift_fast_per_hour"`
}

type RiskActions struct {
	RiskSpreadMult  float64 `bson:"risk_spread_mult"`
	RiskSizeMult    float64 `bson:"risk_size_mult"`
	RiskRefreshMult float64 `bson:"risk_refresh_mult"`
}
