package strategy

import (
	"context"
	"fmt"
	"log"
	"math"
	"math/rand"
	"sort"
	"sync"
	"time"

	"mm-platform-engine/internal/config"
	"mm-platform-engine/internal/core"
	"mm-platform-engine/internal/exchange"
	"mm-platform-engine/internal/types"
)

// Order sizing constants
const (
	// Size jitter multiplier range: order size will be base * (MIN to MAX)
	// Controlled range ensures total stays within ~1.5x target_depth_notional
	// Example: target=$100, 5 levels, base=$20 → orders $14-$26 → total ~$80-$120
	sizeJitterMin = 0.7 // Minimum 70% of base size
	sizeJitterMax = 1.3 // Maximum 130% of base size
)

// Inventory management constants
const (
	// Inventory skew threshold above which spread adjustments kick in
	// Example: 0.05 = 5% deviation from balanced (50/50) triggers adjustment
	inventorySkewThreshold = 0.05

	// Spread split ratio bounds when adjusting for inventory
	// Limits how asymmetric the BID/ASK spread can become
	spreadSplitRatioMin = 0.3 // BID can get minimum 30% of total spread
	spreadSplitRatioMax = 0.7 // BID can get maximum 70% of total spread
)

// Fair price calculation constants
const (
	// VWAP calculation window for fair price
	vwapWindowMinutes = 10 // Use 10 minutes of trade history

	// Maximum age of VWAP before considering it stale
	maxVWAPStalenessMinutes = 30 // Warn if VWAP older than 30 minutes

	// Minimum number of trades required for reliable VWAP
	minTradesForVWAP = 3

	// Inventory adjustment strength: bps per 10% inventory skew
	// Example: 10% skew → ±50 bps adjustment
	inventoryAdjustmentBps = 50.0
)

// ModeChangeCallback is called when the strategy mode changes
type ModeChangeCallback func(oldMode, newMode string, nav, peakNAV, drawdownPct float64)

// BalanceLowCallback is called when balance is too low to trade
type BalanceLowCallback func(available, required float64)

// SimpleLadderConfig is the configuration for SimpleLadderStrategy
type SimpleLadderConfig struct {
	*config.SimpleConfig // Embed base config from MongoDB

	// Strategy-specific settings (not in MongoDB config)
	PriceJitterPct      float64 `json:"price_jitter_pct"`
	SizeJitterPct       float64 `json:"size_jitter_pct"`
	DrawdownWarnPct     float64 `json:"drawdown_warn_pct"`      // Warning threshold (e.g., 0.03 = 3%)
	DrawdownReducePct   float64 `json:"drawdown_reduce_pct"`    // Start reducing size at this level (e.g., 0.02 = 2%)
	RecoveryHours       float64 `json:"recovery_hours"`         // Target recovery time in hours (24-72)
	MaxRecoverySizeMult float64 `json:"max_recovery_size_mult"` // Max size multiplier during recovery (e.g., 0.3 = 30%)

	// Callbacks
	OnModeChange ModeChangeCallback `json:"-"` // Called when mode changes (for notifications)
	OnBalanceLow BalanceLowCallback `json:"-"` // Called when balance is too low to trade
}

// vwapResult contains VWAP calculation result with metadata
type vwapResult struct {
	price       float64       // Calculated VWAP price
	tradeCount  int           // Number of trades used
	totalVolume float64       // Total volume in calculation
	oldestTrade time.Time     // Timestamp of oldest trade
	age         time.Duration // Age of oldest trade
}

// SimpleLadderStrategy implements a two-sided market maker strategy
// that places a ladder of orders on both the bid and ask sides.
type SimpleLadderStrategy struct {
	cfg *SimpleLadderConfig

	// Market info cache
	tickSize    float64
	stepSize    float64
	minNotional float64
	maxOrderQty float64 // Maximum quantity per order (exchange limit)

	// State
	mu sync.RWMutex

	// Risk tracking
	peakNAV        float64          // Peak NAV for drawdown calculation
	navHistory     []navSnapshot    // NAV history for tracking
	currentMode    SimpleLadderMode // Current operating mode
	modeChangedAt  time.Time        // When mode last changed
	recoveryTarget float64          // Target NAV for recovery

	// Fair price tracking (VWAP-based)
	lastFairPrice     float64   // Last calculated fair price
	lastFairPriceTime time.Time // When fair price was last updated
	lastVWAP          float64   // Last calculated VWAP (without inventory adjustment)
	lastVWAPTime      time.Time // When VWAP was last calculated

	// Fill detection: snapshot from previous cycle
	prevBidCount  int     // BUY order count before Step 1 last cycle
	prevAskCount  int     // SELL order count before Step 1 last cycle
	prevBaseFree  float64 // Base free balance last cycle
	prevQuoteFree float64 // Quote free balance last cycle
}

// SimpleLadderMode represents the operating mode
type SimpleLadderMode string

const (
	ModeNormal   SimpleLadderMode = "NORMAL"
	ModeWarning  SimpleLadderMode = "WARNING"  // Drawdown > warn threshold
	ModeRecovery SimpleLadderMode = "RECOVERY" // Drawdown > reduce threshold, reducing size
	ModePaused   SimpleLadderMode = "PAUSED"   // Drawdown > limit, no new orders
)

// navSnapshot stores NAV at a point in time
type navSnapshot struct {
	timestamp time.Time
	nav       float64
}

// NewSimpleLadderStrategy creates a new SimpleLadderStrategy
func NewSimpleLadderStrategy(cfg *SimpleLadderConfig) *SimpleLadderStrategy {
	// Set defaults
	if cfg.NumLevels == 0 {
		cfg.NumLevels = 5
	}
	if cfg.SpreadMinBps == 0 {
		cfg.SpreadMinBps = 40 // 0.4% first level min
	}
	if cfg.SpreadMaxBps == 0 {
		cfg.SpreadMaxBps = 100 // 1% first level max
	}
	if cfg.PriceJitterPct == 0 {
		cfg.PriceJitterPct = 0.2
	}
	if cfg.SizeJitterPct == 0 {
		cfg.SizeJitterPct = 0.3
	}
	if cfg.LadderRegenBps == 0 {
		cfg.LadderRegenBps = 50
	}
	if cfg.DepthBps == 0 {
		cfg.DepthBps = 200 // 2% max depth
	}
	if cfg.PyramidFactor <= 0 {
		cfg.PyramidFactor = 3.0 // outer orders are 3x the size of inner orders
	}
	if cfg.LevelGapTicksMax == 0 {
		cfg.LevelGapTicksMax = 3
	}

	// Risk defaults
	if cfg.DrawdownLimitPct == 0 {
		cfg.DrawdownLimitPct = 0.05 // 5% max drawdown
	}
	if cfg.DrawdownWarnPct == 0 {
		cfg.DrawdownWarnPct = 0.03 // 3% warning
	}
	if cfg.DrawdownReducePct == 0 {
		cfg.DrawdownReducePct = 0.02 // 2% start reducing
	}
	if cfg.RecoveryHours == 0 {
		cfg.RecoveryHours = 48 // 48 hours recovery target
	}
	if cfg.MaxRecoverySizeMult == 0 {
		cfg.MaxRecoverySizeMult = 0.3 // 30% size during max recovery
	}

	return &SimpleLadderStrategy{
		cfg:          cfg,
		navHistory:   make([]navSnapshot, 0, 1000),
		currentMode:  ModeNormal,
		prevBidCount: 0,
		prevAskCount: 0,
	}
}

// Name returns the strategy name
func (s *SimpleLadderStrategy) Name() string {
	return "SimpleLadder"
}

