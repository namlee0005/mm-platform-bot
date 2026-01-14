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
