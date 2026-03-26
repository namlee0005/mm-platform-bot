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
)

// DepthFillerConfig is the configuration for DepthFillerStrategy
type DepthFillerConfig struct {
	MinDepthPct         float64 `json:"min_depth_pct"`         // Min depth from mid (e.g., 5 = 5%)
	MaxDepthPct         float64 `json:"max_depth_pct"`         // Max depth from mid (e.g., 50 = 50%)
	NumLevels           int     `json:"num_levels"`            // Number of levels per side (0 = unlimited when UseFullBalance=true)
	TargetDepthNotional float64 `json:"target_depth_notional"` // Total notional per side (ignored when UseFullBalance=true)
	TimeSleepMs         int     `json:"time_sleep_ms"`         // Sleep between placing orders (rate limit)
	RemoveThresholdPct  float64 `json:"remove_threshold_pct"`  // Remove when price within this % (default = min_depth)
	LadderRegenBps      float64 `json:"ladder_regen_bps"`      // Regenerate when mid moves this much

	// Full balance mode
	UseFullBalance  bool    `json:"use_full_balance"`   // Use all available balance
	MinOrderSizePct float64 `json:"min_order_size_pct"` // Min % of balance per order (default: 1%)
}

// pendingShiftOrder represents an order to add after a fill
type depthFillerShift struct {
	side      string  // BUY or SELL
	fillPrice float64 // Price where fill happened
	fillQty   float64 // Quantity filled
	timestamp int64   // When fill happened
}

// DepthFillerStrategy places orders far from mid (5%-50%) to fill order book depth
// Removes orders when price approaches to avoid getting filled
type DepthFillerStrategy struct {
	cfg *DepthFillerConfig

	// Market info cache
	tickSize    float64
	stepSize    float64
	minNotional float64

	// Cached ladder state
	mu            sync.RWMutex
	cachedLadder  []core.DesiredOrder
	cachedMid     float64
	lastRegenTime time.Time

	// Pending orders to place (for rate limiting)
	pendingOrders []core.DesiredOrder
	lastPlaceTime time.Time

	// Shift ladder tracking - orders to add after fill
	pendingShifts []depthFillerShift
}

// NewDepthFillerStrategy creates a new DepthFillerStrategy
func NewDepthFillerStrategy(cfg *DepthFillerConfig) *DepthFillerStrategy {
	// Set defaults
	if cfg.MinDepthPct == 0 {
		cfg.MinDepthPct = 5 // 5%
	}
	if cfg.MaxDepthPct == 0 {
		cfg.MaxDepthPct = 50 // 50%
	}
	if cfg.NumLevels == 0 && !cfg.UseFullBalance {
		cfg.NumLevels = 10
	}
	if cfg.TimeSleepMs == 0 {
		cfg.TimeSleepMs = 200 // 200ms between orders
	}
	if cfg.RemoveThresholdPct == 0 {
		cfg.RemoveThresholdPct = cfg.MinDepthPct // Same as min depth
	}
	if cfg.LadderRegenBps == 0 {
		cfg.LadderRegenBps = 100 // 1% move triggers regen
	}
	if cfg.MinOrderSizePct == 0 {
		cfg.MinOrderSizePct = 1 // 1% of balance per order
	}

	return &DepthFillerStrategy{
		cfg: cfg,
	}
}

// Name returns the strategy name
func (s *DepthFillerStrategy) Name() string {
	return "DepthFiller"
}

