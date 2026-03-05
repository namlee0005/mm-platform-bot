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
	// Example: 10% skew → ±20 bps adjustment
	inventoryAdjustmentBps = 20.0
)

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

	// Debug settings
	DebugCancelSleep bool `json:"debug_cancel_sleep"` // Enable 30s sleep after cancel (for debugging WebSocket)
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

	// Cached ladder state
	mu             sync.RWMutex
	cachedLadder   []core.DesiredOrder
	cachedMid      float64
	cachedBalance  float64
	lastOrderCount int

	// Fill cooldown tracking
	fillCooldowns  map[float64]int64 // price -> fill timestamp
	fillCooldownMs int64
	lastFillTime   int64 // Last fill timestamp (ms) for global cooldown

	// Regeneration cooldown tracking
	lastRegenTime   int64 // Last regeneration timestamp (ms)
	regenCooldownMs int64 // Cooldown after regeneration (default 30s)

	// Cancel tracking (for debug)
	lastCancelTime int64            // Last EXTERNAL cancel timestamp (ms)
	pendingCancels map[string]bool  // Order IDs that bot is canceling (internal)
	recentFills    map[string]int64 // Order IDs that were recently filled (orderID -> timestamp)

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

	// Set default fill cooldown if not configured
	fillCooldownMs := int64(cfg.FillCooldownMs)
	if fillCooldownMs == 0 {
		fillCooldownMs = 5000 // Default 5 seconds
	}

	// Set default regeneration cooldown (30 seconds to prevent ping-pong)
	regenCooldownMs := int64(30000) // 30 seconds default

	return &SimpleLadderStrategy{
		cfg:             cfg,
		fillCooldowns:   make(map[float64]int64),
		fillCooldownMs:  fillCooldownMs,
		regenCooldownMs: regenCooldownMs,
		pendingCancels:  make(map[string]bool),
		recentFills:     make(map[string]int64),
		navHistory:      make([]navSnapshot, 0, 1000),
		currentMode:     ModeNormal,
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

	if s.minNotional <= 0 {
		s.minNotional = 5.0
	}

	log.Printf("[%s] Initialized: tickSize=%.8f, stepSize=%.8f, minNotional=%.2f",
		s.Name(), s.tickSize, s.stepSize, s.minNotional)

	return nil
}

