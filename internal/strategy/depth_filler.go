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
	NumLevels           int     `json:"num_levels"`            // Number of levels per side
	TargetDepthNotional float64 `json:"target_depth_notional"` // Total notional per side
	TimeSleepMs         int     `json:"time_sleep_ms"`         // Sleep between placing orders (rate limit)
	RemoveThresholdPct  float64 `json:"remove_threshold_pct"`  // Remove when price within this % (default = min_depth)
	LadderRegenBps      float64 `json:"ladder_regen_bps"`      // Regenerate when mid moves this much
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
	if cfg.NumLevels == 0 {
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
	log.Printf("[%s] Config: depth=%.1f%%-%.1f%%, levels=%d, sleepMs=%d",
		s.Name(), s.cfg.MinDepthPct, s.cfg.MaxDepthPct, s.cfg.NumLevels, s.cfg.TimeSleepMs)

	return nil
}

// Tick executes one strategy cycle
func (s *DepthFillerStrategy) Tick(ctx context.Context, input *core.TickInput) (*core.TickOutput, error) {
	snap := input.Snapshot
	balance := input.Balance
	mid := snap.Mid

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
	expectedOrders := s.cfg.NumLevels * 2
	if currentOrderCount < expectedOrders/2 {
		return true, fmt.Sprintf("orders_low_%d/%d", currentOrderCount, expectedOrders)
	}

	return false, ""
}

// computeDesiredOrders computes the desired order ladder
func (s *DepthFillerStrategy) computeDesiredOrders(mid float64, balance *core.BalanceState) []core.DesiredOrder {
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

	// Short batchID
	timestamp := time.Now().UnixMilli() % 100000000
	batchID := fmt.Sprintf("%d%03d", timestamp, rand.Intn(1000))

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
	log.Printf("[%s] UNEXPECTED FILL at %.8f - order was too close to mid!",
		s.Name(), event.Price)

	// Clear cached ladder to force regeneration
	s.mu.Lock()
	s.cachedLadder = nil
	s.mu.Unlock()
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
