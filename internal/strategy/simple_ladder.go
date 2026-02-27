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

	"mm-platform-engine/internal/core"
	"mm-platform-engine/internal/types"
)

// SimpleLadderConfig is the configuration for SimpleLadderStrategy
type SimpleLadderConfig struct {
	SpreadBps           float64 `json:"spread_bps"`
	NumLevels           int     `json:"num_levels"`
	TargetDepthNotional float64 `json:"target_depth_notional"`
	DepthBps            float64 `json:"depth_bps"`
	PriceJitterPct      float64 `json:"price_jitter_pct"`
	SizeJitterPct       float64 `json:"size_jitter_pct"`
	MinBalanceToTrade   float64 `json:"min_balance_to_trade"`
	LadderRegenBps      float64 `json:"ladder_regen_bps"`
	LevelGapTicksMax    int     `json:"level_gap_ticks_max"`
	TargetRatio         float64 `json:"target_ratio"`
	RatioK              float64 `json:"ratio_k"`

	// Risk settings
	DrawdownLimitPct    float64 `json:"drawdown_limit_pct"`     // Max drawdown before pause (e.g., 0.05 = 5%)
	DrawdownWarnPct     float64 `json:"drawdown_warn_pct"`      // Warning threshold (e.g., 0.03 = 3%)
	DrawdownReducePct   float64 `json:"drawdown_reduce_pct"`    // Start reducing size at this level (e.g., 0.02 = 2%)
	RecoveryHours       float64 `json:"recovery_hours"`         // Target recovery time in hours (24-72)
	MaxRecoverySizeMult float64 `json:"max_recovery_size_mult"` // Max size multiplier during recovery (e.g., 0.3 = 30%)

	// Debug settings
	DebugCancelSleep bool `json:"debug_cancel_sleep"` // Enable 30s sleep after cancel (for debugging WebSocket)
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

	// Cancel tracking (for debug)
	lastCancelTime int64           // Last EXTERNAL cancel timestamp (ms)
	pendingCancels map[string]bool // Order IDs that bot is canceling (internal)

	// Risk tracking
	peakNAV        float64          // Peak NAV for drawdown calculation
	navHistory     []navSnapshot    // NAV history for tracking
	currentMode    SimpleLadderMode // Current operating mode
	modeChangedAt  time.Time        // When mode last changed
	recoveryTarget float64          // Target NAV for recovery
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
	if cfg.SpreadBps == 0 {
		cfg.SpreadBps = 50
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
		cfg.DepthBps = 200
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
		cfg:            cfg,
		fillCooldowns:  make(map[float64]int64),
		fillCooldownMs: 5000 + rand.Int63n(5000),
		pendingCancels: make(map[string]bool),
		navHistory:     make([]navSnapshot, 0, 1000),
		currentMode:    ModeNormal,
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
	mid := snap.Mid

	// Calculate current NAV
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

	// Check fill cooldown - don't place new orders immediately after a fill
	s.mu.RLock()
	lastFill := s.lastFillTime
	cooldownMs := s.fillCooldownMs
	lastCancel := s.lastCancelTime
	s.mu.RUnlock()

	if lastFill > 0 {
		elapsed := time.Now().UnixMilli() - lastFill
		if elapsed < cooldownMs {
			remaining := cooldownMs - elapsed
			return &core.TickOutput{
				Action: core.TickActionKeep,
				Reason: fmt.Sprintf("fill_cooldown: %dms remaining", remaining),
			}, nil
		}
	}

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
	s.fillCooldownMs = 5000 + rand.Int63n(5000) // 5-10 seconds random cooldown

	// Clear cached ladder to force regeneration after cooldown
	s.cachedLadder = nil

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

	s.cfg.SpreadBps = cfg.SpreadMinBps
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
		s.Name(), s.cfg.SpreadBps, s.cfg.DepthBps, s.cfg.NumLevels, s.cfg.TargetDepthNotional)

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

	// First time
	if s.cachedMid == 0 || len(s.cachedLadder) == 0 {
		return true, "initial"
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
		s.mu.Unlock()
		log.Printf("[%s] Regenerated ladder: reason=%s, levels=%d", s.Name(), reason, len(s.cachedLadder))
	}

	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cachedLadder
}