// Tick executes one strategy cycle
func (s *SimpleLadderStrategy) Tick(ctx context.Context, input *core.TickInput) (*core.TickOutput, error) {
	snap := input.Snapshot
	balance := input.Balance
	orderBookMid := snap.Mid // Order book mid (fallback)

	// Calculate fair price using VWAP + inventory adjustment
	fairPrice, isFreshVWAP := s.calculateFairPrice(input.RecentTrades, balance, orderBookMid)

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

	// Check fill cooldown - we still regenerate after fills,
	// but cooldown prevents placing orders at the SAME PRICE as fills
	// The cooldown is applied later during order generation (in computeDesiredOrders)
	s.mu.RLock()
	lastCancel := s.lastCancelTime
	s.mu.RUnlock()

	// Debug: Sleep 30s after cancel to observe WebSocket events
	if s.cfg.DebugCancelSleep && lastCancel > 0 {
		const cancelSleepMs int64 = 30000 // 30 seconds
		elapsed := time.Now().UnixMilli() - lastCancel
		if elapsed < cancelSleepMs {
			remaining := cancelSleepMs - elapsed
			return &core.TickOutput{
				Action: core.TickActionKeep,
				Reason: fmt.Sprintf("DEBUG cancel_sleep: %dms remaining (observe WebSocket)", remaining),
			}, nil
		}
	}

	// Get total available balance (both sides)
	availableBalance := s.getAvailableBalance(balance, mid)

	// Check if we have minimum balance to trade
	// Need TargetDepthNotional * 2 (one for each side)
	minRequired := s.cfg.TargetDepthNotional * 2
	if availableBalance < minRequired {
		return &core.TickOutput{
			Action: core.TickActionCancelAll,
			Reason: fmt.Sprintf("insufficient balance: %.2f < %.2f (need %0.f per side)", availableBalance, minRequired, s.cfg.TargetDepthNotional),
		}, nil
	}

	// Calculate size multiplier based on mode
	sizeMult := s.calculateSizeMultiplier(drawdown, mode)

	// Check if we should regenerate ladder
	currentOrderCount := len(input.LiveOrders)
	desired := s.getOrRegenerateLadder(mid, balance, currentOrderCount)

	if len(desired) == 0 {
		return &core.TickOutput{
			Action: core.TickActionKeep,
			Reason: "no orders to place",
		}, nil
	}

	// Apply size multiplier if in recovery mode
	if sizeMult != 1.0 {
		desired = s.applySizeMultiplier(desired, sizeMult)
		log.Printf("[%s] Size adjusted: mult=%.2f", s.Name(), sizeMult)
	}

	// Compute order diff (incremental update)
	diff := s.computeOrderDiff(input.LiveOrders, desired)

	metrics := map[string]float64{
		"nav":       nav,
		"drawdown":  drawdown,
		"size_mult": sizeMult,
	}

	switch diff.Action {
	case core.TickActionKeep:
		return &core.TickOutput{
			Action:  core.TickActionKeep,
			Reason:  diff.Reason,
			Metrics: metrics,
		}, nil

	case core.TickActionReplace:
		// Mark all live orders as pending internal cancel (so we don't trigger external cancel)
		s.mu.Lock()
		for _, order := range input.LiveOrders {
			s.pendingCancels[order.OrderID] = true
		}
		s.mu.Unlock()

		return &core.TickOutput{
			Action:        core.TickActionReplace,
			DesiredOrders: desired,
			Reason:        diff.Reason,
			Metrics:       metrics,
		}, nil

	case core.TickActionAmend:
		// Mark orders as pending internal cancel (so we don't trigger debug sleep)
		s.mu.Lock()
		for _, orderID := range diff.OrdersToCancel {
			s.pendingCancels[orderID] = true
		}
		s.mu.Unlock()

		return &core.TickOutput{
			Action:         core.TickActionAmend,
			OrdersToCancel: diff.OrdersToCancel,
			OrdersToAdd:    diff.OrdersToAdd,
			Reason:         diff.Reason,
			Metrics:        metrics,
		}, nil
	}

	return &core.TickOutput{
		Action: core.TickActionKeep,
		Reason: "unknown_action",
	}, nil
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

	// Log mode changes
	if s.currentMode != prevMode {
		s.modeChangedAt = now
		log.Printf("[%s] Mode changed: %s -> %s (drawdown=%.2f%%, NAV=$%.2f, peak=$%.2f)",
			s.Name(), prevMode, s.currentMode, drawdown*100, nav, s.peakNAV)
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

// applySizeMultiplier applies size multiplier to all orders
func (s *SimpleLadderStrategy) applySizeMultiplier(orders []core.DesiredOrder, mult float64) []core.DesiredOrder {
	result := make([]core.DesiredOrder, 0, len(orders))
	for _, o := range orders {
		newQty := s.roundToStep(o.Qty * mult)
		newNotional := o.Price * newQty
		if newNotional >= s.minNotional {
			o.Qty = newQty
			result = append(result, o)
		}
	}
	return result
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
func (s *SimpleLadderStrategy) OnFill(event *core.FillEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UnixMilli()
	fillPrice := s.roundToTick(event.Price)
	s.fillCooldowns[fillPrice] = now
	s.lastFillTime = now
	// fillCooldownMs is set from config (no longer random)

	// Track filled orders to ignore late CANCELED events
	s.recentFills[event.OrderID] = now

	// Cleanup old filled orders (older than 60 seconds)
	cutoff := now - 60000
	for id, ts := range s.recentFills {
		if ts < cutoff {
			delete(s.recentFills, id)
		}
	}

	// Don't clear cached ladder - let normal logic handle regeneration
	// Only regenerate if inventory changes significantly or mid moves
	// s.cachedLadder = nil  // Removed: too aggressive

	log.Printf("[%s] Fill at %.8f, cooldown %dms activated",
		s.Name(), fillPrice, s.fillCooldownMs)
}

// OnOrderUpdate handles order status updates
func (s *SimpleLadderStrategy) OnOrderUpdate(event *core.OrderEvent) {
	log.Printf("[%s] Order %s: %s @ %.8f status=%s",
		s.Name(), event.OrderID, event.Side, event.Price, event.Status)

	// If order was canceled
	if event.Status == "CANCELED" || event.Status == "EXPIRED" || event.Status == "REJECTED" {
		s.mu.Lock()

		// Check if this is an internal cancel (bot initiated) or external (user/UI)
		isInternalCancel := s.pendingCancels[event.OrderID]
		if isInternalCancel {
			// Remove from pending - this was our own cancel
			delete(s.pendingCancels, event.OrderID)
			s.mu.Unlock()
			log.Printf("[%s] Internal cancel confirmed: %s", s.Name(), event.OrderID)
			return
		}

		// Check if this order was recently filled (partial or full)
		// Late CANCELED events for filled orders should be ignored
		_, wasFilled := s.recentFills[event.OrderID]
		if wasFilled {
			delete(s.recentFills, event.OrderID) // Clean up
			s.mu.Unlock()
			log.Printf("[%s] Filled order canceled (late event): %s", s.Name(), event.OrderID)
			return
		}

		// External cancel - force ladder regeneration and maybe sleep
		s.cachedLadder = nil
		s.lastCancelTime = time.Now().UnixMilli()
		s.mu.Unlock()

		if s.cfg.DebugCancelSleep {
			log.Printf("[%s] DEBUG: EXTERNAL cancel %s, will sleep 30s for WebSocket debug",
				s.Name(), event.OrderID)
		} else {
			log.Printf("[%s] External cancel %s, will regenerate ladder", s.Name(), event.OrderID)
		}
	}
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

	// Clear cached ladder to force regeneration
	s.cachedLadder = nil
	s.cachedMid = 0
	s.cachedBalance = 0

	log.Printf("[%s] Config updated: spread=%.0fbps, depthBps=%.0f, levels=%d, depth=$%.0f",
		s.Name(), s.cfg.SpreadMinBps, s.cfg.DepthBps, s.cfg.NumLevels, s.cfg.TargetDepthNotional)

	return nil
}

// getAvailableBalance returns total available balance (both base and quote as notional)
func (s *SimpleLadderStrategy) getAvailableBalance(balance *core.BalanceState, mid float64) float64 {
	// Total notional = base value + quote value
	return (balance.BaseFree * mid) + balance.QuoteFree
}

// shouldRegenerateLadder checks if we need to regenerate the ladder
func (s *SimpleLadderStrategy) shouldRegenerateLadder(mid, balance float64, currentOrderCount int) (bool, string) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	// First time - always allow
	if s.cachedMid == 0 || len(s.cachedLadder) == 0 {
		return true, "initial"
	}

	// Check regeneration cooldown to prevent ping-pong
	// Skip this check if orders are missing (fills happened)
	expectedOrderCount := s.cfg.NumLevels * 2 // Both sides
	ordersMissing := currentOrderCount < expectedOrderCount

	if !ordersMissing && s.lastRegenTime > 0 {
		elapsed := time.Now().UnixMilli() - s.lastRegenTime
		if elapsed < s.regenCooldownMs {
			// Still in cooldown - don't regenerate
			// This prevents rapid regenerations when mid ping-pongs
			return false, ""
		}
	}

	// Order count changed (fills happened) - regenerate immediately
	// This is critical: when fills happen, orders are removed from book and mid changes
	if ordersMissing {
		missing := expectedOrderCount - currentOrderCount
		return true, fmt.Sprintf("missing_%d_orders", missing)
	}

	// Mid moved significantly
	midChangeBps := math.Abs(mid-s.cachedMid) / s.cachedMid * 10000
	if midChangeBps > s.cfg.LadderRegenBps {
		return true, fmt.Sprintf("mid_moved_%.1fbps", midChangeBps)
	}

	return false, ""
}

// getOrRegenerateLadder returns cached ladder or generates new one
func (s *SimpleLadderStrategy) getOrRegenerateLadder(mid float64, balance *core.BalanceState, currentOrderCount int) []core.DesiredOrder {
	totalBalance := s.getAvailableBalance(balance, mid)
	shouldRegen, reason := s.shouldRegenerateLadder(mid, totalBalance, currentOrderCount)

	if shouldRegen {
		s.mu.Lock()
		s.cachedLadder = s.computeDesiredOrders(mid, balance)
		s.cachedMid = mid
		s.cachedBalance = totalBalance
		s.lastRegenTime = time.Now().UnixMilli() // Update regeneration timestamp
		s.mu.Unlock()
		log.Printf("[%s] Regenerated ladder: reason=%s, levels=%d", s.Name(), reason, len(s.cachedLadder))
	}

	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cachedLadder
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
	inv := calculateInventoryState(balance.BaseFree, balance.QuoteFree, basePrice)

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
// Returns inventory state for spread adjustment decisions
func calculateInventoryState(baseFree, quoteFree, mid float64) inventoryState {
	baseValue := baseFree * mid
	quoteValue := quoteFree
	totalValue := baseValue + quoteValue

	ratio := 0.5 // Default to balanced if no value
	if totalValue > 0 {
		ratio = baseValue / totalValue
	}

	return inventoryState{
		baseValue:  baseValue,
		quoteValue: quoteValue,
		ratio:      ratio,
		skew:       ratio - 0.5, // -0.5 (all quote) to +0.5 (all base)
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

// computeDesiredOrders generates a two-sided order ladder with the following logic:
//
// 1. Spread Allocation:
//   - Total spread (first BID to first ASK) is randomized within [spread_min_bps, spread_max_bps]
//   - Spread is split between BID and ASK based on inventory skew
//   - Balanced inventory (50/50): symmetric split
//   - Excess base: BID gets more spread (further from mid) to encourage selling
//   - Excess quote: ASK gets more spread (further from mid) to encourage buying
//
// 2. Order Generation:
//   - First level: placed at calculated spread offset
//   - Other levels: distributed from first level to depth_bps
//   - Order sizes: randomized (70% to 130% of base) for variation while controlling total
//
// 3. Volume Distribution:
//   - Total target volume per side: target_depth_notional
//   - Distributed across num_levels with random size variation
//
// Returns: Slice of desired orders for both BID and ASK sides
func (s *SimpleLadderStrategy) computeDesiredOrders(mid float64, balance *core.BalanceState) []core.DesiredOrder {
	// Calculate depth limit prices
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
		numLevels = 5
	}

	// Target depth per side (config is per-side, not total)
	targetDepthPerSide := s.cfg.TargetDepthNotional

	// Calculate current inventory state
	inv := calculateInventoryState(balance.BaseFree, balance.QuoteFree, mid)

	// Allocate spread between BID and ASK based on inventory
	spread := calculateSpreadAllocation(spreadMinBps, spreadMaxBps, inv.skew)

	// Log inventory adjustments when significant
	if math.Abs(inv.skew) > inventorySkewThreshold {
		log.Printf("[%s] Inventory skew: %.1f%% (base=$%.0f, quote=$%.0f) → total_spread=%.1fbps, BID=%.1fbps, ASK=%.1fbps",
			s.Name(), inv.skew*100, inv.baseValue, inv.quoteValue, spread.totalBps, spread.bidBps, spread.askBps)
	}

	// Short batchID to fit Gate.io 30 char limit
	timestamp := time.Now().UnixMilli() % 100000000
	batchID := fmt.Sprintf("%d%03d", timestamp, rand.Intn(1000))

	var orders []core.DesiredOrder

	// Generate BID orders (first level at spread.bidBps, others spread to depthBps)
	bidOrders := s.generateSideOrders(mid, spread.bidBps, depthBps, numLevels, targetDepthPerSide, "BUY", batchID)
	orders = append(orders, bidOrders...)

	// Generate ASK orders (first level at spread.askBps, others spread to depthBps)
	askOrders := s.generateSideOrders(mid, spread.askBps, depthBps, numLevels, targetDepthPerSide, "SELL", batchID)
	orders = append(orders, askOrders...)

	// Log first level spread for both sides
	if len(bidOrders) > 0 && len(askOrders) > 0 {
		bidFirstPrice := bidOrders[0].Price
		askFirstPrice := askOrders[0].Price
		bidSpreadBps := (mid - bidFirstPrice) / mid * 10000
		askSpreadBps := (askFirstPrice - mid) / mid * 10000
		bidSpreadPct := bidSpreadBps / 100.0
		askSpreadPct := askSpreadBps / 100.0
		log.Printf("[%s] First level spread: BID %.2f%% (%.1fbps), ASK %.2f%% (%.1fbps), mid=%.8f",
			s.Name(), bidSpreadPct, bidSpreadBps, askSpreadPct, askSpreadBps, mid)
	}

	return orders
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

	// Generate orders with sizes
	orders := make([]core.DesiredOrder, 0, actualLevels)
	baseNotionalPerLevel := targetDepth / float64(actualLevels)

	sideTag := "B"
	if side == "SELL" {
		sideTag = "A"
	}

	for level := 0; level < actualLevels; level++ {
		price := levelPrices[level]

		// Apply size multiplier for unpredictable order sizes
		sizeMultiplier := calculateSizeMultiplier()
		sizeNotional := baseNotionalPerLevel * sizeMultiplier

		qty := sizeNotional / price
		qty = s.roundToStep(qty)

		orderNotional := price * qty
		if orderNotional < s.minNotional {
			continue
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

// shouldReplaceOrders determines if we should replace orders
func (s *SimpleLadderStrategy) shouldReplaceOrders(mid float64, currentCount int, live []core.LiveOrder, desired []core.DesiredOrder) (bool, string) {
	// No live orders - need to place
	if currentCount == 0 {
		return true, "no_live_orders"
	}

	// Different count
	if currentCount != len(desired) {
		return true, fmt.Sprintf("count_mismatch: %d vs %d", currentCount, len(desired))
	}

	// Check price tolerance
	maxGapTicks := s.cfg.LevelGapTicksMax
	if maxGapTicks <= 0 {
		maxGapTicks = 3
	}
	priceTolerance := s.tickSize * float64(maxGapTicks)

	for _, d := range desired {
		matched := false
		for _, l := range live {
			if l.Side == d.Side {
				priceDiff := math.Abs(l.Price - d.Price)
				if priceDiff <= priceTolerance {
					matched = true
					break
				}
			}
		}
		if !matched {
			return true, "price_drift"
		}
	}

	return false, ""
}

// OrderDiff represents the incremental changes needed
type OrderDiff struct {
	Action         core.TickAction
	OrdersToCancel []string            // Order IDs to cancel
	OrdersToAdd    []core.DesiredOrder // Orders to add
	Reason         string
}

// computeOrderDiff computes the minimal changes needed to reach desired state
func (s *SimpleLadderStrategy) computeOrderDiff(live []core.LiveOrder, desired []core.DesiredOrder) *OrderDiff {
	// No live orders - place all
	if len(live) == 0 {
		return &OrderDiff{
			Action:      core.TickActionReplace,
			OrdersToAdd: desired,
			Reason:      "no_live_orders",
		}
	}

	// Sort both lists by price (descending for BID, ascending for ASK)
	// This ensures proper pairing and avoids cross-matching due to tolerance
	sortedLive := make([]core.LiveOrder, len(live))
	copy(sortedLive, live)
	sort.Slice(sortedLive, func(i, j int) bool {
		return sortedLive[i].Price > sortedLive[j].Price // descending
	})

	sortedDesired := make([]core.DesiredOrder, len(desired))
	copy(sortedDesired, desired)
	sort.Slice(sortedDesired, func(i, j int) bool {
		return sortedDesired[i].Price > sortedDesired[j].Price // descending
	})

	maxGapTicks := s.cfg.LevelGapTicksMax
	if maxGapTicks <= 0 {
		maxGapTicks = 3
	}
	priceTolerance := s.tickSize * float64(maxGapTicks)

	// Match sorted orders in parallel (1:1 matching by position)
	var toCancel []string
	var toAdd []core.DesiredOrder

	maxLen := len(sortedLive)
	if len(sortedDesired) > maxLen {
		maxLen = len(sortedDesired)
	}

	for i := 0; i < maxLen; i++ {
		var l *core.LiveOrder
		var d *core.DesiredOrder

		if i < len(sortedLive) {
			l = &sortedLive[i]
		}
		if i < len(sortedDesired) {
			d = &sortedDesired[i]
		}

		if l != nil && d != nil {
			// Both exist at this position - check if they match
			priceDiff := math.Abs(l.Price - d.Price)
			if l.Side == d.Side && priceDiff <= priceTolerance {
				// Matched - no action needed
			} else {
				// Not matched - cancel live, add desired
				toCancel = append(toCancel, l.OrderID)
				toAdd = append(toAdd, *d)
			}
		} else if l != nil && d == nil {
			// Extra live order - cancel it
			toCancel = append(toCancel, l.OrderID)
		} else if l == nil && d != nil {
			// Missing live order - add it
			toAdd = append(toAdd, *d)
		}
	}

	// Determine action
	if len(toCancel) == 0 && len(toAdd) == 0 {
		return &OrderDiff{
			Action: core.TickActionKeep,
			Reason: "orders_in_sync",
		}
	}

	// If we need to cancel most orders, just do a full replace
	if len(toCancel) > len(live)/2 {
		return &OrderDiff{
			Action:      core.TickActionReplace,
			OrdersToAdd: desired,
			Reason:      fmt.Sprintf("too_many_cancels: %d/%d", len(toCancel), len(live)),
		}
	}

	// Incremental update
	return &OrderDiff{
		Action:         core.TickActionAmend,
		OrdersToCancel: toCancel,
		OrdersToAdd:    toAdd,
		Reason:         fmt.Sprintf("amend: cancel=%d, add=%d", len(toCancel), len(toAdd)),
	}
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

	seen := make(map[float64]bool)
	result := make([]float64, 0, len(prices))

	for _, p := range prices {
		// Round to avoid floating point comparison issues
		key := math.Round(p/s.tickSize) * s.tickSize
		if !seen[key] {
			seen[key] = true
			result = append(result, p)
		}
	}

	return result
}