// Init initializes the strategy with market state
func (s *DepthFillerStrategy) Init(ctx context.Context, snap *core.Snapshot, balance *core.BalanceState) error {
	s.tickSize = snap.TickSize
	s.stepSize = snap.StepSize
	s.minNotional = snap.MinNotional

	if s.minNotional <= 0 {
		s.minNotional = 5.0
	}

	log.Printf("[%s] Initialized: tickSize=%.8f, stepSize=%.8f, minNotional=%.2f",
		s.Name(), s.tickSize, s.stepSize, s.minNotional)
	if s.cfg.UseFullBalance {
		log.Printf("[%s] Config: depth=%.1f%%-%.1f%%, useFullBalance=true, minOrderSizePct=%.1f%%",
			s.Name(), s.cfg.MinDepthPct, s.cfg.MaxDepthPct, s.cfg.MinOrderSizePct)
	} else {
		log.Printf("[%s] Config: depth=%.1f%%-%.1f%%, levels=%d, sleepMs=%d",
			s.Name(), s.cfg.MinDepthPct, s.cfg.MaxDepthPct, s.cfg.NumLevels, s.cfg.TimeSleepMs)
	}

	return nil
}

// Tick executes one strategy cycle
func (s *DepthFillerStrategy) Tick(ctx context.Context, input *core.TickInput) (*core.TickOutput, error) {
	snap := input.Snapshot
	balance := input.Balance
	mid := snap.Mid

	// Process pending shifts first (orders to add after fills)
	shiftOrders := s.processPendingShifts(mid, input.LiveOrders, balance)
	if len(shiftOrders) > 0 {
		return &core.TickOutput{
			Action:      core.TickActionAmend,
			OrdersToAdd: shiftOrders,
			Reason:      fmt.Sprintf("shift_ladder: adding %d orders after fill", len(shiftOrders)),
		}, nil
	}

	// Check if any live orders are too close to mid - need to remove them
	ordersToRemove := s.checkOrdersToRemove(input.LiveOrders, mid)
	if len(ordersToRemove) > 0 {
		log.Printf("[%s] Removing %d orders - price approaching", s.Name(), len(ordersToRemove))
		return &core.TickOutput{
			Action:         core.TickActionAmend,
			OrdersToCancel: ordersToRemove,
			Reason:         "price_approaching",
		}, nil
	}

	// Check if we need to regenerate ladder
	shouldRegen, reason := s.shouldRegenerateLadder(mid, len(input.LiveOrders))
	if !shouldRegen {
		return &core.TickOutput{
			Action: core.TickActionKeep,
			Reason: "orders_ok",
		}, nil
	}

	// Generate new ladder
	desired := s.computeDesiredOrders(mid, balance)
	if len(desired) == 0 {
		return &core.TickOutput{
			Action: core.TickActionKeep,
			Reason: "no_orders_to_place",
		}, nil
	}

	s.mu.Lock()
	s.cachedLadder = desired
	s.cachedMid = mid
	s.lastRegenTime = time.Now()
	s.mu.Unlock()

	log.Printf("[%s] Regenerated ladder: reason=%s, levels=%d", s.Name(), reason, len(desired))

	return &core.TickOutput{
		Action:        core.TickActionReplace,
		DesiredOrders: desired,
		Reason:        reason,
	}, nil
}

// checkOrdersToRemove returns order IDs that are too close to mid price
func (s *DepthFillerStrategy) checkOrdersToRemove(liveOrders []core.LiveOrder, mid float64) []string {
	threshold := s.cfg.RemoveThresholdPct / 100.0
	var toRemove []string

	for _, order := range liveOrders {
		distFromMid := math.Abs(order.Price-mid) / mid
		if distFromMid < threshold {
			log.Printf("[%s] Order %s at %.8f is %.2f%% from mid (threshold %.2f%%) - marking for removal",
				s.Name(), order.OrderID, order.Price, distFromMid*100, threshold*100)
			toRemove = append(toRemove, order.OrderID)
		}
	}

	return toRemove
}