// Init initializes the strategy with market state
func (s *SimpleLadderStrategy) Init(ctx context.Context, snap *core.Snapshot, balance *core.BalanceState) error {
	s.tickSize = snap.TickSize
	s.stepSize = snap.StepSize
	s.minNotional = snap.MinNotional
	s.maxOrderQty = snap.MaxOrderQty

	// Validate tickSize - CRITICAL for price calculations
	if s.tickSize <= 0 {
		// Try to derive from mid price (8 decimal places default)
		if snap.Mid > 0 {
			s.tickSize = math.Pow(10, -8) // 0.00000001
			log.Printf("[%s] WARNING: tickSize not set, using default %.8f", s.Name(), s.tickSize)
		} else {
			return fmt.Errorf("tickSize is 0 and cannot derive default")
		}
	}

	// Validate stepSize
	if s.stepSize <= 0 {
		s.stepSize = math.Pow(10, -8) // 0.00000001
		log.Printf("[%s] WARNING: stepSize not set, using default %.8f", s.Name(), s.stepSize)
	}

	if s.minNotional <= 0 {
		s.minNotional = 5.0
	}

	// Default max order qty if not set (Bybit default is 2M for most tokens)
	if s.maxOrderQty <= 0 {
		s.maxOrderQty = 1000000 // Conservative default: 1M
	}

	log.Printf("[%s] Initialized: tickSize=%.8f, stepSize=%.8f, minNotional=%.2f, maxOrderQty=%.0f",
		s.Name(), s.tickSize, s.stepSize, s.minNotional, s.maxOrderQty)

	return nil
}

// Tick executes one strategy cycle
func (s *SimpleLadderStrategy) Tick(ctx context.Context, input *core.TickInput) (*core.TickOutput, error) {
	snap := input.Snapshot
	balance := input.Balance
	orderBookMid := snap.Mid // Order book mid (fallback)

	// Calculate fair price using VWAP + inventory adjustment
	fairPrice, isFreshVWAP := s.calculateFairPrice(input.RecentTrades, balance, orderBookMid)

	// Safety check: if fair price is invalid, cancel all orders and wait
	if fairPrice <= 0 || math.IsNaN(fairPrice) || math.IsInf(fairPrice, 0) {
		return &core.TickOutput{
			Action: core.TickActionCancelAll,
			Reason: fmt.Sprintf("invalid fair price: %.8f (orderBookMid=%.8f)", fairPrice, orderBookMid),
		}, nil
	}

	// Log if using fallback vs fresh VWAP
	if !isFreshVWAP {
		log.Printf("[%s] Using fallback price (no fresh VWAP): %.8f", s.Name(), fairPrice)
	}

	// Use fair price for all calculations (instead of order book mid)
	mid := fairPrice

	// Calculate current NAV (use fair price for valuation)
	nav := s.calculateNAV(balance, mid)

	// Update risk state
	drawdown, mode := s.updateRiskState(nav)

	// Log risk status periodically
	if len(s.navHistory)%60 == 0 { // Every ~60 ticks
		log.Printf("[%s] Risk: mode=%s, NAV=$%.2f, peak=$%.2f, drawdown=%.2f%%",
			s.Name(), mode, nav, s.peakNAV, drawdown*100)
	}

	// If PAUSED due to drawdown limit, cancel all orders
	if mode == ModePaused {
		return &core.TickOutput{
			Action: core.TickActionCancelAll,
			Reason: fmt.Sprintf("PAUSED: drawdown %.2f%% exceeds limit %.2f%%",
				drawdown*100, s.cfg.DrawdownLimitPct*100),
		}, nil
	}

	// Get total available balance (both sides)
	availableBalance := s.getAvailableBalance(balance, mid)

	// Check if we have minimum balance to trade
	// Need TargetDepthNotional * 2 (one for each side)
	minRequired := s.cfg.TargetDepthNotional * 2
	if availableBalance < minRequired {
		// Notify via callback
		if s.cfg.OnBalanceLow != nil {
			go s.cfg.OnBalanceLow(availableBalance, minRequired)
		}
		return &core.TickOutput{
			Action: core.TickActionCancelAll,
			Reason: fmt.Sprintf("insufficient balance: %.2f < %.2f (need %0.f per side)", availableBalance, minRequired, s.cfg.TargetDepthNotional),
		}, nil
	}

	// Calculate size multiplier based on mode
	sizeMult := s.calculateSizeMultiplier(drawdown, mode)

	metrics := map[string]float64{
		"nav":       nav,
		"drawdown":  drawdown,
		"size_mult": sizeMult,
	}

	result := s.maintainLadder(mid, input.Snapshot.BestBid, input.Snapshot.BestAsk, balance, input.LiveOrders, sizeMult)
	if result == nil {
		return &core.TickOutput{Action: core.TickActionKeep, Reason: "ladder_ok", Metrics: metrics}, nil
	}
	result.Metrics = metrics
	return result, nil
}

// calculateNAV computes Net Asset Value
func (s *SimpleLadderStrategy) calculateNAV(balance *core.BalanceState, mid float64) float64 {
	baseValue := (balance.BaseFree + balance.BaseLocked) * mid
	quoteValue := balance.QuoteFree + balance.QuoteLocked
	return baseValue + quoteValue
}

// updateRiskState updates NAV tracking and returns current drawdown and mode
func (s *SimpleLadderStrategy) updateRiskState(nav float64) (drawdown float64, mode SimpleLadderMode) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()

	// Record NAV history
	s.navHistory = append(s.navHistory, navSnapshot{
		timestamp: now,
		nav:       nav,
	})

	// Keep only last 24 hours of history
	cutoff := now.Add(-24 * time.Hour)
	for len(s.navHistory) > 0 && s.navHistory[0].timestamp.Before(cutoff) {
		s.navHistory = s.navHistory[1:]
	}

	// Update peak NAV (only when not in recovery)
	if s.currentMode == ModeNormal || s.currentMode == ModeWarning {
		if nav > s.peakNAV {
			s.peakNAV = nav
		}
	}

	// Initialize peak if first time
	if s.peakNAV == 0 {
		s.peakNAV = nav
	}

	// Calculate drawdown from peak
	if s.peakNAV > 0 {
		drawdown = (s.peakNAV - nav) / s.peakNAV
	}

	// Determine mode based on drawdown
	prevMode := s.currentMode

	if drawdown >= s.cfg.DrawdownLimitPct {
		s.currentMode = ModePaused
	} else if drawdown >= s.cfg.DrawdownReducePct {
		s.currentMode = ModeRecovery
		if prevMode != ModeRecovery {
			s.recoveryTarget = s.peakNAV * (1 - s.cfg.DrawdownReducePct*0.5)
		}
	} else if drawdown >= s.cfg.DrawdownWarnPct {
		s.currentMode = ModeWarning
	} else {
		s.currentMode = ModeNormal
		// Reset peak when fully recovered
		if prevMode == ModeRecovery && nav >= s.recoveryTarget {
			log.Printf("[%s] Recovery complete! Resetting peak NAV to $%.2f", s.Name(), nav)
			s.peakNAV = nav
		}
	}

	// Log mode changes and trigger callback
	if s.currentMode != prevMode {
		s.modeChangedAt = now
		log.Printf("[%s] Mode changed: %s -> %s (drawdown=%.2f%%, NAV=$%.2f, peak=$%.2f)",
			s.Name(), prevMode, s.currentMode, drawdown*100, nav, s.peakNAV)

		// Call mode change callback (for notifications)
		if s.cfg.OnModeChange != nil {
			go s.cfg.OnModeChange(string(prevMode), string(s.currentMode), nav, s.peakNAV, drawdown)
		}
	}

	return drawdown, s.currentMode
}

