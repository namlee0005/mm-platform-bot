package strategy

import (
	"context"
	"fmt"
	"log"
	"math"
	"math/rand"
	"sync"
	"time"

	"mm-platform-engine/internal/core"
	"mm-platform-engine/internal/types"
)

// BotSide defines which side this strategy handles
type BotSide string

const (
	BotSideBid BotSide = "bid" // maker-bid: only BUY orders
	BotSideAsk BotSide = "ask" // maker-ask: only SELL orders
)

// SimpleLadderConfig is the configuration for SimpleLadderStrategy
type SimpleLadderConfig struct {
	BotSide             BotSide `json:"bot_side"`
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

	// Inventory rebalancing settings
	EnableRebalance   bool    `json:"enable_rebalance"`   // Enable inventory-aware sizing
	TargetInvRatio    float64 `json:"target_inv_ratio"`   // Target inventory ratio (default 0.5 = 50% base)
	RebalanceK        float64 `json:"rebalance_k"`        // Rebalance sensitivity (default 2.0)
	MaxRebalanceMult  float64 `json:"max_rebalance_mult"` // Max size multiplier for rebalance (default 2.0)
	MinRebalanceMult  float64 `json:"min_rebalance_mult"` // Min size multiplier for rebalance (default 0.2)
	RebalanceDeadzone float64 `json:"rebalance_deadzone"` // Deadzone - no adjustment if deviation < this (default 0.05)
}

// SimpleLadderStrategy implements a one-sided market maker strategy
// that places a ladder of orders on either the bid or ask side.
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

	// Rebalance defaults
	if cfg.TargetInvRatio == 0 {
		cfg.TargetInvRatio = 0.5 // 50% base, 50% quote
	}
	if cfg.RebalanceK == 0 {
		cfg.RebalanceK = 2.0 // sensitivity factor
	}
	if cfg.MaxRebalanceMult == 0 {
		cfg.MaxRebalanceMult = 2.0 // max 2x size
	}
	if cfg.MinRebalanceMult == 0 {
		cfg.MinRebalanceMult = 0.2 // min 0.2x size
	}
	if cfg.RebalanceDeadzone == 0 {
		cfg.RebalanceDeadzone = 0.05 // 5% deadzone
	}

	return &SimpleLadderStrategy{
		cfg:            cfg,
		fillCooldowns:  make(map[float64]int64),
		fillCooldownMs: 5000 + rand.Int63n(5000),
		navHistory:     make([]navSnapshot, 0, 1000),
		currentMode:    ModeNormal,
	}
}