// shouldRegenerateLadder checks if we need to regenerate the ladder
func (s *DepthFillerStrategy) shouldRegenerateLadder(mid float64, currentOrderCount int) (bool, string) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	// First time
	if s.cachedMid == 0 || len(s.cachedLadder) == 0 {
		return true, "initial"
	}

	// No orders left
	if currentOrderCount == 0 {
		return true, "no_orders"
	}

	// Mid moved significantly
	midChangeBps := math.Abs(mid-s.cachedMid) / s.cachedMid * 10000
	if midChangeBps > s.cfg.LadderRegenBps {
		return true, fmt.Sprintf("mid_moved_%.1fbps", midChangeBps)
	}

	// Too few orders (more than half removed)
	expectedOrders := len(s.cachedLadder)
	if s.cfg.NumLevels > 0 {
		expectedOrders = s.cfg.NumLevels * 2
	}
	if currentOrderCount < expectedOrders/2 {
		return true, fmt.Sprintf("orders_low_%d/%d", currentOrderCount, expectedOrders)
	}

	return false, ""
}

// computeDesiredOrders computes the desired order ladder
func (s *DepthFillerStrategy) computeDesiredOrders(mid float64, balance *core.BalanceState) []core.DesiredOrder {
	// Short batchID
	timestamp := time.Now().UnixMilli() % 100000000
	batchID := fmt.Sprintf("%d%03d", timestamp, rand.Intn(1000))

	if s.cfg.UseFullBalance {
		return s.computeFullBalanceOrders(mid, balance, batchID)
	}
	return s.computeFixedLevelOrders(mid, balance, batchID)
}

// computeFullBalanceOrders generates random orders using all available balance
func (s *DepthFillerStrategy) computeFullBalanceOrders(mid float64, balance *core.BalanceState, batchID string) []core.DesiredOrder {
	orders := make([]core.DesiredOrder, 0, 20)

	// Calculate available balance for each side
	quoteAvailable := balance.QuoteFree // For BID orders
	baseAvailable := balance.BaseFree   // For ASK orders
	baseNotional := baseAvailable * mid

	// Calculate min order size based on total balance
	totalNotional := quoteAvailable + baseNotional
	minOrderNotional := totalNotional * s.cfg.MinOrderSizePct / 100.0

	// Ensure minOrderNotional is at least minNotional
	if minOrderNotional < s.minNotional {
		minOrderNotional = s.minNotional
	}

	log.Printf("[%s] FullBalance mode: quoteAvail=%.2f, baseNotional=%.2f, minOrderNotional=%.2f",
		s.Name(), quoteAvailable, baseNotional, minOrderNotional)

	// Generate BID orders until quote balance depleted
	bidOrders := s.generateRandomOrders(mid, "BUY", quoteAvailable, minOrderNotional, batchID)
	orders = append(orders, bidOrders...)

	// Generate ASK orders until base balance depleted
	askOrders := s.generateRandomOrders(mid, "SELL", baseNotional, minOrderNotional, batchID)
	orders = append(orders, askOrders...)

	log.Printf("[%s] Generated %d BID + %d ASK = %d total orders",
		s.Name(), len(bidOrders), len(askOrders), len(orders))

	return orders
}