// calculateSizeMultiplier returns size multiplier based on drawdown and mode
func (s *SimpleLadderStrategy) calculateSizeMultiplier(drawdown float64, mode SimpleLadderMode) float64 {
	switch mode {
	case ModeNormal:
		return 1.0

	case ModeWarning:
		// Slight reduction: 80-100%
		return 0.8 + 0.2*(1-drawdown/s.cfg.DrawdownReducePct)

	case ModeRecovery:
		// Progressive reduction based on drawdown severity
		// At DrawdownReducePct: 100% -> At DrawdownLimitPct: MaxRecoverySizeMult
		severity := (drawdown - s.cfg.DrawdownReducePct) / (s.cfg.DrawdownLimitPct - s.cfg.DrawdownReducePct)
		if severity > 1 {
			severity = 1
		}
		mult := 1.0 - severity*(1.0-s.cfg.MaxRecoverySizeMult)
		if mult < s.cfg.MaxRecoverySizeMult {
			mult = s.cfg.MaxRecoverySizeMult
		}
		return mult

	case ModePaused:
		return 0 // No orders

	default:
		return 1.0
	}
}

// GetMode returns current operating mode
func (s *SimpleLadderStrategy) GetMode() SimpleLadderMode {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.currentMode
}

// GetDrawdown returns current drawdown percentage
func (s *SimpleLadderStrategy) GetDrawdown() float64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.peakNAV == 0 || len(s.navHistory) == 0 {
		return 0
	}
	currentNAV := s.navHistory[len(s.navHistory)-1].nav
	return (s.peakNAV - currentNAV) / s.peakNAV
}

// GetNAV returns current NAV
func (s *SimpleLadderStrategy) GetNAV() float64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(s.navHistory) == 0 {
		return 0
	}
	return s.navHistory[len(s.navHistory)-1].nav
}

// GetPeakNAV returns peak NAV
func (s *SimpleLadderStrategy) GetPeakNAV() float64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.peakNAV
}

// OnFill handles fill events
// Strategy only logs - BaseBot handles order tracker updates, Redis, and MongoDB
func (s *SimpleLadderStrategy) OnFill(event *core.FillEvent) {
	notional := event.Price * event.Quantity
	log.Printf("[%s] 💰 FILL %s @ %.8f x %.4f ($%.2f) order=%s",
		s.Name(), event.Side, event.Price, event.Quantity, notional, event.OrderID)
}

// OnOrderUpdate handles order status updates
// Strategy only logs important events - BaseBot handles order tracker updates
func (s *SimpleLadderStrategy) OnOrderUpdate(event *core.OrderEvent) {
	// Only log NEW events (CANCELED/FILLED handled elsewhere or silently)
	if event.Status == "NEW" {
		log.Printf("[%s] Order %s: %s @ %.8f status=%s",
			s.Name(), event.OrderID, event.Side, event.Price, event.Status)
	}
	// No state tracking needed - BaseBot syncs from exchange every tick
}

