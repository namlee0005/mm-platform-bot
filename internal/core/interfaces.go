package core

import (
	"context"
	"time"

	"mm-platform-engine/internal/exchange"
)

// TickAction defines what action the strategy wants after computing a tick
type TickAction string

const (
	TickActionKeep      TickAction = "KEEP"       // Keep existing orders
	TickActionReplace   TickAction = "REPLACE"    // Replace existing orders with new ones (cancel all + place all)
	TickActionAmend     TickAction = "AMEND"      // Incremental update (cancel some + add some)
	TickActionCancelAll TickAction = "CANCEL_ALL" // Cancel all orders (pause mode)
)

// Mode represents the bot operating mode
type Mode string

const (
	ModeNormal    Mode = "NORMAL"
	ModeDefensive Mode = "DEFENSIVE"
	ModeRecovery  Mode = "RECOVERY"
	ModePaused    Mode = "PAUSED"
)

// Strategy interface - each bot implements its own tick logic
type Strategy interface {
	// Name returns the strategy name for logging
	Name() string

	// Init initializes the strategy with initial market state
	Init(ctx context.Context, snap *Snapshot, balance *BalanceState) error

	// Tick executes one strategy cycle and returns desired actions
	Tick(ctx context.Context, input *TickInput) (*TickOutput, error)

	// OnFill handles fill events from WebSocket
	OnFill(event *FillEvent)

	// OnOrderUpdate handles order update events from WebSocket
	OnOrderUpdate(event *OrderEvent)

	// UpdateConfig updates strategy config at runtime (hot-reload)
	UpdateConfig(newCfg interface{}) error

	// UpdatePrevSnapshot saves post-execution state for fill detection next cycle.
	// Called by bot after syncLiveOrders + getBalanceState following order execution.
	UpdatePrevSnapshot(liveOrders []LiveOrder, balance *BalanceState)
}

// TickInput contains all data needed for a strategy tick
type TickInput struct {
	Snapshot     *Snapshot
	Balance      *BalanceState // Current bot's balance
	LiveOrders   []LiveOrder
	Timestamp    int64
	Mode         Mode             // Current operating mode
	RecentTrades []exchange.Trade // Recent market trades (for VWAP fair price calculation)
}

// TickOutput contains the result of a strategy tick
type TickOutput struct {
	Action         TickAction
	DesiredOrders  []DesiredOrder     // For REPLACE action: all desired orders
	OrdersToAdd    []DesiredOrder     // For AMEND action: orders to add
	OrdersToCancel []string           // For AMEND action: order IDs to cancel
	Metrics        map[string]float64 // Optional metrics for reporting
	NewMode        Mode               // If strategy wants to change mode
	Reason         string             // Reason for the action
}

// Snapshot is a point-in-time market data snapshot
type Snapshot struct {
	BestBid     float64
	BestAsk     float64
	Mid         float64
	TickSize    float64
	StepSize    float64
	MinNotional float64
	MaxOrderQty float64 // Maximum quantity per single order (exchange limit)
	Timestamp   time.Time
	// Full orderbook for sweep detection
	Bids []PriceLevel // sorted best→worst
	Asks []PriceLevel // sorted best→worst
}

// PriceLevel is a single price level in the order book
type PriceLevel struct {
	Price float64
	Qty   float64
}

// BalanceState contains current balance information
type BalanceState struct {
	BaseFree    float64
	QuoteFree   float64
	BaseLocked  float64
	QuoteLocked float64
}

// TotalBase returns total base asset (free + locked)
func (b *BalanceState) TotalBase() float64 {
	return b.BaseFree + b.BaseLocked
}

// TotalQuote returns total quote asset (free + locked)
func (b *BalanceState) TotalQuote() float64 {
	return b.QuoteFree + b.QuoteLocked
}

// LiveOrder represents an order currently on the exchange
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

// DesiredOrder represents an order the strategy wants to place
type DesiredOrder struct {
	Side       string // "BUY" or "SELL"
	Price      float64
	Qty        float64
	LevelIndex int
	Tag        string // client order ID / tag
}