// generateRandomOrders generates random orders until totalNotional is depleted
func (s *DepthFillerStrategy) generateRandomOrders(
	mid float64,
	side string,
	totalNotional float64,
	minOrderNotional float64,
	batchID string,
) []core.DesiredOrder {
	var orders []core.DesiredOrder
	remainingNotional := totalNotional

	minDepth := s.cfg.MinDepthPct / 100.0
	maxDepth := s.cfg.MaxDepthPct / 100.0

	levelIndex := 0
	prefix := "B"
	if side == "SELL" {
		prefix = "A"
	}

	for remainingNotional >= minOrderNotional {
		// Random depth between min and max
		depth := minDepth + rand.Float64()*(maxDepth-minDepth)

		// Random order size (10% - 30% of remaining, but at least minOrderNotional)
		maxOrderNotional := remainingNotional * 0.3
		if maxOrderNotional < minOrderNotional*1.1 {
			// Use remaining if it's close to min
			maxOrderNotional = remainingNotional
		}
		minOrderSize := minOrderNotional
		if minOrderSize > remainingNotional {
			minOrderSize = remainingNotional
		}
		orderNotional := minOrderSize + rand.Float64()*(maxOrderNotional-minOrderSize)

		// Ensure we don't exceed remaining
		if orderNotional > remainingNotional {
			orderNotional = remainingNotional
		}

		// Calculate price based on side
		var price float64
		if side == "BUY" {
			price = mid * (1.0 - depth)
		} else {
			price = mid * (1.0 + depth)
		}
		price = s.roundToTick(price)

		// Calculate qty
		qty := orderNotional / price
		qty = s.roundToStep(qty)

		// Validate min notional
		actualNotional := price * qty
		if actualNotional < s.minNotional {
			// Skip if too small
			remainingNotional -= minOrderNotional * 0.1
			continue
		}

		orders = append(orders, core.DesiredOrder{
			Side:       side,
			Price:      price,
			Qty:        qty,
			LevelIndex: levelIndex,
			Tag:        fmt.Sprintf("DF_%s_%s%d", batchID, prefix, levelIndex),
		})

		remainingNotional -= actualNotional
		levelIndex++

		// Stop if remaining is too small
		if remainingNotional < minOrderNotional {
			break
		}
	}

	return orders
}

// computeFixedLevelOrders generates fixed number of levels (original logic)
func (s *DepthFillerStrategy) computeFixedLevelOrders(mid float64, balance *core.BalanceState, batchID string) []core.DesiredOrder {
	minDepth := s.cfg.MinDepthPct / 100.0 // e.g., 0.05
	maxDepth := s.cfg.MaxDepthPct / 100.0 // e.g., 0.50
	numLevels := s.cfg.NumLevels

	// Calculate depth step between levels
	depthStep := (maxDepth - minDepth) / float64(numLevels-1)
	if numLevels == 1 {
		depthStep = 0
	}

	// Target notional per level
	notionalPerLevel := s.cfg.TargetDepthNotional / float64(numLevels)

	var orders []core.DesiredOrder

	// Generate BID orders (below mid)
	for i := 0; i < numLevels; i++ {
		depth := minDepth + depthStep*float64(i)
		// Add small random jitter (±0.5% of depth)
		jitter := (rand.Float64() - 0.5) * 0.01 * depth
		price := mid * (1.0 - depth - jitter)
		price = s.roundToTick(price)

		// Random size jitter 1.0x - 1.3x
		sizeJitter := 1.0 + rand.Float64()*0.3
		sizeNotional := notionalPerLevel * sizeJitter

		qty := sizeNotional / price
		qty = s.roundToStep(qty)

		if price*qty >= s.minNotional {
			orders = append(orders, core.DesiredOrder{
				Side:       "BUY",
				Price:      price,
				Qty:        qty,
				LevelIndex: i,
				Tag:        fmt.Sprintf("DF_%s_B%d", batchID, i),
			})
		}
	}

	// Generate ASK orders (above mid)
	for i := 0; i < numLevels; i++ {
		depth := minDepth + depthStep*float64(i)
		// Add small random jitter
		jitter := (rand.Float64() - 0.5) * 0.01 * depth
		price := mid * (1.0 + depth + jitter)
		price = s.roundToTick(price)

		// Random size jitter
		sizeJitter := 1.0 + rand.Float64()*0.3
		sizeNotional := notionalPerLevel * sizeJitter

		qty := sizeNotional / price
		qty = s.roundToStep(qty)

		if price*qty >= s.minNotional {
			orders = append(orders, core.DesiredOrder{
				Side:       "SELL",
				Price:      price,
				Qty:        qty,
				LevelIndex: i,
				Tag:        fmt.Sprintf("DF_%s_A%d", batchID, i),
			})
		}
	}

	return orders
}