// UpdateConfig updates strategy config at runtime
func (s *SimpleLadderStrategy) UpdateConfig(newCfg interface{}) error {
	cfg, ok := newCfg.(*types.SimpleConfigUpdate)
	if !ok {
		return fmt.Errorf("invalid config type")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.cfg.SpreadMinBps = cfg.SpreadMinBps
	s.cfg.NumLevels = cfg.NumLevels
	s.cfg.TargetDepthNotional = cfg.TargetDepthNotional
	s.cfg.LadderRegenBps = cfg.LadderRegenBps
	s.cfg.MinBalanceToTrade = cfg.MinBalanceToTrade
	s.cfg.LevelGapTicksMax = cfg.LevelGapTicksMax
	s.cfg.DepthBps = cfg.DepthBps
	s.cfg.LevelStepBps = cfg.LevelStepBps

	log.Printf("[%s] Config updated: spread=%.0fbps, depthBps=%.0f, levels=%d, depth=$%.0f, levelStepBps=%.0f",
		s.Name(), s.cfg.SpreadMinBps, s.cfg.DepthBps, s.cfg.NumLevels, s.cfg.TargetDepthNotional, s.cfg.LevelStepBps)

	return nil
}

// getAvailableBalance returns total NAV (base + quote, including locked in orders)
// Must include locked so that fully-deployed sides don't incorrectly trigger CancelAll.
func (s *SimpleLadderStrategy) getAvailableBalance(balance *core.BalanceState, mid float64) float64 {
	return balance.TotalBase()*mid + balance.TotalQuote()
}

// calculateVWAP computes volume-weighted average price from recent trades
// Returns VWAP result with metadata, or error if insufficient data
func calculateVWAP(trades []exchange.Trade, windowDuration time.Duration) (*vwapResult, error) {
	if len(trades) == 0 {
		return nil, fmt.Errorf("no trades provided")
	}

	now := time.Now()
	cutoff := now.Add(-windowDuration)

	var totalValue float64
	var totalVolume float64
	var oldestTrade time.Time
	var tradeCount int

	for _, trade := range trades {
		// Only include trades within window
		if trade.Timestamp.Before(cutoff) {
			continue
		}

		value := trade.Price * trade.Quantity
		totalValue += value
		totalVolume += trade.Quantity
		tradeCount++

		// Track oldest trade in window
		if oldestTrade.IsZero() || trade.Timestamp.Before(oldestTrade) {
			oldestTrade = trade.Timestamp
		}
	}

	if tradeCount < minTradesForVWAP {
		return nil, fmt.Errorf("insufficient trades: %d < %d required", tradeCount, minTradesForVWAP)
	}

	if totalVolume == 0 {
		return nil, fmt.Errorf("zero total volume")
	}

	vwap := totalValue / totalVolume
	age := now.Sub(oldestTrade)

	return &vwapResult{
		price:       vwap,
		tradeCount:  tradeCount,
		totalVolume: totalVolume,
		oldestTrade: oldestTrade,
		age:         age,
	}, nil
}

// calculateFairPrice computes fair price using VWAP with inventory adjustment
// Returns fair price and whether it's from fresh data
func (s *SimpleLadderStrategy) calculateFairPrice(trades []exchange.Trade, balance *core.BalanceState, mid float64) (float64, bool) {
	// Try to calculate VWAP from recent trades
	vwapRes, err := calculateVWAP(trades, vwapWindowMinutes*time.Minute)

	var basePrice float64
	var isFresh bool

	if err != nil {
		// No valid VWAP - use fallback
		if s.lastVWAP > 0 && time.Since(s.lastVWAPTime) < maxVWAPStalenessMinutes*time.Minute {
			// Use cached VWAP if not too stale
			basePrice = s.lastVWAP
			isFresh = false
			log.Printf("[FairPrice] ⚠️  No recent trades, using cached VWAP: %.8f (age: %v)",
				basePrice, time.Since(s.lastVWAPTime))
		} else {
			// Emergency fallback: use order book mid
			basePrice = mid
			isFresh = false
			log.Printf("[FairPrice] 🔴 No VWAP available, using order book mid: %.8f", basePrice)
		}
	} else {
		// Valid VWAP
		basePrice = vwapRes.price
		isFresh = true

		// Update cache
		s.lastVWAP = basePrice
		s.lastVWAPTime = time.Now()

		// Log VWAP calculation
		if vwapRes.age > time.Duration(maxVWAPStalenessMinutes)*time.Minute {
			log.Printf("[FairPrice] ⚠️  VWAP stale: %.8f (trades=%d, age=%v, volume=%.2f)",
				basePrice, vwapRes.tradeCount, vwapRes.age, vwapRes.totalVolume)
		} else {
			log.Printf("[FairPrice] VWAP: %.8f (trades=%d, age=%v, volume=%.2f)",
				basePrice, vwapRes.tradeCount, vwapRes.age, vwapRes.totalVolume)
		}
	}

	// Calculate inventory state
	inv := calculateInventoryState(balance.BaseFree, balance.QuoteFree, basePrice, float64(s.cfg.InitBase), float64(s.cfg.InitQuote))

	// Apply inventory adjustment
	// Positive skew (too much base) → lower price to encourage selling
	// Negative skew (too much quote) → raise price to encourage buying
	adjustment := -inv.skew * basePrice * (inventoryAdjustmentBps / 10000.0)

	fairPrice := basePrice + adjustment

	// Log inventory adjustment if significant
	if math.Abs(inv.skew) > inventorySkewThreshold {
		log.Printf("[FairPrice] Inventory adj: skew=%.1f%%, base=$%.0f, quote=$%.0f → adj=%.8f",
			inv.skew*100, inv.baseValue, inv.quoteValue, adjustment)
	}

	log.Printf("[FairPrice] Final: %.8f (base=%.8f, inv_adj=%.8f)", fairPrice, basePrice, adjustment)

	// Update cache
	s.lastFairPrice = fairPrice
	s.lastFairPriceTime = time.Now()

	return fairPrice, isFresh
}

// inventoryState represents the current inventory position
type inventoryState struct {
	baseValue  float64 // Value of base asset in quote terms
	quoteValue float64 // Value of quote asset
	ratio      float64 // baseValue / totalValue (0.5 = balanced)
	skew       float64 // Deviation from balanced: ratio - 0.5 (-0.5 to +0.5)
}

// calculateInventoryState computes current inventory position and skew
// relative to the configured init_base / init_quote amounts.
//
// skew > 0: holding more base than init → tighten ASK, widen BID (sell more)
// skew < 0: holding less base than init → tighten BID, widen ASK (buy more)
func calculateInventoryState(baseFree, quoteFree, mid, initBase, initQuote float64) inventoryState {
	baseValue := baseFree * mid
	quoteValue := quoteFree
	initNav := initBase*mid + initQuote

	skew := 0.0
	if initNav > 0 {
		skew = (baseFree - initBase) * mid / initNav
	}
	// Clamp to [-0.5, 0.5] to keep spread allocation well-behaved
	if skew > 0.5 {
		skew = 0.5
	} else if skew < -0.5 {
		skew = -0.5
	}

	ratio := 0.5
	if baseValue+quoteValue > 0 {
		ratio = baseValue / (baseValue + quoteValue)
	}

	return inventoryState{
		baseValue:  baseValue,
		quoteValue: quoteValue,
		ratio:      ratio,
		skew:       skew,
	}
}

// spreadAllocation represents how total spread is divided between BID and ASK
type spreadAllocation struct {
	totalBps float64 // Total spread from first BID to first ASK
	bidBps   float64 // BID offset from mid
	askBps   float64 // ASK offset from mid
}

// calculateSpreadAllocation determines how to split total spread between BID and ASK
// Adjusts for inventory skew to encourage balanced position
func calculateSpreadAllocation(spreadMinBps, spreadMaxBps float64, invSkew float64) spreadAllocation {
	// Random total spread within configured range
	totalSpread := spreadMinBps + rand.Float64()*(spreadMaxBps-spreadMinBps)

	// Split ratio: 0.5 = balanced (50/50), adjusted by inventory skew
	// invSkew > 0 (too much base): BID gets more spread (further from mid) to sell base
	// invSkew < 0 (too much quote): ASK gets more spread (further from mid) to buy base
	splitRatio := 0.5 + invSkew

	// Clamp to prevent extreme asymmetry
	if splitRatio < spreadSplitRatioMin {
		splitRatio = spreadSplitRatioMin
	}
	if splitRatio > spreadSplitRatioMax {
		splitRatio = spreadSplitRatioMax
	}

	return spreadAllocation{
		totalBps: totalSpread,
		bidBps:   totalSpread * splitRatio,
		askBps:   totalSpread * (1.0 - splitRatio),
	}
}

// calculateSizeMultiplier returns a random multiplier for order size
// Wide range creates unpredictable order patterns
func calculateSizeMultiplier() float64 {
	return sizeJitterMin + rand.Float64()*(sizeJitterMax-sizeJitterMin)
}

// pyramidNotional returns the target notional for a specific level using a pyramid size distribution.
// level 0 = inner (closest to mid, smallest), level totalLevels-1 = outer (largest).
// factor: ratio of outer to inner size (e.g. 3.0 means outer is 3x inner).
// With factor=3, totalLevels=20: inner=$125, outer=$375 when targetDepth=$5000.
func pyramidNotional(level, totalLevels int, targetDepth, factor float64) float64 {
	if totalLevels <= 1 {
		return targetDepth
	}
	if factor <= 1 {
		return targetDepth / float64(totalLevels)
	}
	// weight[i] = 1 + (factor-1) × i/(n-1)
	// totalWeight = n × (1+factor)/2
	totalWeight := float64(totalLevels) * (1.0 + factor) / 2.0
	weight := 1.0 + (factor-1.0)*float64(level)/float64(totalLevels-1)
	return targetDepth * weight / totalWeight
}

// generateSideOrders generates orders for one side (BUY or SELL)
func (s *SimpleLadderStrategy) generateSideOrders(mid, firstLevelSpreadBps, depthBps float64, numLevels int, targetDepth float64, side string, batchID string) []core.DesiredOrder {
	// First level is at firstLevelSpreadBps from mid (calculated in computeDesiredOrders)
	// Other levels spread from first level to depthBps

	// Calculate price range for ladder
	// First level: at firstLevelSpreadBps from mid (randomized)
	// Last level: at depthBps from mid
	var firstPrice, lastPrice float64
	if side == "BUY" {
		firstPrice = mid * (1.0 - firstLevelSpreadBps/10000.0)
		lastPrice = mid * (1.0 - depthBps/10000.0)
		// Ensure lastPrice is at least tickSize (for depthBps >= 10000)
		if lastPrice < s.tickSize {
			lastPrice = s.tickSize
		}
	} else {
		firstPrice = mid * (1.0 + firstLevelSpreadBps/10000.0)
		lastPrice = mid * (1.0 + depthBps/10000.0)
	}

	// Calculate base step between levels (in price)
	priceRange := math.Abs(lastPrice - firstPrice)

	// Ensure levels fit within price range with minimum gap (tickSize)
	minStepRequired := s.tickSize
	maxLevelsThatFit := int(priceRange/minStepRequired) + 1
	if maxLevelsThatFit < 1 {
		maxLevelsThatFit = 1
	}
	if numLevels > maxLevelsThatFit {
		numLevels = maxLevelsThatFit
	}

	baseStep := priceRange / float64(numLevels-1)
	if numLevels == 1 {
		baseStep = 0
	}

	// Calculate max jitter
	maxGapTicks := s.cfg.LevelGapTicksMax
	if maxGapTicks <= 0 {
		maxGapTicks = 3
	}
	maxJitterTicks := s.tickSize * float64(maxGapTicks)
	maxJitterStep := baseStep * 0.3
	maxJitter := maxJitterTicks
	if maxJitterStep < maxJitter && maxJitterStep > 0 {
		maxJitter = maxJitterStep
	}

	// Generate prices
	levelPrices := make([]float64, 0, numLevels)
	for level := 0; level < numLevels; level++ {
		var basePrice float64
		if side == "BUY" {
			basePrice = firstPrice - baseStep*float64(level)
		} else {
			basePrice = firstPrice + baseStep*float64(level)
		}

		var price float64
		if level == 0 || level == numLevels-1 {
			price = basePrice
		} else {
			jitter := (rand.Float64()*2 - 1) * maxJitter
			price = basePrice + jitter
		}

		price = s.roundToTick(price)

		// Ensure within bounds
		if side == "BUY" {
			minPrice := mid * (1.0 - depthBps/10000.0)
			if price < minPrice {
				price = s.ceilToTick(minPrice) // Round UP to ensure we don't go below minPrice
			}
			if price > mid {
				price = s.roundToTick(mid * (1.0 - firstLevelSpreadBps/10000.0))
			}
		} else {
			maxPrice := mid * (1.0 + depthBps/10000.0)
			if price > maxPrice {
				price = s.floorToTick(maxPrice) // Round DOWN to ensure we don't exceed maxPrice
			}
			if price < mid {
				price = s.roundToTick(mid * (1.0 + firstLevelSpreadBps/10000.0))
			}
		}

		levelPrices = append(levelPrices, price)
	}

	// Sort prices
	if side == "BUY" {
		sort.Float64s(levelPrices)
		// Reverse for descending order (highest first for bids)
		for i, j := 0, len(levelPrices)-1; i < j; i, j = i+1, j-1 {
			levelPrices[i], levelPrices[j] = levelPrices[j], levelPrices[i]
		}
	} else {
		sort.Float64s(levelPrices) // Ascending for asks
	}

	levelPrices = s.removeDuplicatePrices(levelPrices)
	actualLevels := len(levelPrices)
	if actualLevels == 0 {
		return nil
	}

	// Pre-compute noisy pyramid weights for all levels, then normalize so
	// sum(baseNotional) = targetDepth exactly → both sides always balanced.
	// ±15% noise per level makes the shape organic without drifting total.
	rawWeights := make([]float64, actualLevels)
	totalRawWeight := 0.0
	for i := 0; i < actualLevels; i++ {
		factor := s.cfg.PyramidFactor
		if factor <= 0 {
			factor = 1
		}
		w := 1.0 + (factor-1.0)*float64(i)/float64(actualLevels-1)
		w *= 0.50 + rand.Float64()*1.00 // ±50% noise
		rawWeights[i] = w
		totalRawWeight += w
	}

	// Generate orders with sizes
	orders := make([]core.DesiredOrder, 0, actualLevels)

	sideTag := "B"
	if side == "SELL" {
		sideTag = "A"
	}

	for level := 0; level < actualLevels; level++ {
		price := levelPrices[level]

		// Validate price before calculating quantity
		if price <= 0 || math.IsNaN(price) || math.IsInf(price, 0) {
			log.Printf("[%s] Skipping invalid price at level %d: %.8f", s.Name(), level, price)
			continue
		}

		// Normalized pyramid notional: noise applied but total = targetDepth
		levelNotional := targetDepth * rawWeights[level] / totalRawWeight
		sizeNotional := levelNotional * calculateSizeMultiplier()

		qty := sizeNotional / price
		qty = s.roundToStep(qty)

		// Validate quantity (catch Inf, NaN, zero, negative)
		if qty <= 0 || math.IsNaN(qty) || math.IsInf(qty, 0) {
			log.Printf("[%s] Skipping invalid qty at level %d: price=%.8f qty=%.8f", s.Name(), level, price, qty)
			continue
		}

		// Cap quantity to exchange max order limit
		if s.maxOrderQty > 0 && qty > s.maxOrderQty {
			log.Printf("[%s] Capping qty at level %d: %.0f -> %.0f (max limit)", s.Name(), level, qty, s.maxOrderQty)
			qty = s.maxOrderQty
		}

		orderNotional := price * qty
		if orderNotional < s.minNotional {
			// Bump qty up to minimum viable rather than dropping the level
			minQty := s.minNotional / price
			qty = s.roundToStep(minQty)
			// After rounding up, re-check (roundToStep may round down on some exchanges)
			if price*qty < s.minNotional {
				qty += s.stepSize
				qty = s.roundToStep(qty)
			}
			orderNotional = price * qty
			if orderNotional < s.minNotional {
				log.Printf("[%s] Skipping level %d: can't meet minNotional %.4f at price %.8f (minQty=%.8f)", s.Name(), level, s.minNotional, price, qty)
				continue
			}
		}

		orders = append(orders, core.DesiredOrder{
			Side:       side,
			Price:      price,
			Qty:        qty,
			LevelIndex: level,
			Tag:        fmt.Sprintf("SM_%s_L%d_%s", batchID, level, sideTag),
		})
	}

	return orders
}

// computeAvgStep returns the average price step between consecutive same-side orders.
// Returns 0 if fewer than 2 orders.
func (s *SimpleLadderStrategy) computeAvgStep(orders []core.LiveOrder, side string) float64 {
	var prices []float64
	for _, o := range orders {
		if o.Side == side {
			prices = append(prices, o.Price)
		}
	}
	if len(prices) < 2 {
		return 0
	}
	sort.Float64s(prices)
	total := 0.0
	for i := 1; i < len(prices); i++ {
		total += prices[i] - prices[i-1]
	}
	return total / float64(len(prices)-1)
}

// maintainLadder performs incremental ladder maintenance without full regeneration.
//   - Cancels orders that drifted outside the depth window
//   - Adds outer orders when fills create gaps (BUY fill → new BUY at outer; SELL fill → new SELL at outer)
//   - Corrects spread if it widened beyond SpreadMaxBps after the outer add
//
// Returns nil (KEEP) when no changes are needed.
func (s *SimpleLadderStrategy) maintainLadder(mid, marketBestBid, marketBestAsk float64, balance *core.BalanceState, liveOrders []core.LiveOrder, sizeMult float64) *core.TickOutput {
	depthBps := s.cfg.DepthBps
	if depthBps <= 0 {
		depthBps = 200
	}
	spreadMinBps := s.cfg.SpreadMinBps
	if spreadMinBps <= 0 {
		spreadMinBps = 40
	}
	spreadMaxBps := s.cfg.SpreadMaxBps
	if spreadMaxBps <= 0 {
		spreadMaxBps = 100
	}
	expectedPerSide := s.cfg.NumLevels
	if expectedPerSide <= 0 {
		expectedPerSide = 5
	}
	targetDepthPerSide := s.cfg.TargetDepthNotional

	priceTolerance := s.tickSize * float64(s.cfg.LevelGapTicksMax)
	if priceTolerance <= 0 {
		priceTolerance = s.tickSize * 3
	}

	depthFloor := s.roundToTick(mid * (1.0 - depthBps/10000.0))
	depthCeil := s.roundToTick(mid * (1.0 + depthBps/10000.0))

	// Fill detection: count orders before Step 1 touches anything.
	// If count dropped vs previous cycle → a fill happened on that side.
	totalBidsBefore, totalAsksBefore := 0, 0
	for _, o := range liveOrders {
		if o.Side == "BUY" {
			totalBidsBefore++
		} else {
			totalAsksBefore++
		}
	}
	bidFilled := totalBidsBefore < s.prevBidCount
	askFilled := totalAsksBefore < s.prevAskCount
	if bidFilled {
		baseUp := balance.BaseFree > s.prevBaseFree
		log.Printf("[%s] Fill detected: BUY side %d→%d (base %.2f→%.2f, up=%v)",
			s.Name(), s.prevBidCount, totalBidsBefore, s.prevBaseFree, balance.BaseFree, baseUp)
	}
	if askFilled {
		quoteUp := balance.QuoteFree > s.prevQuoteFree
		log.Printf("[%s] Fill detected: SELL side %d→%d (quote %.2f→%.2f, up=%v)",
			s.Name(), s.prevAskCount, totalAsksBefore, s.prevQuoteFree, balance.QuoteFree, quoteUp)
	}

	// Step 1: partition live orders into valid and invalid (out-of-range)
	var validBids, validAsks []core.LiveOrder
	var toCancel []string
	for _, o := range liveOrders {
		var invalid bool
		var cancelReason string
		if o.Side == "BUY" {
			if o.Price >= mid {
				invalid = true
				cancelReason = fmt.Sprintf("BUY price %.8f >= mid %.8f (above mid)", o.Price, mid)
			} else if o.Price < depthFloor-s.tickSize {
				invalid = true
				cancelReason = fmt.Sprintf("BUY price %.8f < depthFloor %.8f (out of range)", o.Price, depthFloor)
			}
		} else {
			if o.Price <= mid {
				invalid = true
				cancelReason = fmt.Sprintf("SELL price %.8f <= mid %.8f (below mid)", o.Price, mid)
			} else if o.Price > depthCeil+s.tickSize {
				invalid = true
				cancelReason = fmt.Sprintf("SELL price %.8f > depthCeil %.8f (out of range)", o.Price, depthCeil)
			}
		}
		if invalid {
			log.Printf("[%s] CANCEL order=%s %s @ %.8f — %s", s.Name(), o.OrderID, o.Side, o.Price, cancelReason)
			toCancel = append(toCancel, o.OrderID)
		} else if o.Side == "BUY" {
			validBids = append(validBids, o)
		} else {
			validAsks = append(validAsks, o)
		}
	}

	bidCount := len(validBids)
	askCount := len(validAsks)

	// Cancel excess orders (keep only expectedPerSide per side, cancel outermost first).
	// Exception: if notional is still below target, allow up to expectedPerSide*2 to keep top-up orders alive.
	if bidCount > expectedPerSide {
		totalBidNotional := 0.0
		for _, o := range validBids {
			totalBidNotional += o.Price * o.Qty
		}
		if totalBidNotional >= targetDepthPerSide || bidCount > expectedPerSide*2 {
			sort.Slice(validBids, func(i, j int) bool { return validBids[i].Price < validBids[j].Price })
			excess := bidCount - expectedPerSide
			for i := 0; i < excess; i++ {
				log.Printf("[%s] CANCEL order=%s BUY @ %.8f — excess BUY (%d > %d), removing outermost", s.Name(), validBids[i].OrderID, validBids[i].Price, bidCount, expectedPerSide)
				toCancel = append(toCancel, validBids[i].OrderID)
			}
			validBids = validBids[excess:]
			bidCount = expectedPerSide
		}
	}
	if askCount > expectedPerSide {
		totalAskNotional := 0.0
		for _, o := range validAsks {
			totalAskNotional += o.Price * o.Qty
		}
		if totalAskNotional >= targetDepthPerSide || askCount > expectedPerSide*2 {
			sort.Slice(validAsks, func(i, j int) bool { return validAsks[i].Price > validAsks[j].Price })
			excess := askCount - expectedPerSide
			for i := 0; i < excess; i++ {
				log.Printf("[%s] CANCEL order=%s SELL @ %.8f — excess SELL (%d > %d), removing outermost", s.Name(), validAsks[i].OrderID, validAsks[i].Price, askCount, expectedPerSide)
				toCancel = append(toCancel, validAsks[i].OrderID)
			}
			validAsks = validAsks[excess:]
			askCount = expectedPerSide
		}
	}

	existingBidPrices := make([]float64, 0, len(validBids))
	existingAskPrices := make([]float64, 0, len(validAsks))
	for _, o := range validBids {
		existingBidPrices = append(existingBidPrices, o.Price)
	}
	for _, o := range validAsks {
		existingAskPrices = append(existingAskPrices, o.Price)
	}

	// priceExists: broad check (priceTolerance) used for Step 4 / cancel guards
	priceExists := func(price float64, existing []float64) bool {
		for _, ep := range existing {
			if math.Abs(ep-price) <= priceTolerance {
				return true
			}
		}
		return false
	}

	// priceSlotTaken: tight check (1 tick) used for partial replenishment and top-up
	// to avoid blocking valid slots that are just >1 tick away from existing orders
	priceSlotTaken := func(price float64, existing []float64) bool {
		for _, ep := range existing {
			if math.Abs(ep-price) <= s.tickSize {
				return true
			}
		}
		return false
	}

	timestamp := time.Now().UnixMilli() % 100000000
	batchID := fmt.Sprintf("%d%03d", timestamp, rand.Intn(1000))

	makeOrder := func(side string, price float64, level int) *core.DesiredOrder {
		sideTag := "B"
		if side == "SELL" {
			sideTag = "A"
		}
		price = s.roundToTick(price)
		if price <= 0 {
			return nil
		}
		notional := pyramidNotional(level, expectedPerSide, targetDepthPerSide, s.cfg.PyramidFactor)
		qty := s.roundToStep((notional * calculateSizeMultiplier() * sizeMult) / price)
		if qty <= 0 || price*qty < s.minNotional {
			return nil
		}
		if s.maxOrderQty > 0 && qty > s.maxOrderQty {
			qty = s.maxOrderQty
		}
		return &core.DesiredOrder{
			Side:       side,
			Price:      price,
			Qty:        qty,
			LevelIndex: level,
			Tag:        fmt.Sprintf("SM_%s_L%d_%s", batchID, level, sideTag),
		}
	}

	var toAdd []core.DesiredOrder

	// Step 2: add missing BUY orders
	if bidCount < expectedPerSide {
		if bidCount == 0 {
			// All BUYs missing → generate full side via spread allocation
			inv := calculateInventoryState(balance.BaseFree, balance.QuoteFree, mid, float64(s.cfg.InitBase), float64(s.cfg.InitQuote))
			spread := calculateSpreadAllocation(spreadMinBps, spreadMaxBps, inv.skew)
			orders := s.generateSideOrders(mid, spread.bidBps, depthBps, expectedPerSide, targetDepthPerSide, "BUY", batchID)
			for _, o := range orders {
				if sizeMult != 1.0 {
					o.Qty = s.roundToStep(o.Qty * sizeMult)
				}
				if o.Qty > 0 && o.Price*o.Qty >= s.minNotional {
					log.Printf("[%s] ADD BUY @ %.8f x %.2f — full side rebuild (all BUYs missing)", s.Name(), o.Price, o.Qty)
					toAdd = append(toAdd, o)
					existingBidPrices = append(existingBidPrices, o.Price)
				}
			}
		} else {
			// Partial missing → add outer orders below existing lowest bid
			avgStep := s.computeAvgStep(validBids, "BUY")
			if avgStep < s.tickSize {
				// fallback: spread remaining range across levels
				innerBid := mid * (1.0 - spreadMinBps/2/10000.0)
				avgStep = (innerBid - float64(depthFloor)) / float64(expectedPerSide)
				if avgStep < s.tickSize {
					avgStep = s.tickSize * 2
				}
			}
			// Cap step if level_step_bps is configured
			if s.cfg.LevelStepBps > 0 {
				maxStep := mid * s.cfg.LevelStepBps / 10000.0
				if avgStep > maxStep {
					avgStep = maxStep
				}
			}

			lowestBid := validBids[0].Price
			for _, o := range validBids {
				if o.Price < lowestBid {
					lowestBid = o.Price
				}
			}

			missing := expectedPerSide - bidCount
			for added := 0; added < missing; added++ {
				outerPrice := lowestBid - float64(added+1)*avgStep
				clamped := outerPrice < depthFloor
				if clamped {
					outerPrice = depthFloor
				}
				outerPrice = s.roundToTick(outerPrice)
				if outerPrice <= 0 || priceSlotTaken(outerPrice, existingBidPrices) {
					break
				}
				order := makeOrder("BUY", outerPrice, bidCount+added)
				if order == nil {
					break
				}
				log.Printf("[%s] ADD BUY @ %.8f — partial missing (%d/%d), outer below lowestBid=%.8f step=%.8f", s.Name(), outerPrice, bidCount, expectedPerSide, lowestBid, avgStep)
				toAdd = append(toAdd, *order)
				existingBidPrices = append(existingBidPrices, outerPrice)
			}
		}
		if len(toAdd) > 0 {
			log.Printf("[%s] Maintain BUY: had=%d expected=%d adding=%d", s.Name(), bidCount, expectedPerSide, len(toAdd))
		}
	}

	// Step 2.5: BUY notional top-up — if total BUY notional still below target, add extra orders.
	// Extra count is random between numLevels/2 and numLevels.
	{
		currentBidNotional := 0.0
		for _, o := range validBids {
			currentBidNotional += o.Price * o.Qty
		}
		for _, o := range toAdd {
			if o.Side == "BUY" {
				currentBidNotional += o.Price * o.Qty
			}
		}
		if currentBidNotional < targetDepthPerSide {
			half := expectedPerSide / 2
			if half < 1 {
				half = 1
			}
			extraCount := half + rand.Intn(expectedPerSide-half+1)
			lowestBid := math.MaxFloat64
			for _, o := range validBids {
				if o.Price < lowestBid {
					lowestBid = o.Price
				}
			}
			for _, o := range toAdd {
				if o.Side == "BUY" && o.Price < lowestBid {
					lowestBid = o.Price
				}
			}
			if lowestBid == math.MaxFloat64 {
				lowestBid = mid * (1.0 - spreadMinBps/2/10000.0)
			}
			deficit := targetDepthPerSide - currentBidNotional
			priceRange := lowestBid - depthFloor
			step := s.tickSize * 2
			if priceRange > 0 && extraCount > 0 {
				step = priceRange / float64(extraCount+1)
				if step < s.tickSize {
					step = s.tickSize * 2
				}
			}
			added := 0
			for i := 1; i <= extraCount; i++ {
				p := lowestBid - float64(i)*step
				if p < depthFloor {
					p = depthFloor
				}
				p = s.roundToTick(p)
				if p <= 0 || priceSlotTaken(p, existingBidPrices) {
					continue
				}
				unitNotional := deficit / float64(extraCount)
				qty := s.roundToStep((unitNotional * calculateSizeMultiplier() * sizeMult) / p)
				if qty <= 0 || p*qty < s.minNotional {
					qty = s.roundToStep(s.minNotional / p)
					if p*qty < s.minNotional {
						qty += s.stepSize
						qty = s.roundToStep(qty)
					}
				}
				if s.maxOrderQty > 0 && qty > s.maxOrderQty {
					qty = s.maxOrderQty
				}
				if qty <= 0 || p*qty < s.minNotional {
					continue
				}
				o := core.DesiredOrder{
					Side:       "BUY",
					Price:      p,
					Qty:        qty,
					LevelIndex: bidCount + added,
					Tag:        fmt.Sprintf("SM_%s_L%d_B", batchID, bidCount+added),
				}
				log.Printf("[%s] ADD BUY @ %.8f x %.2f — notional top-up (%d/%d, deficit=$%.2f)", s.Name(), p, qty, added+1, extraCount, deficit)
				toAdd = append(toAdd, o)
				existingBidPrices = append(existingBidPrices, p)
				added++
			}
			if added > 0 {
				log.Printf("[%s] BUY notional top-up: had=$%.2f target=$%.2f added=%d", s.Name(), currentBidNotional, targetDepthPerSide, added)
			}
		}
	}

	// Step 3: add missing SELL orders
	addedAsks := 0
	if askCount < expectedPerSide {
		if askCount == 0 {
			// All SELLs missing → generate full side
			inv := calculateInventoryState(balance.BaseFree, balance.QuoteFree, mid, float64(s.cfg.InitBase), float64(s.cfg.InitQuote))
			spread := calculateSpreadAllocation(spreadMinBps, spreadMaxBps, inv.skew)
			orders := s.generateSideOrders(mid, spread.askBps, depthBps, expectedPerSide, targetDepthPerSide, "SELL", batchID)
			for _, o := range orders {
				if sizeMult != 1.0 {
					o.Qty = s.roundToStep(o.Qty * sizeMult)
				}
				if o.Qty > 0 && o.Price*o.Qty >= s.minNotional {
					log.Printf("[%s] ADD SELL @ %.8f x %.2f — full side rebuild (all SELLs missing)", s.Name(), o.Price, o.Qty)
					toAdd = append(toAdd, o)
					existingAskPrices = append(existingAskPrices, o.Price)
					addedAsks++
				}
			}
		} else {
			// Partial missing → add outer orders above existing highest ask
			avgStep := s.computeAvgStep(validAsks, "SELL")
			if avgStep < s.tickSize {
				innerAsk := mid * (1.0 + spreadMinBps/2/10000.0)
				avgStep = (float64(depthCeil) - innerAsk) / float64(expectedPerSide)
				if avgStep < s.tickSize {
					avgStep = s.tickSize * 2
				}
			}
			// Cap step if level_step_bps is configured
			if s.cfg.LevelStepBps > 0 {
				maxStep := mid * s.cfg.LevelStepBps / 10000.0
				if avgStep > maxStep {
					avgStep = maxStep
				}
			}

			highestAsk := validAsks[0].Price
			for _, o := range validAsks {
				if o.Price > highestAsk {
					highestAsk = o.Price
				}
			}

			missing := expectedPerSide - askCount
			for added := 0; added < missing; added++ {
				outerPrice := highestAsk + float64(added+1)*avgStep
				clamped := outerPrice > depthCeil
				if clamped {
					outerPrice = depthCeil
				}
				outerPrice = s.roundToTick(outerPrice)
				if outerPrice <= 0 || priceSlotTaken(outerPrice, existingAskPrices) {
					break
				}
				order := makeOrder("SELL", outerPrice, askCount+added)
				if order == nil {
					break
				}
				log.Printf("[%s] ADD SELL @ %.8f — partial missing (%d/%d), outer above highestAsk=%.8f step=%.8f", s.Name(), outerPrice, askCount, expectedPerSide, highestAsk, avgStep)
				toAdd = append(toAdd, *order)
				existingAskPrices = append(existingAskPrices, outerPrice)
				addedAsks++
			}
		}
		if addedAsks > 0 {
			log.Printf("[%s] Maintain ASK: had=%d expected=%d adding=%d", s.Name(), askCount, expectedPerSide, addedAsks)
		}
	}

	// Step 3.5: SELL notional top-up — if total SELL notional still below target,
	// pick a random outer SELL order (from outer half of ladder) and replace it
	// with a larger one that absorbs the deficit.
	{
		currentAskNotional := 0.0
		for _, o := range validAsks {
			currentAskNotional += o.Price * o.Qty
		}
		for _, o := range toAdd {
			if o.Side == "SELL" {
				currentAskNotional += o.Price * o.Qty
			}
		}
		if currentAskNotional < targetDepthPerSide && len(validAsks) > 0 {
			deficit := targetDepthPerSide - currentAskNotional

			// Sort by price descending — outermost (highest) first
			sortedAsks := make([]core.LiveOrder, len(validAsks))
			copy(sortedAsks, validAsks)
			sort.Slice(sortedAsks, func(i, j int) bool {
				return sortedAsks[i].Price > sortedAsks[j].Price
			})

			// Candidates: outer half (highest prices)
			outerCount := len(sortedAsks) / 2
			if outerCount < 1 {
				outerCount = 1
			}
			candidates := sortedAsks[:outerCount]
			target := candidates[rand.Intn(len(candidates))]

			// New qty absorbs original notional + deficit
			newNotional := target.Price*target.Qty + deficit
			newQty := s.roundToStep(newNotional / target.Price)
			if s.maxOrderQty > 0 && newQty > s.maxOrderQty {
				newQty = s.maxOrderQty
			}
			if newQty > target.Qty && newQty*target.Price >= s.minNotional {
				toCancel = append(toCancel, target.OrderID)
				o := core.DesiredOrder{
					Side:       "SELL",
					Price:      target.Price,
					Qty:        newQty,
					LevelIndex: target.LevelIndex,
					Tag:        fmt.Sprintf("SM_%s_L%d_A", batchID, target.LevelIndex),
				}
				log.Printf("[%s] SELL notional top-up: replace outer L%d @ %.8f qty %.2f→%.2f (deficit=$%.2f)",
					s.Name(), target.LevelIndex, target.Price, target.Qty, newQty, deficit)
				toAdd = append(toAdd, o)
			}
		}
	}

	// Step 4: fill-driven opposite-side response.
	// When a BUY is filled → add inner SELL near mid + cancel outermost SELL (notional stable).
	// When a SELL is filled → add inner BUY near mid + cancel outermost BUY (notional stable).
	// Guard: inner price must not cross the market (avoid taker fill).
	if bidFilled {
		innerSellPrice := s.roundToTick(mid + spreadMinBps/2/10000.0*mid)
		priceOk := innerSellPrice > marketBestBid
		dupOk := !priceExists(innerSellPrice, existingAskPrices)
		if !priceOk || !dupOk {
			log.Printf("[%s] Step4 BUY fill: skip SELL inner=%.8f marketBid=%.8f priceOk=%v dupOk=%v",
				s.Name(), innerSellPrice, marketBestBid, priceOk, dupOk)
		}
		if priceOk && dupOk {
			order := makeOrder("SELL", innerSellPrice, 0)
			if order == nil {
				log.Printf("[%s] Step4 BUY fill: makeOrder nil inner=%.8f", s.Name(), innerSellPrice)
			}
			if order != nil {
				// Cancel current innermost SELL (L0) and replace with new SELL closer to mid
				innermostSell := ""
				innermostSellPrice := math.MaxFloat64
				for _, o := range validAsks {
					if o.Price < innermostSellPrice {
						innermostSellPrice = o.Price
						innermostSell = o.OrderID
					}
				}
				if innermostSell != "" {
					log.Printf("[%s] CANCEL SELL @ %.8f — shift inner SELL closer to mid (BUY fill response)", s.Name(), innermostSellPrice)
					toCancel = append(toCancel, innermostSell)
				}
				log.Printf("[%s] ADD SELL @ %.8f — fill response: BUY filled, inner SELL (marketBid=%.8f)", s.Name(), innerSellPrice, marketBestBid)
				toAdd = append(toAdd, *order)
			}
		}
	}
	if askFilled {
		innerBuyPrice := s.roundToTick(mid - spreadMinBps/2/10000.0*mid)
		priceOkAsk := innerBuyPrice < marketBestAsk
		dupOkAsk := !priceExists(innerBuyPrice, existingBidPrices)
		if !priceOkAsk || !dupOkAsk {
			log.Printf("[%s] Step4 SELL fill: skip BUY inner=%.8f marketAsk=%.8f priceOk=%v dupOk=%v",
				s.Name(), innerBuyPrice, marketBestAsk, priceOkAsk, dupOkAsk)
		}
		if priceOkAsk && dupOkAsk {
			order := makeOrder("BUY", innerBuyPrice, 0)
			if order != nil {
				// Cancel current innermost BUY (L0) and replace with new BUY closer to mid
				innermostBuy := ""
				innermostBuyPrice := 0.0
				for _, o := range validBids {
					if o.Price > innermostBuyPrice {
						innermostBuyPrice = o.Price
						innermostBuy = o.OrderID
					}
				}
				if innermostBuy != "" {
					log.Printf("[%s] CANCEL BUY @ %.8f — shift inner BUY closer to mid (SELL fill response)", s.Name(), innermostBuyPrice)
					toCancel = append(toCancel, innermostBuy)
				}
				log.Printf("[%s] ADD BUY @ %.8f — fill response: SELL filled, inner BUY (marketAsk=%.8f)", s.Name(), innerBuyPrice, marketBestAsk)
				toAdd = append(toAdd, *order)
			}
		}
	}

	if len(toCancel) == 0 && len(toAdd) == 0 {
		return nil
	}

	return &core.TickOutput{
		Action:         core.TickActionAmend,
		OrdersToCancel: toCancel,
		OrdersToAdd:    toAdd,
		Reason:         fmt.Sprintf("maintain: bid=%d/%d ask=%d/%d cancel=%d add=%d bidFilled=%v askFilled=%v", bidCount, expectedPerSide, askCount, expectedPerSide, len(toCancel), len(toAdd), bidFilled, askFilled),
	}
}

// UpdatePrevSnapshot saves post-execution live order counts and balance for fill detection.
// Called by bot after syncLiveOrders + getBalanceState following order execution.
func (s *SimpleLadderStrategy) UpdatePrevSnapshot(liveOrders []core.LiveOrder, balance *core.BalanceState) {
	bidCount, askCount := 0, 0
	for _, o := range liveOrders {
		if o.Side == "BUY" {
			bidCount++
		} else {
			askCount++
		}
	}
	s.prevBidCount = bidCount
	s.prevAskCount = askCount
	s.prevBaseFree = balance.BaseFree
	s.prevQuoteFree = balance.QuoteFree
}

// roundToTick rounds price to tick size
func (s *SimpleLadderStrategy) roundToTick(price float64) float64 {
	if s.tickSize <= 0 {
		return price
	}
	return math.Round(price/s.tickSize) * s.tickSize
}

// floorToTick rounds price DOWN to tick size (ensures price doesn't exceed max)
func (s *SimpleLadderStrategy) floorToTick(price float64) float64 {
	if s.tickSize <= 0 {
		return price
	}
	return math.Floor(price/s.tickSize) * s.tickSize
}

// ceilToTick rounds price UP to tick size (ensures price doesn't go below min)
func (s *SimpleLadderStrategy) ceilToTick(price float64) float64 {
	if s.tickSize <= 0 {
		return price
	}
	return math.Ceil(price/s.tickSize) * s.tickSize
}

// roundToStep rounds quantity to step size
func (s *SimpleLadderStrategy) roundToStep(qty float64) float64 {
	if s.stepSize <= 0 {
		return qty
	}
	return math.Floor(qty/s.stepSize) * s.stepSize
}

// removeDuplicatePrices removes duplicate prices from the slice
func (s *SimpleLadderStrategy) removeDuplicatePrices(prices []float64) []float64 {
	if len(prices) <= 1 {
		return prices
	}

	// Validate tickSize to prevent division by zero
	tickSize := s.tickSize
	if tickSize <= 0 {
		tickSize = 1e-8 // Fallback to 8 decimal places
	}

	seen := make(map[float64]bool)
	result := make([]float64, 0, len(prices))

	for _, p := range prices {
		// Skip invalid prices
		if p <= 0 || math.IsNaN(p) || math.IsInf(p, 0) {
			continue
		}
		// Round to avoid floating point comparison issues
		key := math.Round(p/tickSize) * tickSize
		if !seen[key] {
			seen[key] = true
			result = append(result, p)
		}
	}

	return result
}
