package strategy

import (
	"sync/atomic"
	"time"

	"github.com/shopspring/decimal"
)

// VolatilityRegime classifies current market conditions.
type VolatilityRegime int

const (
	RegimeSideways VolatilityRegime = iota
	RegimeSpike
)

func (r VolatilityRegime) String() string {
	switch r {
	case RegimeSideways:
		return "Sideways"
	case RegimeSpike:
		return "Spike"
	default:
		return "Unknown"
	}
}

// Side is Bid or Ask.
type Side int

const (
	Bid Side = iota
	Ask
)

// DepthLevel is the quote ladder level (L0 = tightest, L9 = widest).
type DepthLevel int

const (
	L0 DepthLevel = iota
	L1
	L2
	L3
	L4
	L5
	L6
	L7
	L8
	L9
)

// OrderBook is a minimal snapshot of the CLOB for one instrument.
type OrderBook struct {
	BestBid   decimal.Decimal
	BestAsk   decimal.Decimal
	BidQty    decimal.Decimal
	AskQty    decimal.Decimal
	Timestamp time.Time
}

// Trade is a single aggressor trade from the CLOB tape.
type Trade struct {
	Price     decimal.Decimal
	Qty       decimal.Decimal
	Side      Side
	Timestamp time.Time
}

// Fill is a confirmed fill against one of our resting orders.
type Fill struct {
	OrderID   string
	Price     decimal.Decimal
	Qty       decimal.Decimal
	Side      Side
	Timestamp time.Time
}

// HistoricalTick is the immutable data contract for backtest and live runners.
// Schema is locked here — do not add fields without updating both consumers.
type HistoricalTick struct {
	Timestamp time.Time
	Book      OrderBook
	Trades    []Trade
	Fills     []Fill
}

// FairValueConfig controls fair-value computation parameters.
type FairValueConfig struct {
	OFIAlpha decimal.Decimal `yaml:"ofi_alpha"`
	TickSize decimal.Decimal `yaml:"tick_size"`
}

// VolatilityConfig controls EWMA and regime FSM parameters.
type VolatilityConfig struct {
	EWMAHalflifeSeconds float64 `yaml:"ewma_halflife_seconds"`
	VolThreshold        float64 `yaml:"vol_threshold"`
	VolExitThreshold    float64 `yaml:"vol_exit_threshold"`
	UseEWMA             bool    `yaml:"vol_use_ewma"`
}

// InventoryConfig controls position sizing and skew behaviour.
type InventoryConfig struct {
	MaxInventory decimal.Decimal `yaml:"max_inventory"`
	EmergencyCap decimal.Decimal `yaml:"emergency_cap"`
	MaxSkewTicks int             `yaml:"max_skew_ticks"`
}

// ToxicityConfig controls adverse-selection detection and pause tiers.
type ToxicityConfig struct {
	ToxicMoveTicks       int           `yaml:"toxic_move_ticks"`
	ToxicDetectionWindow time.Duration `yaml:"toxic_detection_window"`
	SpikeDetectionWindow time.Duration `yaml:"spike_detection_window"`
}

// CircuitBreakerConfig holds all hard-stop thresholds.
type CircuitBreakerConfig struct {
	MaxLossPerSession decimal.Decimal `yaml:"max_loss_per_session"`
	StaleFeedTimeout  time.Duration   `yaml:"stale_feed_timeout"`
}

// ReconcilerConfig controls the position reconciler goroutine.
type ReconcilerConfig struct {
	PollInterval       time.Duration   `yaml:"poll_interval"`
	MaxDriftThreshold  decimal.Decimal `yaml:"max_drift_threshold"`
	MaxConsecutiveMiss int             `yaml:"max_consecutive_miss"`
}

// MASConfig is the top-level config loaded from config.yaml under `mas:`.
// Read-only after startup. Changes deploy via image rollout only.
type MASConfig struct {
	FairValue      FairValueConfig      `yaml:"fair_value"`
	Volatility     VolatilityConfig     `yaml:"volatility"`
	Inventory      InventoryConfig      `yaml:"inventory"`
	Toxicity       ToxicityConfig       `yaml:"toxicity"`
	CircuitBreaker CircuitBreakerConfig `yaml:"circuit_breaker"`
	Reconciler     ReconcilerConfig     `yaml:"reconciler"`
}

// MASConfigPtr is loaded once at OnTick entry. Never mutate during the pipeline.
var MASConfigPtr atomic.Pointer[MASConfig]

// MASState carries all mutable per-tick state.
// Never share across goroutines without explicit synchronization.
type MASState struct {
	Regime            VolatilityRegime
	FairValue         decimal.Decimal
	OFINormalized     decimal.Decimal
	NetPosition       decimal.Decimal
	InventoryRatio    float64
	ToxicPausedSide   *Side
	ToxicFillCount    int
	EWMAVol           float64
	EWMAVar           float64
	SessionLoss       decimal.Decimal
	LastFeedTimestamp time.Time
	CBTripCondition   string
}

// PriceAtLevel returns the absolute price for a quote at the given depth level.
// bid = fairValue - spread*(level+1), ask = fairValue + spread*(level+1).
func PriceAtLevel(side Side, level DepthLevel, fairValue, spread decimal.Decimal) decimal.Decimal {
	multiplier := decimal.NewFromInt(int64(level) + 1)
	offset := spread.Mul(multiplier)
	if side == Bid {
		return fairValue.Sub(offset)
	}
	return fairValue.Add(offset)
}