// OnFill handles fill events
func (s *DepthFillerStrategy) OnFill(event *core.FillEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UnixMilli()
	fillPrice := s.roundToTick(event.Price)

	log.Printf("[%s] Fill at %.8f, adding pending shift (side=%s, qty=%.4f)",
		s.Name(), fillPrice, event.Side, event.Quantity)

	// Add pending shift order - will be processed on next tick
	s.pendingShifts = append(s.pendingShifts, depthFillerShift{
		side:      event.Side,
		fillPrice: fillPrice,
		fillQty:   event.Quantity,
		timestamp: now,
	})
}

// OnOrderUpdate handles order status updates
func (s *DepthFillerStrategy) OnOrderUpdate(event *core.OrderEvent) {
	if event.Status == "CANCELED" || event.Status == "EXPIRED" {
		log.Printf("[%s] Order %s: %s @ %.8f status=%s",
			s.Name(), event.OrderID, event.Side, event.Price, event.Status)
	}
}

// UpdateConfig updates strategy config at runtime
func (s *DepthFillerStrategy) UpdateConfig(newCfg interface{}) error {
	// Not implemented for now
	return nil
}

func (s *DepthFillerStrategy) UpdatePrevSnapshot(_ []core.LiveOrder, _ *core.BalanceState) {
	// Not needed for DepthFiller
}

// roundToTick rounds price to tick size
func (s *DepthFillerStrategy) roundToTick(price float64) float64 {
	if s.tickSize <= 0 {
		return price
	}
	return math.Round(price/s.tickSize) * s.tickSize
}

// roundToStep rounds quantity to step size
func (s *DepthFillerStrategy) roundToStep(qty float64) float64 {
	if s.stepSize <= 0 {
		return qty
	}
	return math.Floor(qty/s.stepSize) * s.stepSize
}

