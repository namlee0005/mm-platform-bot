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

// Convergence constants
const (
	// Default tolerance for relist: if live order is within this tolerance
	// of the desired price, keep it to avoid API churn
	defaultRelistToleranceBps = 25.0

	// Quantity tolerance: if live qty differs by more than this ratio, replace
	defaultQtyTolerance = 0.3

	// Safety buffer from depth boundary to avoid exchange rejection
	// (bot mid can differ from exchange reference price)
	// Outer orders always 10-15bps inside depth boundary
	outerSafetyBps = 15.0
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
	vwapWindowMinutes = 3 // Use 3 minutes of trade history

	// Maximum age of VWAP before considering it stale
	maxVWAPStalenessMinutes = 30 // Warn if VWAP older than 30 minutes

	// Minimum number of trades required for reliable VWAP
	minTradesForVWAP = 3

	// Inventory adjustment strength: bps per 10% inventory skew
	// Example: 10% skew → ±50 bps adjustment (500 → ~10bps at 19% skew)
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
	RelistToleranceBps  float64 `json:"relist_tolerance_bps"`   // Drift tolerance before relisting (default 15bps)

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

	// Cached noise for stable desired orders across ticks
	cachedBidPriceJitter []float64 // per-level price jitter in ticks (signed)
	cachedAskPriceJitter []float64
	cachedBidQtyNoise    []float64 // per-level qty multiplier (0.8-1.2)
	cachedAskQtyNoise    []float64
	cachedNoiseMid       float64 // mid when noise was generated

	// Fill tracking for inner gap protection
	lastBidFillTime time.Time // last time a BUY order was filled
	lastAskFillTime time.Time // last time a SELL order was filled
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
		cfg.PyramidFactor = 2.0 // outer orders are 2x the size of inner orders
	}
	if cfg.LevelGapTicksMax == 0 {
		cfg.LevelGapTicksMax = 3
	}
	if cfg.RelistToleranceBps == 0 {
		cfg.RelistToleranceBps = defaultRelistToleranceBps
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
		cfg:         cfg,
		navHistory:  make([]navSnapshot, 0, 1000),
		currentMode: ModeNormal,
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

	// Auto-detect InitBase/InitQuote from current balance if not configured
	if s.cfg.InitBase == 0 && s.cfg.InitQuote == 0 {
		s.cfg.InitBase = balance.TotalBase()
		s.cfg.InitQuote = balance.TotalQuote()
		log.Printf("[%s] Auto-detected init inventory: base=%.2f, quote=%.2f",
			s.Name(), s.cfg.InitBase, s.cfg.InitQuote)
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
		"nav":        nav,
		"drawdown":   drawdown,
		"size_mult":  sizeMult,
		"fair_price": mid,
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

	now := time.Now()
	if event.Side == "BUY" {
		s.lastBidFillTime = now
	} else {
		s.lastAskFillTime = now
	}
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
		// Valid VWAP — blend with last trade price (Option B)
		// fairPrice = 0.9 × vwap + 0.1 × lastTradePrice
		// Small lastTrade weight: enough to reduce lag, not enough to cause churn from noise
		vwap := vwapRes.price
		isFresh = true

		// Find last trade price (most recent trade in slice)
		lastTradePrice := vwap // fallback to vwap if no trades
		if len(trades) > 0 {
			latest := trades[0]
			for _, t := range trades[1:] {
				if t.Timestamp.After(latest.Timestamp) {
					latest = t
				}
			}
			lastTradePrice = latest.Price
		}

		basePrice = 0.9*vwap + 0.1*lastTradePrice

		// Update cache
		s.lastVWAP = vwap
		s.lastVWAPTime = time.Now()

		// Log VWAP calculation
		if vwapRes.age > time.Duration(maxVWAPStalenessMinutes)*time.Minute {
			log.Printf("[FairPrice] ⚠️  VWAP stale: %.8f (trades=%d, age=%v, volume=%.2f)",
				vwap, vwapRes.tradeCount, vwapRes.age, vwapRes.totalVolume)
		} else {
			log.Printf("[FairPrice] VWAP: %.8f (trades=%d, age=%v, volume=%.2f)",
				vwap, vwapRes.tradeCount, vwapRes.age, vwapRes.totalVolume)
		}
		log.Printf("[FairPrice] Blend: 0.9×vwap(%.8f) + 0.1×last(%.8f) = %.8f",
			vwap, lastTradePrice, basePrice)
	}

	// Calculate inventory state
	inv := calculateInventoryState(balance.TotalBase(), balance.TotalQuote(), basePrice, float64(s.cfg.InitBase), float64(s.cfg.InitQuote))

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

	log.Printf("[FairPrice] mid=%.8f (blend=%.8f, inv_adj=%.8f)", fairPrice, basePrice, adjustment)

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

// refreshNoiseIfNeeded regenerates cached noise when mid has shifted significantly (>1%)
// or when noise hasn't been generated yet. This ensures desired orders are stable
// across ticks while still appearing organic.
func (s *SimpleLadderStrategy) refreshNoiseIfNeeded(mid float64, numLevels int) {
	// Refresh if: no cache, wrong size, or mid shifted >0.3%
	needRefresh := len(s.cachedBidPriceJitter) != numLevels ||
		len(s.cachedBidQtyNoise) != numLevels ||
		s.cachedNoiseMid == 0 ||
		math.Abs(mid-s.cachedNoiseMid)/s.cachedNoiseMid > 0.003

	if !needRefresh {
		return
	}

	generateNoise := func() ([]float64, []float64) {
		jitter := make([]float64, numLevels)
		noise := make([]float64, numLevels)
		for i := 0; i < numLevels; i++ {
			if i > 0 && i < numLevels-1 {
				jitterTicks := float64(rand.Intn(5) + 2)
				jitter[i] = (rand.Float64()*2 - 1) * jitterTicks
			}
			noise[i] = 0.80 + rand.Float64()*0.40
		}
		return jitter, noise
	}

	s.cachedBidPriceJitter, s.cachedBidQtyNoise = generateNoise()
	s.cachedAskPriceJitter, s.cachedAskQtyNoise = generateNoise()
	s.cachedNoiseMid = mid
}

// logSpacedPrices generates numLevels prices between inner and outer using
// logarithmic spacing (dense near inner, sparse near outer) with cached jitter.
// Returns prices sorted inner→outer (descending for BUY, ascending for SELL).
func (s *SimpleLadderStrategy) logSpacedPrices(inner, outer float64, numLevels int, side string) []float64 {
	if numLevels <= 0 {
		return nil
	}
	if numLevels == 1 {
		return []float64{s.roundToTick(inner)}
	}

	jitterCache := s.cachedBidPriceJitter
	if side == "SELL" {
		jitterCache = s.cachedAskPriceJitter
	}

	priceRange := math.Abs(outer - inner)
	direction := 1.0
	if outer < inner {
		direction = -1.0 // BUY side: inner > outer
	}

	prices := make([]float64, 0, numLevels)
	logBase := math.Log(1 + float64(numLevels-1))

	for i := 0; i < numLevels; i++ {
		// Logarithmic interpolation: dense near inner, sparse near outer
		t := math.Log(1+float64(i)) / logBase
		basePrice := inner + direction*t*priceRange

		// Apply cached jitter for middle levels
		if i > 0 && i < numLevels-1 && i < len(jitterCache) {
			basePrice += jitterCache[i] * s.tickSize
		}

		price := s.roundToTick(basePrice)
		if price <= 0 {
			continue
		}
		prices = append(prices, price)
	}

	// Remove duplicates
	prices = s.removeDuplicatePrices(prices)

	// If dedup reduced count, pad with outer prices (2 ticks apart) to maintain numLevels
	for len(prices) < numLevels {
		lastPrice := prices[len(prices)-1]
		nextPrice := s.roundToTick(lastPrice + direction*s.tickSize*2)
		if nextPrice <= 0 {
			break
		}
		prices = append(prices, nextPrice)
	}

	return prices
}

// noisyPyramidWeights generates normalized pyramid weights with ±20% noise.
// Level 0 (inner) gets weight ~1.0, level N-1 (outer) gets weight ~pyramidFactor.
// Total weights sum to 1.0 so multiplying by targetDepth gives exact notional.
func (s *SimpleLadderStrategy) noisyPyramidWeights(numLevels int, pyramidFactor float64, side string) []float64 {
	if numLevels <= 0 {
		return nil
	}
	if pyramidFactor <= 0 {
		pyramidFactor = 1.0
	}

	noiseCache := s.cachedBidQtyNoise
	if side == "SELL" {
		noiseCache = s.cachedAskQtyNoise
	}

	weights := make([]float64, numLevels)
	totalW := 0.0
	for i := 0; i < numLevels; i++ {
		w := 1.0
		if numLevels > 1 {
			w = 1.0 + (pyramidFactor-1.0)*float64(i)/float64(numLevels-1)
		}
		// Apply cached noise for organic appearance
		if i < len(noiseCache) {
			w *= noiseCache[i]
		}
		weights[i] = w
		totalW += w
	}
	// Normalize so sum = 1.0
	if totalW > 0 {
		for i := range weights {
			weights[i] /= totalW
		}
	}
	return weights
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

// computeDesiredOrders computes the exact set of orders desired on the book right now.
// Pure function based on current mid, balance, and inventory — no state from previous cycle.
func (s *SimpleLadderStrategy) computeDesiredOrders(mid, marketBestBid, marketBestAsk float64, balance *core.BalanceState, sizeMult float64) []core.DesiredOrder {
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
	numLevels := s.cfg.NumLevels
	if numLevels <= 0 {
		numLevels = 10
	}
	targetDepth := s.cfg.TargetDepthNotional
	pyramidFactor := s.cfg.PyramidFactor
	if pyramidFactor <= 0 {
		pyramidFactor = 2.0
	}

	// Refresh cached noise if mid shifted significantly
	s.refreshNoiseIfNeeded(mid, numLevels)

	// 1. Inventory → spread allocation
	inv := calculateInventoryState(balance.TotalBase(), balance.TotalQuote(), mid, float64(s.cfg.InitBase), float64(s.cfg.InitQuote))
	spread := calculateSpreadAllocation(spreadMinBps, spreadMaxBps, inv.skew)

	var desired []core.DesiredOrder

	// 2. BUY side: log spacing + noisy pyramid
	bidInner := s.roundToTick(mid * (1.0 - spread.bidBps/10000.0))
	bidOuter := s.roundToTick(mid * (1.0 - (depthBps-outerSafetyBps)/10000.0))
	if bidOuter < s.tickSize {
		bidOuter = s.tickSize
	}
	bidPrices := s.logSpacedPrices(bidInner, bidOuter, numLevels, "BUY")
	// Sort descending (inner first = highest price for BUY)
	sort.Sort(sort.Reverse(sort.Float64Slice(bidPrices)))
	bidWeights := s.noisyPyramidWeights(len(bidPrices), pyramidFactor, "BUY")

	for i, price := range bidPrices {
		if price <= 0 || price >= mid {
			continue
		}
		// Anti-taker guard: must be at least 1 tick below best ask
		if marketBestAsk > 0 && price >= marketBestAsk-s.tickSize {
			continue
		}
		notional := targetDepth * bidWeights[i]
		qty := s.roundToStep(notional * sizeMult / price)
		if qty <= 0 || price*qty < s.minNotional {
			continue
		}
		if s.maxOrderQty > 0 && qty > s.maxOrderQty {
			qty = s.maxOrderQty
		}
		desired = append(desired, core.DesiredOrder{
			Side:       "BUY",
			Price:      price,
			Qty:        qty,
			LevelIndex: i,
		})
	}

	// 3. SELL side: independent random (different spacing + weights)
	askInner := s.roundToTick(mid * (1.0 + spread.askBps/10000.0))
	askOuter := s.roundToTick(mid * (1.0 + (depthBps-outerSafetyBps)/10000.0))
	askPrices := s.logSpacedPrices(askInner, askOuter, numLevels, "SELL")
	// Sort ascending (inner first = lowest price for SELL)
	sort.Float64s(askPrices)
	askWeights := s.noisyPyramidWeights(len(askPrices), pyramidFactor, "SELL")

	for i, price := range askPrices {
		if price <= 0 || price <= mid {
			continue
		}
		// Anti-taker guard: must be at least 1 tick above best bid
		if marketBestBid > 0 && price <= marketBestBid+s.tickSize {
			continue
		}
		notional := targetDepth * askWeights[i]
		qty := s.roundToStep(notional * sizeMult / price)
		if qty <= 0 || price*qty < s.minNotional {
			continue
		}
		if s.maxOrderQty > 0 && qty > s.maxOrderQty {
			qty = s.maxOrderQty
		}
		desired = append(desired, core.DesiredOrder{
			Side:       "SELL",
			Price:      price,
			Qty:        qty,
			LevelIndex: i,
		})
	}

	// 4. Budget constraint: scale down if balance insufficient
	desired = s.applyBudgetConstraint(desired, balance, mid)

	return desired
}

// applyBudgetConstraint scales down orders when balance is insufficient for target depth.
// Scales proportionally first, then drops orders below minNotional (outer first since sorted inner→outer).
func (s *SimpleLadderStrategy) applyBudgetConstraint(desired []core.DesiredOrder, balance *core.BalanceState, mid float64) []core.DesiredOrder {
	var bidTotal, askTotal float64
	for _, d := range desired {
		if d.Side == "BUY" {
			bidTotal += d.Price * d.Qty
		} else {
			askTotal += d.Price * d.Qty
		}
	}

	// Use total balance (free + locked) since desired orders are computed from scratch
	// and converge can cancel/replace any existing order.
	availableQuote := balance.QuoteFree + balance.QuoteLocked
	availableBase := balance.BaseFree + balance.BaseLocked

	bidScale := 1.0
	if bidTotal > 0 && availableQuote < bidTotal {
		bidScale = availableQuote / bidTotal
	}
	askScale := 1.0
	askValue := availableBase * mid
	if askTotal > 0 && askValue < askTotal {
		askScale = askValue / askTotal
	}

	if bidScale >= 1.0 && askScale >= 1.0 {
		return desired // No scaling needed
	}

	var result []core.DesiredOrder
	for _, d := range desired {
		scale := bidScale
		if d.Side == "SELL" {
			scale = askScale
		}
		if scale >= 1.0 {
			result = append(result, d)
			continue
		}
		d.Qty = s.roundToStep(d.Qty * scale)
		if d.Qty > 0 && d.Price*d.Qty >= s.minNotional {
			result = append(result, d)
		}
		// else: dropped (outer levels first since ordered inner→outer)
	}

	if bidScale < 1.0 {
		log.Printf("[%s] Budget constraint: BUY scaled to %.0f%% (available=$%.2f, target=$%.2f)",
			s.Name(), bidScale*100, availableQuote, bidTotal)
	}
	if askScale < 1.0 {
		log.Printf("[%s] Budget constraint: SELL scaled to %.0f%% (available=$%.2f, target=$%.2f)",
			s.Name(), askScale*100, askValue, askTotal)
	}

	return result
}

// converge compares desired orders with live orders and returns cancel/create lists.
// Uses price-matching: each desired order finds the closest live order by price.
// Hybrid approach: after a fill, only creates OUTER orders (not inner) to avoid
// "propping up" price for someone dumping. Inner gaps fill naturally as mid shifts
// over subsequent ticks.
func (s *SimpleLadderStrategy) converge(desired []core.DesiredOrder, live []core.LiveOrder) (toCancel []string, toCreate []core.DesiredOrder) {
	tolerance := s.cfg.RelistToleranceBps / 10000.0
	if tolerance <= 0 {
		tolerance = defaultRelistToleranceBps / 10000.0
	}

	// Split by side
	var desiredBuys, desiredSells []core.DesiredOrder
	for _, d := range desired {
		if d.Side == "BUY" {
			desiredBuys = append(desiredBuys, d)
		} else {
			desiredSells = append(desiredSells, d)
		}
	}
	var liveBuys, liveSells []core.LiveOrder
	for _, lo := range live {
		if lo.Side == "BUY" {
			liveBuys = append(liveBuys, lo)
		} else {
			liveSells = append(liveSells, lo)
		}
	}

	// Determine if fill gap protection is active (within 1m of a fill)
	const fillGapWindow = 1 * time.Minute
	now := time.Now()
	bidFillActive := !s.lastBidFillTime.IsZero() && now.Sub(s.lastBidFillTime) < fillGapWindow
	askFillActive := !s.lastAskFillTime.IsZero() && now.Sub(s.lastAskFillTime) < fillGapWindow

	// Find innermost live price per side (only needed when fill gap is active)
	innermostLiveBid := 0.0
	innermostLiveAsk := math.MaxFloat64
	if bidFillActive {
		for _, lo := range liveBuys {
			if lo.Price > innermostLiveBid {
				innermostLiveBid = lo.Price
			}
		}
	}
	if askFillActive {
		for _, lo := range liveSells {
			if lo.Price < innermostLiveAsk {
				innermostLiveAsk = lo.Price
			}
		}
	}

	// matchSide: for each desired order, find closest live order by price.
	// Unmatched desired → create (or skip if inner gap after recent fill).
	// Unmatched live → cancel (orphans).
	matchSide := func(desiredSide []core.DesiredOrder, liveSide []core.LiveOrder, side string) {
		used := make([]bool, len(liveSide))

		for _, d := range desiredSide {
			bestIdx := -1
			bestDiff := math.MaxFloat64

			for j, lo := range liveSide {
				if used[j] {
					continue
				}
				diff := math.Abs(lo.Price-d.Price) / d.Price
				if diff < bestDiff {
					bestDiff = diff
					bestIdx = j
				}
			}

			if bestIdx >= 0 && bestDiff <= tolerance {
				// Live order close enough → keep it
				used[bestIdx] = true
				// Check qty tolerance — if qty drifted too much, replace
				qtyDiff := 0.0
				if d.Qty > 0 {
					qtyDiff = math.Abs(liveSide[bestIdx].RemainingQty-d.Qty) / d.Qty
				}
				if qtyDiff > defaultQtyTolerance {
					toCancel = append(toCancel, liveSide[bestIdx].OrderID)
					toCreate = append(toCreate, d)
					used[bestIdx] = false
				}
			} else {
				// No live order close enough — check if inner gap after recent fill
				isInnerGap := false
				if side == "BUY" && bidFillActive && len(liveSide) > 0 && d.Price > innermostLiveBid {
					isInnerGap = true
				}
				if side == "SELL" && askFillActive && len(liveSide) > 0 && d.Price < innermostLiveAsk {
					isInnerGap = true
				}

				if isInnerGap {
					log.Printf("[%s] converge: skip inner %s @ %.8f (fill gap, expires in %.0fs)",
						s.Name(), side, d.Price, fillGapWindow.Seconds()-now.Sub(func() time.Time {
							if side == "BUY" {
								return s.lastBidFillTime
							}
							return s.lastAskFillTime
						}()).Seconds())
				} else {
					toCreate = append(toCreate, d)
				}
			}
		}

		// Cancel unmatched live orders (orphans — outside desired range)
		for j, lo := range liveSide {
			if !used[j] {
				toCancel = append(toCancel, lo.OrderID)
			}
		}
	}

	matchSide(desiredBuys, liveBuys, "BUY")
	matchSide(desiredSells, liveSells, "SELL")

	return toCancel, toCreate
}

// maintainLadder computes the desired order set and diffs it against live orders.
// Returns nil (KEEP) when no changes needed.
func (s *SimpleLadderStrategy) maintainLadder(mid, marketBestBid, marketBestAsk float64, balance *core.BalanceState, liveOrders []core.LiveOrder, sizeMult float64) *core.TickOutput {
	desired := s.computeDesiredOrders(mid, marketBestBid, marketBestAsk, balance, sizeMult)
	toCancel, toCreate := s.converge(desired, liveOrders)

	if len(toCancel) == 0 && len(toCreate) == 0 {
		return nil
	}

	// Post-converge budget check: trim new orders to fit actual free balance.
	// applyBudgetConstraint uses total balance (free+locked) which is correct for
	// the full desired set. But converge keeps existing orders (already locking balance),
	// so new orders must fit in free balance + balance freed by cancellations.
	toCreate = s.trimNewOrdersToBudget(toCreate, toCancel, liveOrders, balance, mid)

	if len(toCancel) == 0 && len(toCreate) == 0 {
		return nil
	}

	// Log summary
	bidCount, askCount := 0, 0
	for _, d := range desired {
		if d.Side == "BUY" {
			bidCount++
		} else {
			askCount++
		}
	}

	return &core.TickOutput{
		Action:         core.TickActionAmend,
		OrdersToCancel: toCancel,
		OrdersToAdd:    toCreate,
		Reason: fmt.Sprintf("converge: desired=%d (bid=%d ask=%d) cancel=%d create=%d",
			len(desired), bidCount, askCount, len(toCancel), len(toCreate)),
	}
}

// trimNewOrdersToBudget ensures new orders fit within actual available balance.
// Available = free balance + balance freed by cancelled orders.
// Drops outermost new orders first (highest LevelIndex) when budget is exceeded.
func (s *SimpleLadderStrategy) trimNewOrdersToBudget(toCreate []core.DesiredOrder, toCancel []string, liveOrders []core.LiveOrder, balance *core.BalanceState, mid float64) []core.DesiredOrder {
	if len(toCreate) == 0 {
		return toCreate
	}

	// Build lookup of cancelled order IDs
	cancelSet := make(map[string]bool, len(toCancel))
	for _, id := range toCancel {
		cancelSet[id] = true
	}

	// Calculate balance freed by cancellations
	var freedQuote, freedBase float64
	for _, lo := range liveOrders {
		if !cancelSet[lo.OrderID] {
			continue
		}
		if lo.Side == "BUY" {
			freedQuote += lo.Price * lo.RemainingQty
		} else {
			freedBase += lo.RemainingQty
		}
	}

	// Available budget for new orders
	availQuote := balance.QuoteFree + freedQuote
	availBase := balance.BaseFree + freedBase

	// Sum new order requirements per side
	var newBuyNotional, newSellBase float64
	for _, d := range toCreate {
		if d.Side == "BUY" {
			newBuyNotional += d.Price * d.Qty
		} else {
			newSellBase += d.Qty
		}
	}

	// Check if we need to trim
	buyOk := newBuyNotional <= availQuote*0.99 // 1% margin for rounding
	sellOk := newSellBase <= availBase*0.99
	if buyOk && sellOk {
		return toCreate
	}

	// Sort new orders: inner first (low LevelIndex), outer last (high LevelIndex)
	// Drop outermost orders first to stay within budget
	sort.Slice(toCreate, func(i, j int) bool {
		if toCreate[i].Side != toCreate[j].Side {
			return toCreate[i].Side < toCreate[j].Side // BUY before SELL
		}
		return toCreate[i].LevelIndex < toCreate[j].LevelIndex
	})

	var result []core.DesiredOrder
	var usedQuote, usedBase float64
	for _, d := range toCreate {
		if d.Side == "BUY" {
			notional := d.Price * d.Qty
			if usedQuote+notional > availQuote*0.99 {
				log.Printf("[%s] trimBudget: dropping BUY L%d @ %.8f (need $%.2f, avail $%.2f)",
					s.Name(), d.LevelIndex, d.Price, usedQuote+notional, availQuote)
				continue
			}
			usedQuote += notional
		} else {
			if usedBase+d.Qty > availBase*0.99 {
				log.Printf("[%s] trimBudget: dropping SELL L%d @ %.8f (need %.2f, avail %.2f base)",
					s.Name(), d.LevelIndex, d.Price, usedBase+d.Qty, availBase)
				continue
			}
			usedBase += d.Qty
		}
		result = append(result, d)
	}

	if len(result) < len(toCreate) {
		log.Printf("[%s] trimBudget: %d→%d orders (quoteFree=%.2f+freed=%.2f, baseFree=%.2f+freed=%.2f)",
			s.Name(), len(toCreate), len(result), balance.QuoteFree, freedQuote, balance.BaseFree, freedBase)
	}

	return result
}

// UpdatePrevSnapshot is a no-op in the convergence model.
// Kept for Strategy interface compliance.
func (s *SimpleLadderStrategy) UpdatePrevSnapshot(liveOrders []core.LiveOrder, balance *core.BalanceState) {
	// No-op: stateless convergence model doesn't need previous cycle state
}

// generateSideOrders is kept for backward compatibility with tests.
// New code should use computeDesiredOrders instead.
func (s *SimpleLadderStrategy) generateSideOrders(mid, firstLevelSpreadBps, depthBps float64, numLevels int, targetDepth float64, side string, batchID string) []core.DesiredOrder {
	// First level is at firstLevelSpreadBps from mid (calculated in computeDesiredOrders)
	// Other levels spread from first level to depthBps

	// Calculate price range for ladder
	// First level: at firstLevelSpreadBps from mid (randomized)
	// Last level: at depthBps from mid
	// Reserve 5bps safety buffer on the outermost level so exchange price-range
	// filters (typically ±2%) don't reject it when the exchange reference price
	// differs slightly from our mid.
	const outerSafetyBps = 15

	var firstPrice, lastPrice float64
	if side == "BUY" {
		firstPrice = mid * (1.0 - firstLevelSpreadBps/10000.0)
		lastPrice = mid * (1.0 - (depthBps-outerSafetyBps)/10000.0)
		// Ensure lastPrice is at least tickSize (for depthBps >= 10000)
		if lastPrice < s.tickSize {
			lastPrice = s.tickSize
		}
	} else {
		firstPrice = mid * (1.0 + firstLevelSpreadBps/10000.0)
		lastPrice = mid * (1.0 + (depthBps-outerSafetyBps)/10000.0)
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
		sizeNotional := levelNotional

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