// computeDesiredOrders computes the desired order ladder for BOTH sides
func (s *SimpleLadderStrategy) computeDesiredOrders(mid float64, balance *core.BalanceState) []core.DesiredOrder {
	// Calculate depth limit prices
	depthBps := s.cfg.DepthBps
	if depthBps <= 0 {
		depthBps = 200
	}

	spreadBps := s.cfg.SpreadBps
	if spreadBps <= 0 {
		spreadBps = 50
	}

	numLevels := s.cfg.NumLevels
	if numLevels <= 0 {
		numLevels = 5
	}

	// Target depth per side (config is per-side, not total)
	targetDepthPerSide := s.cfg.TargetDepthNotional

	// Calculate inventory skew
	// invRatio = baseValue / totalValue (0.5 = balanced)
	// invSkew > 0: too much base → ASK closer to mid, BID further
	// invSkew < 0: too much quote → BID closer to mid, ASK further
	baseValue := balance.BaseFree * mid
	quoteValue := balance.QuoteFree
	totalValue := baseValue + quoteValue
	invRatio := 0.5
	if totalValue > 0 {
		invRatio = baseValue / totalValue
	}
	invSkew := invRatio - 0.5 // -0.5 to +0.5

	// Adjust spread based on skew
	// skewBps: how much to shift the "heavy" side towards depthBps
	// Max shift = (depthBps - spreadBps) when fully skewed
	maxShift := depthBps - spreadBps
	skewBps := math.Abs(invSkew) * 2 * maxShift // 0 to maxShift

	// BID spreadBps: increase if too much base (invSkew > 0)
	// ASK spreadBps: increase if too much quote (invSkew < 0)
	bidSpreadBps := spreadBps
	askSpreadBps := spreadBps
	if invSkew > 0.05 { // Too much base - BID further, ASK closer
		bidSpreadBps = spreadBps + skewBps
		askSpreadBps = spreadBps
	} else if invSkew < -0.05 { // Too much quote - ASK further, BID closer
		askSpreadBps = spreadBps + skewBps
		bidSpreadBps = spreadBps
	}

	// Cap to not exceed depthBps
	if bidSpreadBps > depthBps {
		bidSpreadBps = depthBps * 0.9
	}
	if askSpreadBps > depthBps {
		askSpreadBps = depthBps * 0.9
	}

	if math.Abs(invSkew) > 0.05 {
		log.Printf("[%s] Inventory skew: %.1f%% (base=$%.0f, quote=$%.0f) → BID@%.0fbps, ASK@%.0fbps",
			s.Name(), invSkew*100, baseValue, quoteValue, bidSpreadBps, askSpreadBps)
	}

	// Short batchID to fit Gate.io 30 char limit
	timestamp := time.Now().UnixMilli() % 100000000
	batchID := fmt.Sprintf("%d%03d", timestamp, rand.Intn(1000))

	var orders []core.DesiredOrder

	// Generate BID orders (with adjusted spread)
	bidOrders := s.generateSideOrders(mid, bidSpreadBps, depthBps, numLevels, targetDepthPerSide, "BUY", batchID)
	orders = append(orders, bidOrders...)

	// Generate ASK orders (with adjusted spread)
	askOrders := s.generateSideOrders(mid, askSpreadBps, depthBps, numLevels, targetDepthPerSide, "SELL", batchID)
	orders = append(orders, askOrders...)

	return orders
}

// generateSideOrders generates orders for one side (BUY or SELL)
func (s *SimpleLadderStrategy) generateSideOrders(mid, spreadBps, depthBps float64, numLevels int, targetDepth float64, side string, batchID string) []core.DesiredOrder {
	// Calculate price range for ladder
	// First level: at spreadBps from mid
	// Last level: at depthBps from mid
	var firstPrice, lastPrice float64
	if side == "BUY" {
		firstPrice = mid * (1.0 - spreadBps/10000.0)
		lastPrice = mid * (1.0 - depthBps/10000.0)
	} else {
		firstPrice = mid * (1.0 + spreadBps/10000.0)
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
				price = s.roundToTick(minPrice)
			}
			if price > mid {
				price = s.roundToTick(mid * (1.0 - spreadBps/10000.0))
			}
		} else {
			maxPrice := mid * (1.0 + depthBps/10000.0)
			if price > maxPrice {
				price = s.roundToTick(maxPrice)
			}
			if price < mid {
				price = s.roundToTick(mid * (1.0 + spreadBps/10000.0))
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

		// Random size: base × (1.0 to 1.5) - ensures total > targetDepth
		jitter := 1.0 + rand.Float64()*0.5
		sizeNotional := baseNotionalPerLevel * jitter

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
	matchedLive := make(map[string]bool)
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
				matchedLive[l.OrderID] = true
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