// processPendingShifts processes pending shift orders after fills
// Returns orders to add at deeper levels in the ladder
func (s *DepthFillerStrategy) processPendingShifts(mid float64, liveOrders []core.LiveOrder, balance *core.BalanceState) []core.DesiredOrder {
	s.mu.Lock()
	pendingShifts := s.pendingShifts
	s.pendingShifts = nil // Clear pending
	s.mu.Unlock()

	if len(pendingShifts) == 0 {
		return nil
	}

	// Max shifts per tick to prevent overload
	const maxShiftsPerSide = 3

	// Group shifts by side
	var bidShifts, askShifts []depthFillerShift
	for _, shift := range pendingShifts {
		if shift.side == "BUY" {
			bidShifts = append(bidShifts, shift)
		} else {
			askShifts = append(askShifts, shift)
		}
	}

	// Limit shifts per side
	if len(bidShifts) > maxShiftsPerSide {
		log.Printf("[%s] Limiting BID shifts: %d -> %d", s.Name(), len(bidShifts), maxShiftsPerSide)
		bidShifts = bidShifts[:maxShiftsPerSide]
	}
	if len(askShifts) > maxShiftsPerSide {
		log.Printf("[%s] Limiting ASK shifts: %d -> %d", s.Name(), len(askShifts), maxShiftsPerSide)
		askShifts = askShifts[:maxShiftsPerSide]
	}

	// Find current deepest orders
	var lowestBid, highestAsk float64
	for _, order := range liveOrders {
		if order.Side == "BUY" {
			if lowestBid == 0 || order.Price < lowestBid {
				lowestBid = order.Price
			}
		} else {
			if highestAsk == 0 || order.Price > highestAsk {
				highestAsk = order.Price
			}
		}
	}

	// If no live orders, calculate from mid
	minDepth := s.cfg.MinDepthPct / 100.0
	maxDepth := s.cfg.MaxDepthPct / 100.0
	if lowestBid == 0 {
		lowestBid = mid * (1.0 - minDepth)
	}
	if highestAsk == 0 {
		highestAsk = mid * (1.0 + minDepth)
	}

	// Calculate step for new orders (move deeper by ~2-5% of current depth range)
	depthRange := maxDepth - minDepth
	stepPct := depthRange / float64(s.cfg.NumLevels) * 2 // Double the normal step

	var ordersToAdd []core.DesiredOrder
	timestamp := time.Now().UnixMilli() % 100000000
	batchID := fmt.Sprintf("%d%03d", timestamp, rand.Intn(1000))

	// Process BID shifts - add order below lowest bid (deeper)
	for i, shift := range bidShifts {
		// Calculate new price - step below lowest bid
		newPrice := lowestBid * (1.0 - stepPct)
		newPrice = s.roundToTick(newPrice)

		// Check max depth limit
		depthFromMid := (mid - newPrice) / mid
		if depthFromMid > maxDepth {
			log.Printf("[%s] Shift BID skipped: depth %.2f%% exceeds max %.2f%%",
				s.Name(), depthFromMid*100, maxDepth*100)
			continue
		}

		// Check min depth (don't place too close to mid)
		if depthFromMid < minDepth {
			newPrice = mid * (1.0 - minDepth)
			newPrice = s.roundToTick(newPrice)
		}

		// Calculate qty similar to fill qty with jitter
		qty := shift.fillQty * (0.9 + rand.Float64()*0.2) // 90%-110% of fill qty
		qty = s.roundToStep(qty)

		notional := newPrice * qty
		if notional < s.minNotional {
			qty = s.minNotional / newPrice * 1.1
			qty = s.roundToStep(qty)
			notional = newPrice * qty
		}

		// Check balance
		if notional > balance.QuoteFree*0.95 {
			log.Printf("[%s] Shift BID skipped: insufficient quote balance", s.Name())
			continue
		}

		ordersToAdd = append(ordersToAdd, core.DesiredOrder{
			Side:       "BUY",
			Price:      newPrice,
			Qty:        qty,
			LevelIndex: 0,
			Tag:        fmt.Sprintf("DFS_%s_B%d", batchID, i),
		})

		// Update lowest for next iteration
		lowestBid = newPrice

		log.Printf("[%s] Shift BID: adding order at %.8f x %.4f (depth=%.2f%%, after fill at %.8f)",
			s.Name(), newPrice, qty, depthFromMid*100, shift.fillPrice)
	}

	// Process ASK shifts - add order above highest ask (deeper)
	for i, shift := range askShifts {
		// Calculate new price - step above highest ask
		newPrice := highestAsk * (1.0 + stepPct)
		newPrice = s.roundToTick(newPrice)

		// Check max depth limit
		depthFromMid := (newPrice - mid) / mid
		if depthFromMid > maxDepth {
			log.Printf("[%s] Shift ASK skipped: depth %.2f%% exceeds max %.2f%%",
				s.Name(), depthFromMid*100, maxDepth*100)
			continue
		}

		// Check min depth (don't place too close to mid)
		if depthFromMid < minDepth {
			newPrice = mid * (1.0 + minDepth)
			newPrice = s.roundToTick(newPrice)
		}

		// Calculate qty similar to fill qty with jitter
		qty := shift.fillQty * (0.9 + rand.Float64()*0.2) // 90%-110% of fill qty
		qty = s.roundToStep(qty)

		notional := newPrice * qty
		if notional < s.minNotional {
			qty = s.minNotional / newPrice * 1.1
			qty = s.roundToStep(qty)
		}

		// Check balance
		if qty > balance.BaseFree*0.95 {
			log.Printf("[%s] Shift ASK skipped: insufficient base balance", s.Name())
			continue
		}

		ordersToAdd = append(ordersToAdd, core.DesiredOrder{
			Side:       "SELL",
			Price:      newPrice,
			Qty:        qty,
			LevelIndex: 0,
			Tag:        fmt.Sprintf("DFS_%s_A%d", batchID, i),
		})

		// Update highest for next iteration
		highestAsk = newPrice

		log.Printf("[%s] Shift ASK: adding order at %.8f x %.4f (depth=%.2f%%, after fill at %.8f)",
			s.Name(), newPrice, qty, depthFromMid*100, shift.fillPrice)
	}

	return ordersToAdd
}
