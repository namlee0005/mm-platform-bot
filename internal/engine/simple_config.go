package engine

import (
	"fmt"
	"math"
	"math/rand"
	"time"
)

// SimpleConfig is a minimal configuration that auto-generates ladder params
type SimpleConfig struct {
	Symbol     string `json:"symbol" bson:"symbol"`
	BaseAsset  string `json:"base_asset" bson:"base_asset"`
	QuoteAsset string `json:"quote_asset" bson:"quote_asset"`

	// === MAIN PARAMS (chỉ cần tune 3-4 cái này) ===

	// Spread target: 0.5% - 1% → 50-100 bps
	SpreadMinBps float64 `json:"spread_min_bps" bson:"spread_min_bps"` // e.g., 50 (0.5%)
	SpreadMaxBps float64 `json:"spread_max_bps" bson:"spread_max_bps"` // e.g., 100 (1%)

	// Depth
	NumLevels           int     `json:"num_levels" bson:"num_levels"`                       // e.g., 20
	TargetDepthNotional float64 `json:"target_depth_notional" bson:"target_depth_notional"` // e.g., 1000 USD

	// Inventory target (optional, default 0.5)
	TargetRatio float64 `json:"target_ratio" bson:"target_ratio"` // e.g., 0.5

	// === OPTIONAL TUNING (có defaults hợp lý) ===

	// Risk limits
	DrawdownLimitPct float64 `json:"drawdown_limit_pct,omitempty" bson:"drawdown_limit_pct,omitempty"` // default 0.05 (5%)
	MaxFillsPerMin   float64 `json:"max_fills_per_min,omitempty" bson:"max_fills_per_min,omitempty"`   // default 30

	// Inventory skew params
	SkewK              float64 `json:"skew_k,omitempty" bson:"skew_k,omitempty"`                           // default 2.0
	MaxSkewBps         int     `json:"max_skew_bps,omitempty" bson:"max_skew_bps,omitempty"`               // default 200
	ImbalanceThreshold float64 `json:"imbalance_threshold,omitempty" bson:"imbalance_threshold,omitempty"` // default 0.20

	// Tick interval
	TickIntervalMs int `json:"tick_interval_ms,omitempty" bson:"tick_interval_ms,omitempty"` // default 5000
}

// DefaultSimpleConfig returns a minimal config with sensible defaults
func DefaultSimpleConfig(symbol, baseAsset, quoteAsset string) *SimpleConfig {
	return &SimpleConfig{
		Symbol:              symbol,
		BaseAsset:           baseAsset,
		QuoteAsset:          quoteAsset,
		SpreadMinBps:        50,  // 0.5%
		SpreadMaxBps:        100, // 1%
		NumLevels:           20,
		TargetDepthNotional: 1000,
		TargetRatio:         0.5,
		DrawdownLimitPct:    0.05,
		MaxFillsPerMin:      30,
		SkewK:               2.0,
		MaxSkewBps:          200,
		ImbalanceThreshold:  0.20,
		TickIntervalMs:      5000,
	}
}

// ToEngineConfig converts SimpleConfig to full engine Config
// Auto-generates offsets_bps and size_mult with randomization
func (s *SimpleConfig) ToEngineConfig() *Config {
	cfg := DefaultConfig()

	// Basic info
	cfg.Symbol = s.Symbol
	cfg.BaseAsset = s.BaseAsset
	cfg.QuoteAsset = s.QuoteAsset

	// Spread
	cfg.Spread.BaseSpreadBps = (s.SpreadMinBps + s.SpreadMaxBps) / 2
	cfg.Spread.MinSpreadBps = s.SpreadMinBps
	cfg.Spread.MaxSpreadBps = s.SpreadMaxBps * 3 // allow wider in defensive mode

	// Depth - auto generate ladder
	cfg.Depth.NumLevels = s.NumLevels
	cfg.Depth.TargetDepthNotional = s.TargetDepthNotional

	// Auto-generate offsets và sizes (asymmetric ladder generated per-tick)
	bidOffsets, bidSizes := generateRandomLadder(s.NumLevels, s.SpreadMinBps, s.SpreadMaxBps)

	// Store in config as initial values (asymmetric ladder regenerated per-tick)
	cfg.Depth.OffsetsBps = bidOffsets
	cfg.Depth.SizeMult = bidSizes

	// Calculate quote per order to hit target depth
	// target_depth = num_levels * 2 * quote_per_order (roughly)
	cfg.Depth.QuotePerOrder = s.TargetDepthNotional / float64(s.NumLevels*2) * 1.2 // 1.2x buffer
	cfg.Depth.MinOrdersPerSide = s.NumLevels

	// Inventory
	cfg.Inventory.TargetRatio = s.TargetRatio
	cfg.Inventory.K = s.SkewK
	cfg.Inventory.MaxSkewBps = s.MaxSkewBps
	cfg.Inventory.ImbalanceThreshold = s.ImbalanceThreshold
	if cfg.Inventory.TargetRatio == 0 {
		cfg.Inventory.TargetRatio = 0.5
	}

	// Risk
	if s.DrawdownLimitPct > 0 {
		cfg.Risk.DrawdownLimitPct = s.DrawdownLimitPct
		cfg.Risk.DrawdownWarnPct = s.DrawdownLimitPct * 0.6
	}
	if s.MaxFillsPerMin > 0 {
		cfg.Risk.MaxFillsPerMin = s.MaxFillsPerMin
		cfg.AntiAbuse.MaxFillsPerMin = int(s.MaxFillsPerMin)
	}

	// Execution
	if s.TickIntervalMs > 0 {
		cfg.Execution.TickIntervalMs = s.TickIntervalMs
	}

	return cfg
}

