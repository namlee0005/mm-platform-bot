package engine

import "time"

// Mode represents the engine operating mode
type Mode string

const (
	ModeNormal    Mode = "NORMAL"
	ModeDefensive Mode = "DEFENSIVE"
	ModeRecovery  Mode = "RECOVERY"
	ModePaused    Mode = "PAUSED"
)

// DiffAction represents what to do with an order in the diff
type DiffAction string

const (
	DiffPlace  DiffAction = "PLACE"
	DiffAmend  DiffAction = "AMEND"
	DiffCancel DiffAction = "CANCEL"
)

// DesiredOrder is a target order the engine wants on the book
type DesiredOrder struct {
	Side       string  // "BUY" or "SELL"
	Price      float64 // target price
	Qty        float64 // target quantity
	LevelIndex int     // ladder level 0..N-1
	Tag        string  // client order ID / tag
}

// LiveOrder is an order currently on the exchange
type LiveOrder struct {
	OrderID       string
	ClientOrderID string
	Side          string
	Price         float64
	Qty           float64
	RemainingQty  float64
	LevelIndex    int
	PlacedAt      time.Time
}

// OrderDiff is a single instruction for the execution engine
type OrderDiff struct {
	Action  DiffAction
	Desired *DesiredOrder // non-nil for PLACE and AMEND
	Live    *LiveOrder    // non-nil for AMEND and CANCEL
	Reason  string        // Human-readable reason for this action
}

// Snapshot is a point-in-time market snapshot
type Snapshot struct {
	BestBid     float64
	BestAsk     float64
	Mid         float64
	TickSize    float64
	StepSize    float64
	MinNotional float64
	Timestamp   time.Time
	// Full book sides for sweep detection
	Bids []PriceLevel // sorted best→worst
	Asks []PriceLevel // sorted best→worst
}

// PriceLevel is a single price level in the order book
type PriceLevel struct {
	Price float64
	Qty   float64
}

// NAVPoint is a point in the NAV time series for drawdown calculation
type NAVPoint struct {
	Timestamp time.Time
	NAV       float64
}

// EngineMetrics is the realtime metrics snapshot emitted each tick
type EngineMetrics struct {
	Timestamp     time.Time `json:"timestamp"`
	Mode          Mode      `json:"mode"`
	Mid           float64   `json:"mid"`
	AvgSpreadBps  float64   `json:"avg_spread_bps"`
	DepthNotional float64   `json:"depth_notional"` // within ±2%
	NumBidOrders  int       `json:"num_bid_orders"`
	NumAskOrders  int       `json:"num_ask_orders"`
	InvRatio      float64   `json:"inv_ratio"`
	InvDeviation  float64   `json:"inv_deviation"`
	SkewBps       float64   `json:"skew_bps"`
	SizeTilt      float64   `json:"size_tilt"`
	Drawdown24h   float64   `json:"drawdown_24h"`
	FillsPerMin   float64   `json:"fills_per_min"`
	CancelPerMin  float64   `json:"cancel_per_min"`
	QuoteUptime   float64   `json:"quote_uptime"` // 0..1
	RealizedVol   float64   `json:"realized_vol"`
	FillRate      float64   `json:"fill_rate"` // fills / quotes
	BaseValue     float64   `json:"base_value"`
	QuoteValue    float64   `json:"quote_value"`
	NAV           float64   `json:"nav"`
}

// DailySummary is emitted once per day
type DailySummary struct {
	Date             string           `json:"date"`
	Symbol           string           `json:"symbol"`
	AvgSpreadBps     float64          `json:"avg_spread_bps"`
	AvgDepthNotional float64          `json:"avg_depth_notional"`
	TotalFills       int              `json:"total_fills"`
	TotalVolume      float64          `json:"total_volume"`
	MaxDrawdown      float64          `json:"max_drawdown"`
	QuoteUptime      float64          `json:"quote_uptime"`
	StartNAV         float64          `json:"start_nav"`
	EndNAV           float64          `json:"end_nav"`
	InvRatioAvg      float64          `json:"inv_ratio_avg"`
	TimeInModes      map[Mode]float64 `json:"time_in_modes"` // seconds per mode
}

// TradeLogEntry is a sanitized fill record for external reporting
// (no strategy internals like skew, mode, ladder level)
type TradeLogEntry struct {
	Timestamp time.Time `json:"timestamp"`
	Symbol    string    `json:"symbol"`
	Side      string    `json:"side"`
	Price     float64   `json:"price"`
	Qty       float64   `json:"qty"`
	Notional  float64   `json:"notional"`
	Fee       float64   `json:"fee"`
	FeeAsset  string    `json:"fee_asset"`
	TradeID   string    `json:"trade_id"`
}