// Name returns the strategy name
func (s *SimpleLadderStrategy) Name() string {
	return fmt.Sprintf("SimpleLadder[%s]", s.cfg.BotSide)
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

	// Get available balance for this side
	availableBalance := s.getAvailableBalance(balance, mid)

	// Check if we have minimum balance to trade
	minBalance := s.cfg.MinBalanceToTrade
	if minBalance == 0 {
		minBalance = s.minNotional
	}

	if availableBalance < minBalance {
		return &core.TickOutput{
			Action: core.TickActionCancelAll,
			Reason: fmt.Sprintf("insufficient balance: %.4f < %.4f", availableBalance, minBalance),
		}, nil
	}

	// Calculate size multiplier based on mode
	sizeMult := s.calculateSizeMultiplier(drawdown, mode)

	// Calculate inventory rebalance multiplier
	var rebalanceMult float64 = 1.0
	var invRatio, invDev float64
	if s.cfg.EnableRebalance {
		invRatio, invDev, rebalanceMult = s.calculateRebalanceMultiplier(balance, mid)
		if len(s.navHistory)%60 == 0 { // Log periodically
			log.Printf("[%s] Rebalance: inv=%.1f%% (target=%.1f%%), dev=%.1f%%, mult=%.2f",
				s.Name(), invRatio*100, s.cfg.TargetInvRatio*100, invDev*100, rebalanceMult)
		}
	}

	// Combine multipliers
	finalMult := sizeMult * rebalanceMult

	// Check if we should regenerate ladder
	currentOrderCount := len(input.LiveOrders)
	desired := s.getOrRegenerateLadder(mid, availableBalance, currentOrderCount)

	if len(desired) == 0 {
		return &core.TickOutput{
			Action: core.TickActionKeep,
			Reason: "no orders to place",
		}, nil
	}

	// Apply combined size multiplier
	if finalMult != 1.0 {
		desired = s.applySizeMultiplier(desired, finalMult)
		if finalMult < 1.0 {
			log.Printf("[%s] Size reduced: risk=%.2f × rebalance=%.2f = %.2f", s.Name(), sizeMult, rebalanceMult, finalMult)
		} else {
			log.Printf("[%s] Size increased: risk=%.2f × rebalance=%.2f = %.2f", s.Name(), sizeMult, rebalanceMult, finalMult)
		}
	}

	_ = invRatio // suppress unused warning when rebalance disabled

	// Check if we should replace orders
	shouldReplace, reason := s.shouldReplaceOrders(mid, currentOrderCount, input.LiveOrders, desired)

	if shouldReplace {
		return &core.TickOutput{
			Action:        core.TickActionReplace,
			DesiredOrders: desired,
			Reason:        reason,
			Metrics: map[string]float64{
				"nav":       nav,
				"drawdown":  drawdown,
				"size_mult": sizeMult,
			},
		}, nil
	}

	return &core.TickOutput{
		Action: core.TickActionKeep,
		Reason: "orders within tolerance",
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

// calculateRebalanceMultiplier calculates size multiplier based on inventory deviation
// Returns: invRatio, invDeviation, multiplier
func (s *SimpleLadderStrategy) calculateRebalanceMultiplier(balance *core.BalanceState, mid float64) (float64, float64, float64) {
	// Calculate inventory ratio (base value / total value)
	baseValue := (balance.BaseFree + balance.BaseLocked) * mid
	quoteValue := balance.QuoteFree + balance.QuoteLocked
	totalValue := baseValue + quoteValue

	if totalValue <= 0 {
		return 0.5, 0, 1.0
	}

	invRatio := baseValue / totalValue
	invDev := invRatio - s.cfg.TargetInvRatio

	// If within deadzone, no adjustment
	if math.Abs(invDev) <= s.cfg.RebalanceDeadzone {
		return invRatio, invDev, 1.0
	}

	// Calculate multiplier based on side and deviation
	// BID (buy): if too much base (invDev > 0), reduce buying → mult < 1
	// ASK (sell): if too much base (invDev > 0), increase selling → mult > 1
	var mult float64

	if s.cfg.BotSide == BotSideBid {
		// BUY side: reduce when we have too much base
		// invDev > 0 → mult < 1 (reduce buying)
		// invDev < 0 → mult > 1 (increase buying)
		effectiveDev := invDev - s.cfg.RebalanceDeadzone*sign(invDev)
		mult = 1.0 - effectiveDev*s.cfg.RebalanceK
	} else {
		// SELL side: increase when we have too much base
		// invDev > 0 → mult > 1 (increase selling)
		// invDev < 0 → mult < 1 (reduce selling)
		effectiveDev := invDev - s.cfg.RebalanceDeadzone*sign(invDev)
		mult = 1.0 + effectiveDev*s.cfg.RebalanceK
	}

	// Clamp to min/max
	if mult > s.cfg.MaxRebalanceMult {
		mult = s.cfg.MaxRebalanceMult
	}
	if mult < s.cfg.MinRebalanceMult {
		mult = s.cfg.MinRebalanceMult
	}

	return invRatio, invDev, mult
}

// sign returns -1, 0, or 1 depending on the sign of x
func sign(x float64) float64 {
	if x > 0 {
		return 1
	}
	if x < 0 {
		return -1
	}
	return 0
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
	fillPrice := s.roundToTick(event.Price)
	s.fillCooldowns[fillPrice] = time.Now().UnixMilli()
	s.fillCooldownMs = 5000 + rand.Int63n(5000)
	log.Printf("[%s] Fill cooldown set for price %.8f, will wait %dms",
		s.Name(), fillPrice, s.fillCooldownMs)
}

// OnOrderUpdate handles order status updates
func (s *SimpleLadderStrategy) OnOrderUpdate(event *core.OrderEvent) {
	// Log order updates
	log.Printf("[%s] Order %s: %s @ %.8f status=%s",
		s.Name(), event.OrderID, event.Side, event.Price, event.Status)
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

	log.Printf("[%s] Config updated: spread=%.0fbps, levels=%d, depth=$%.0f",
		s.Name(), s.cfg.SpreadBps, s.cfg.NumLevels, s.cfg.TargetDepthNotional)

	return nil
}

// getAvailableBalance returns available balance for this side
func (s *SimpleLadderStrategy) getAvailableBalance(balance *core.BalanceState, mid float64) float64 {
	if s.cfg.BotSide == BotSideBid {
		return balance.QuoteFree // Need quote to buy
	}
	// For ask, convert base to notional value
	return balance.BaseFree * mid
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
func (s *SimpleLadderStrategy) getOrRegenerateLadder(mid, balance float64, currentOrderCount int) []core.DesiredOrder {
	shouldRegen, reason := s.shouldRegenerateLadder(mid, balance, currentOrderCount)

	if shouldRegen {
		s.mu.Lock()
		s.cachedLadder = s.computeDesiredOrders(mid, balance)
		s.cachedMid = mid
		s.cachedBalance = balance
		s.mu.Unlock()
		log.Printf("[%s] Regenerated ladder: reason=%s, levels=%d", s.Name(), reason, len(s.cachedLadder))
	}

	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cachedLadder
}

// computeDesiredOrders computes the desired order ladder
func (s *SimpleLadderStrategy) computeDesiredOrders(mid float64, availableBalance float64) []core.DesiredOrder {
	// Target depth for one side
	targetDepthOneSide := s.cfg.TargetDepthNotional / 2.0

	// Calculate effective depth based on available balance
	effectiveDepth := targetDepthOneSide
	if availableBalance < effectiveDepth {
		effectiveDepth = availableBalance * 0.9
	}

	side := "BUY"
	if s.cfg.BotSide == BotSideAsk {
		side = "SELL"
	}

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

	// Calculate price range for ladder
	// First level: at spreadBps from mid
	// Last level: at depthBps from mid
	// Distribute levels evenly within this range

	var firstPrice, lastPrice float64
	if s.cfg.BotSide == BotSideBid {
		firstPrice = mid * (1.0 - spreadBps/10000.0)
		lastPrice = mid * (1.0 - depthBps/10000.0)
	} else {
		firstPrice = mid * (1.0 + spreadBps/10000.0)
		lastPrice = mid * (1.0 + depthBps/10000.0)
	}

	// Calculate base step between levels (in price)
	priceRange := math.Abs(lastPrice - firstPrice)
	baseStep := priceRange / float64(numLevels-1)
	if numLevels == 1 {
		baseStep = 0
	}

	// Max jitter: ±LevelGapTicksMax ticks (but stay within bounds)
	maxGapTicks := s.cfg.LevelGapTicksMax
	if maxGapTicks <= 0 {
		maxGapTicks = 3
	}
	maxJitter := s.tickSize * float64(maxGapTicks)

	// Generate prices: evenly distributed with small random jitter
	levelPrices := make([]float64, 0, numLevels)

	for level := 0; level < numLevels; level++ {
		var basePrice float64
		if s.cfg.BotSide == BotSideBid {
			// Bid: firstPrice (highest) -> lastPrice (lowest)
			basePrice = firstPrice - baseStep*float64(level)
		} else {
			// Ask: firstPrice (lowest) -> lastPrice (highest)
			basePrice = firstPrice + baseStep*float64(level)
		}

		// Add random jitter (except for first and last levels to maintain bounds)
		var price float64
		if level == 0 || level == numLevels-1 {
			price = basePrice
		} else {
			// Random jitter: ±maxJitter, but more likely to be small
			jitter := (rand.Float64()*2 - 1) * maxJitter * 0.5 // ±50% of maxJitter
			price = basePrice + jitter
		}

		price = s.roundToTick(price)

		// Ensure within bounds
		if s.cfg.BotSide == BotSideBid {
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

	// Remove duplicate prices (can happen with small ranges and rounding)
	levelPrices = s.removeDuplicatePrices(levelPrices)

	actualLevels := len(levelPrices)
	if actualLevels == 0 {
		return nil
	}

	// Warn if we couldn't fit all requested levels
	if actualLevels < numLevels {
		ticksInRange := priceRange / s.tickSize
		log.Printf("[%s] WARNING: Only %d/%d levels fit in range. Price range=%.8f, tickSize=%.8f, ticks=%.0f",
			s.Name(), actualLevels, numLevels, priceRange, s.tickSize, ticksInRange)
	}

	// Pass 2: Calculate weighted sizes
	totalWeight := float64(actualLevels * (actualLevels + 1) / 2)

	orders := make([]core.DesiredOrder, 0, actualLevels)
	var totalNotionalUsed float64
	timestamp := time.Now().UnixMilli()
	batchID := fmt.Sprintf("%d_%04d", timestamp, rand.Intn(10000))

	for level := 0; level < actualLevels; level++ {
		remainingBalance := availableBalance - totalNotionalUsed
		if remainingBalance < s.minNotional {
			break
		}

		price := levelPrices[level]

		// Weighted notional
		weight := float64(actualLevels - level)
		baseSizeNotional := effectiveDepth * weight / totalWeight

		// Add jitter
		sizeJitter := 1.0 + s.cfg.SizeJitterPct*(2*rand.Float64()-1)
		sizeNotional := baseSizeNotional * sizeJitter

		if sizeNotional > remainingBalance {
			sizeNotional = remainingBalance * 0.95
		}

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
			Tag:        fmt.Sprintf("SM_%s_L%d_%s", batchID, level, s.cfg.BotSide),
		})

		totalNotionalUsed += orderNotional
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
