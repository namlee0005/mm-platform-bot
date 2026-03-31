package engine

// EngineConfig contains all tunable parameters for the event-driven engine.
type EngineConfig struct {
	// Symbol and assets
	Symbol     string
	BaseAsset  string
	QuoteAsset string
	Exchange   string
	ExchangeID string
	BotID      string
	BotType    string

	// Coalescing — controls how rapidly incoming events are batched
	// before triggering a strategy recalc.
	CoalesceWindowMs int // Wait this long after first event before recalc (default: 50)
	MaxCoalesceMs    int // Never delay recalc more than this (default: 200)

	// Reprice threshold — only recalc on orderbook update if mid moved
	// more than this many basis points since last recalc.
	RepriceThresholdBps int // default: 5 (= 0.05%)

	// Backstop — safety-net timer that forces a recalc even if no events arrive.
	// Catches silent WS stalls and periodic housekeeping.
	BackstopIntervalMs int // default: 30000 (30s)

	// EventBus buffer capacity
	BusCapacity int // default: 256

	// Price source
	UseLastTradePrice bool // Use last trade price instead of mid (bid+ask)/2

	// Execution
	RateLimitOrdersPerSec int // default: 10
	BatchSize             int // default: 20

	// Order diff tolerances
	PriceToleranceBps  int     // default: 2
	QtyTolerancePct    float64 // default: 0.05
	MinOrderLifetimeMs int     // default: 1000

	// Config hot-reload interval (ms). Checks MongoDB for config changes.
	ConfigCheckIntervalMs int // default: 10000

	// REST fallback polling interval when WS is disconnected.
	FallbackPollIntervalMs int // default: 3000

	// REST reconcile interval — periodic sync of balance + orders from REST
	// even when WS is healthy. Catches WS drift, missed events, phantom orders.
	ReconcileIntervalMs int // default: 60000 (1 minute)

	// TimerOnlyMode disables event-driven recalc. When true, strategy.Tick()
	// is only called by the backstop timer (BackstopIntervalMs), not on WS events.
	// Events are still applied for state tracking (balance, orders, orderbook).
	TimerOnlyMode bool
}

// ApplyDefaults fills zero-value fields with defaults.
func (c *EngineConfig) ApplyDefaults() {
	if c.CoalesceWindowMs == 0 {
		c.CoalesceWindowMs = 50
	}
	if c.MaxCoalesceMs == 0 {
		c.MaxCoalesceMs = 200
	}
	if c.RepriceThresholdBps == 0 {
		c.RepriceThresholdBps = 5
	}
	if c.BackstopIntervalMs == 0 {
		c.BackstopIntervalMs = 30000
	}
	if c.BusCapacity == 0 {
		c.BusCapacity = 256
	}
	if c.RateLimitOrdersPerSec == 0 {
		c.RateLimitOrdersPerSec = 10
	}
	if c.BatchSize == 0 {
		c.BatchSize = 20
	}
	if c.PriceToleranceBps == 0 {
		c.PriceToleranceBps = 2
	}
	if c.QtyTolerancePct == 0 {
		c.QtyTolerancePct = 0.05
	}
	if c.MinOrderLifetimeMs == 0 {
		c.MinOrderLifetimeMs = 1000
	}
	if c.ConfigCheckIntervalMs == 0 {
		c.ConfigCheckIntervalMs = 10000
	}
	if c.FallbackPollIntervalMs == 0 {
		c.FallbackPollIntervalMs = 3000
	}
	if c.ReconcileIntervalMs == 0 {
		c.ReconcileIntervalMs = 60000
	}
}