// generateRandomLadder generates offsets and sizes for one side with randomization
// Returns: offsets (bps, increasing), sizes (multipliers, roughly decreasing with noise)
func generateRandomLadder(numLevels int, minSpreadBps, maxSpreadBps float64) ([]int, []float64) {
	// Use time-based source for randomization
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))

	offsets := make([]int, numLevels)
	sizes := make([]float64, numLevels)

	// Generate offsets: start from minSpread, spread out to ~5x maxSpread at last level
	// With random variation at each level
	baseOffset := minSpreadBps
	maxOffset := maxSpreadBps * 5 // furthest level at 5x max spread

	for i := 0; i < numLevels; i++ {
		// Base progression: exponential-ish growth
		progress := float64(i) / float64(numLevels-1)
		baseValue := baseOffset + (maxOffset-baseOffset)*math.Pow(progress, 1.5)

		// Add random jitter: ±15%
		jitter := 1.0 + (rng.Float64()-0.5)*0.30
		offsets[i] = int(baseValue * jitter)

		// Ensure increasing
		if i > 0 && offsets[i] <= offsets[i-1] {
			offsets[i] = offsets[i-1] + rng.Intn(10) + 5
		}
	}

	// Generate sizes: roughly decreasing with random variation
	// Close levels get more size, far levels get less
	for i := 0; i < numLevels; i++ {
		// Base decay: 1.0 at first level, ~0.2 at last level
		progress := float64(i) / float64(numLevels-1)
		baseSize := 1.0 - 0.8*progress

		// Add random noise: ±30%
		noise := 1.0 + (rng.Float64()-0.5)*0.60
		sizes[i] = math.Max(0.1, baseSize*noise)
	}

	return offsets, sizes
}

// GenerateAsymmetricLadder generates different offsets/sizes for bid and ask sides
// This is called during each tick to create asymmetry
type AsymmetricLadder struct {
	BidOffsets []int
	BidSizes   []float64
	AskOffsets []int
	AskSizes   []float64
}

// NewAsymmetricLadder generates a new asymmetric ladder
func NewAsymmetricLadder(numLevels int, minSpreadBps, maxSpreadBps float64) *AsymmetricLadder {
	bidOffsets, bidSizes := generateRandomLadder(numLevels, minSpreadBps, maxSpreadBps)
	askOffsets, askSizes := generateRandomLadder(numLevels, minSpreadBps, maxSpreadBps)

	return &AsymmetricLadder{
		BidOffsets: bidOffsets,
		BidSizes:   bidSizes,
		AskOffsets: askOffsets,
		AskSizes:   askSizes,
	}
}

// RegenerateSide regenerates one side only (for refresh with partial asymmetry)
func (l *AsymmetricLadder) RegenerateSide(side string, numLevels int, minSpreadBps, maxSpreadBps float64) {
	offsets, sizes := generateRandomLadder(numLevels, minSpreadBps, maxSpreadBps)

	if side == "BUY" {
		l.BidOffsets = offsets
		l.BidSizes = sizes
	} else {
		l.AskOffsets = offsets
		l.AskSizes = sizes
	}
}

// Print prints the ladder for debugging
func (l *AsymmetricLadder) Print() {
	fmt.Println("=== Asymmetric Ladder ===")
	fmt.Println("Level | Bid Offset | Bid Size | Ask Offset | Ask Size")
	for i := 0; i < len(l.BidOffsets); i++ {
		fmt.Printf("  %2d  |    %4d    |   %.2f   |    %4d    |   %.2f\n",
			i, l.BidOffsets[i], l.BidSizes[i], l.AskOffsets[i], l.AskSizes[i])
	}
}