// FillEvent represents a trade execution event
type FillEvent struct {
	OrderID         string
	ClientOrderID   string
	Symbol          string
	Side            string // BUY or SELL
	Price           float64
	Quantity        float64
	Commission      float64
	CommissionAsset string
	TradeID         string
	Timestamp       time.Time
}

// OrderEvent represents an order state change event
type OrderEvent struct {
	OrderID       string
	ClientOrderID string
	Symbol        string
	Side          string
	Status        string // NEW, FILLED, CANCELED, etc
	Price         float64
	Quantity      float64
	ExecutedQty   float64
	Timestamp     time.Time
}

// OrderEventType represents the type of order event for broadcasting
type OrderEventType string

const (
	OrderEventTypePlace       OrderEventType = "place"
	OrderEventTypeCancel      OrderEventType = "cancel"
	OrderEventTypeAmend       OrderEventType = "amend"
	OrderEventTypeFill        OrderEventType = "fill"
	OrderEventTypePartialFill OrderEventType = "partial_fill"
)

// BotOrderEvent represents an order event for broadcasting to external systems
type BotOrderEvent struct {
	Type      OrderEventType
	Symbol    string
	OrderID   string
	Side      string
	Price     float64
	Qty       float64
	Level     int
	Reason    string
	Timestamp int64
}

// OrderEventCallback is called when an order event occurs
type OrderEventCallback func(event BotOrderEvent)

// DiffAction represents what to do with an order in the diff
type DiffAction string

const (
	DiffPlace  DiffAction = "PLACE"
	DiffAmend  DiffAction = "AMEND"
	DiffCancel DiffAction = "CANCEL"
)

// OrderDiff is a single instruction for the execution engine
type OrderDiff struct {
	Action  DiffAction
	Desired *DesiredOrder // non-nil for PLACE and AMEND
	Live    *LiveOrder    // non-nil for AMEND and CANCEL
	Reason  string        // Human-readable reason for this action
}

// BaseBotConfig contains configuration for BaseBot
type BaseBotConfig struct {
	Symbol         string
	BaseAsset      string
	QuoteAsset     string
	Exchange       string // exchange name (mexc, gate, etc.)
	ExchangeID     string // exchange ObjectID for finding partner
	BotID          string // unique bot instance ID
	BotType        string // bot type identifier
	TickIntervalMs int    // tick interval in milliseconds

	// Rate limiting
	RateLimitOrdersPerSec int
	BatchSize             int

	// Config check interval (in ticks)
	ConfigCheckInterval int

	// Price source
	UseLastTradePrice bool // Use last trade price instead of mid (bid+ask)/2

	// Order sync interval (ms). 0 = sync every tick, >0 = sync every N ms
	// Use 0 for strategies with few orders (simple_ladder ~6 orders)
	// Use 300000 (5min) for strategies with many orders (depth_filler ~50 orders)
	SyncOrdersIntervalMs int
}

// FillNotifier sends notifications when orders are filled.
// Implemented by notify.TelegramNotifier.
type FillNotifier interface {
	NotifyFill(side string, price, qty, notional float64, isFull bool)
}

// ComputeInventory computes inventory metrics based on balance and mid price
func (b *BalanceState) ComputeInventory(mid float64, targetRatio float64) (invRatio, invDev float64) {
	baseValue := b.TotalBase() * mid
	quoteValue := b.TotalQuote()
	totalValue := baseValue + quoteValue

	if totalValue < 1e-8 {
		return 0, 0
	}

	invRatio = baseValue / totalValue
	invDev = invRatio - targetRatio
	return
}

// NAV returns net asset value in quote currency
func (b *BalanceState) NAV(mid float64) float64 {
	return b.TotalBase()*mid + b.TotalQuote()
}

// BaseValue returns base asset value in quote currency
func (b *BalanceState) BaseValue(mid float64) float64 {
	return b.TotalBase() * mid
}
