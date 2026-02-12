package engine

import (
	"context"
	"fmt"
	"log"
	"math"
	"math/rand"
	"strconv"
	"sync"
	"time"

	"mm-platform-engine/internal/exchange"
	"mm-platform-engine/internal/store"
	"mm-platform-engine/internal/types"
)

// BotSide defines which side this bot handles
type BotSide string

const (
	BotSideBid BotSide = "bid" // maker-bid: only BUY orders
	BotSideAsk BotSide = "ask" // maker-ask: only SELL orders
)

// SimpleMakerConfig is the minimal config for simple maker bot
type SimpleMakerConfig struct {
	Symbol              string  `json:"symbol"`
	BaseAsset           string  `json:"base_asset"`
	QuoteAsset          string  `json:"quote_asset"`
	BotSide             BotSide `json:"bot_side"`              // "bid" or "ask" - FIXED, không switch
	SpreadBps           float64 `json:"spread_bps"`            // spread from mid in basis points
	NumLevels           int     `json:"num_levels"`            // number of order levels
	TargetDepthNotional float64 `json:"target_depth_notional"` // total depth in quote currency
	TickIntervalMs      int     `json:"tick_interval_ms"`      // tick interval in milliseconds

	// Depth limit - all orders must be within ±DepthBps from mid
	DepthBps float64 `json:"depth_bps"` // max distance from mid in bps (e.g., 200 = 2%)

	// Randomization params (0.0 - 1.0)
	PriceJitterPct float64 `json:"price_jitter_pct"` // price randomization ±% (e.g., 0.2 = ±20% of spread)
	SizeJitterPct  float64 `json:"size_jitter_pct"`  // size randomization ±% (e.g., 0.3 = ±30% of size)

	// Balance threshold - minimum balance required to place orders
	MinBalanceToTrade float64 `json:"min_balance_to_trade"` // minimum quote (for bid) or base (for ask) to trade

	// Ladder regeneration threshold
	LadderRegenBps float64 `json:"ladder_regen_bps"` // mid change (bps) to regenerate ladder (default 50 = 0.5%)

	// Level spacing - random gap between levels
	LevelGapTicksMax int `json:"level_gap_ticks_max"` // max random ticks between levels (default 3)

	// Bot identification for Redis
	Exchange   string `json:"exchange"`    // exchange name (mexc, gate, etc.)
	ExchangeID string `json:"exchange_id"` // exchange ObjectID for finding partner
	BotID      string `json:"bot_id"`      // unique bot instance ID
	BotType    string `json:"bot_type"`    // "maker-bid" or "maker-ask"

	// Target ratio (inventory balancing)
	TargetRatio float64 `json:"target_ratio"` // target base/total ratio (0.5 = 50/50)
	RatioK      float64 `json:"ratio_k"`      // sensitivity factor for ratio adjustment (default 2.0)
}

// SimpleMaker is a minimal market maker that only places orders on one side
type SimpleMaker struct {
	cfg   *SimpleMakerConfig
	exch  exchange.Exchange
	redis *store.RedisStore
	mongo *store.MongoStore

	// State
	mu      sync.RWMutex
	running bool
	ctx     context.Context
	cancel  context.CancelFunc

	// Live orders tracking
	liveOrders map[string]*LiveOrder // orderID -> order

	// Callbacks
	onOrderEvent OrderEventCallback

	// Market info cache
	tickSize    float64
	stepSize    float64
	minNotional float64

	// Cached ladder - only regenerate when mid moves significantly or fill detected
	cachedLadder   []SimpleDesiredOrder
	cachedMid      float64
	cachedBalance  float64
	ladderRegenBps float64 // bps change in mid to trigger regeneration (e.g., 50 = 0.5%)
	lastOrderCount int     // track order count to detect fills

	// Config check counter
	tickCount           int
	configCheckInterval int // check config every N ticks

	// Partner bot for ratio balancing
	partnerBotID string
}

// NewSimpleMaker creates a new simple maker bot
func NewSimpleMaker(
	cfg *SimpleMakerConfig,
	exch exchange.Exchange,
	redis *store.RedisStore,
	mongo *store.MongoStore,
) *SimpleMaker {
	// Set defaults
	if cfg.TickIntervalMs == 0 {
		cfg.TickIntervalMs = 5000 // 5 seconds
	}
	if cfg.NumLevels == 0 {
		cfg.NumLevels = 5
	}
	if cfg.SpreadBps == 0 {
		cfg.SpreadBps = 50 // 0.5%
	}
	// Default randomization: ±20% price jitter, ±30% size jitter
	if cfg.PriceJitterPct == 0 {
		cfg.PriceJitterPct = 0.2
	}
	if cfg.SizeJitterPct == 0 {
		cfg.SizeJitterPct = 0.3
	}

	// Set default ladder regen threshold
	ladderRegenBps := cfg.LadderRegenBps
	if ladderRegenBps == 0 {
		ladderRegenBps = 50 // default 0.5%
	}

	// Calculate config check interval (every 10 seconds worth of ticks)
	configCheckInterval := 10000 / cfg.TickIntervalMs
	if configCheckInterval < 1 {
		configCheckInterval = 1
	}

	return &SimpleMaker{
		cfg:                 cfg,
		exch:                exch,
		redis:               redis,
		mongo:               mongo,
		liveOrders:          make(map[string]*LiveOrder),
		ladderRegenBps:      ladderRegenBps,
		configCheckInterval: configCheckInterval,
	}
}

// SetOrderEventCallback sets callback for order events
func (m *SimpleMaker) SetOrderEventCallback(cb OrderEventCallback) {
	m.onOrderEvent = cb
}

// Start starts the simple maker
func (m *SimpleMaker) Start(ctx context.Context) error {
	m.mu.Lock()
	if m.running {
		m.mu.Unlock()
		return fmt.Errorf("already running")
	}
	m.ctx, m.cancel = context.WithCancel(ctx)
	m.running = true
	m.mu.Unlock()

	log.Printf("[SIMPLE_MAKER] Starting %s bot for %s...", m.cfg.BotSide, m.cfg.Symbol)

	// Start exchange
	if err := m.exch.Start(m.ctx); err != nil {
		return fmt.Errorf("exchange start failed: %w", err)
	}

	// Subscribe to WebSocket user stream for real-time balance updates
	if err := m.subscribeUserStream(); err != nil {
		log.Printf("[SIMPLE_MAKER] WARNING: Failed to subscribe user stream: %v", err)
		// Continue without WebSocket - will use polling instead
	}

	// Cancel any stale orders
	if err := m.exch.CancelAllOrders(m.ctx, m.cfg.Symbol); err != nil {
		log.Printf("[SIMPLE_MAKER] WARNING: Failed to cancel stale orders: %v", err)
	}

	// Load market info
	if err := m.loadMarketInfo(); err != nil {
		return fmt.Errorf("failed to load market info: %w", err)
	}

	// Start main loop
	go m.mainLoop()

	log.Printf("[SIMPLE_MAKER] Started %s bot: spread=%.0fbps, levels=%d, depth=$%.0f",
		m.cfg.BotSide, m.cfg.SpreadBps, m.cfg.NumLevels, m.cfg.TargetDepthNotional)

	return nil
}

// Stop stops the simple maker
func (m *SimpleMaker) Stop(ctx context.Context) error {
	m.mu.Lock()
	if !m.running {
		m.mu.Unlock()
		return nil
	}
	m.running = false // Mark as not running to prevent new ticks
	m.mu.Unlock()

	log.Printf("[SIMPLE_MAKER] Stopping %s bot...", m.cfg.BotSide)

	// 1. Stop mainLoop FIRST by cancelling context
	if m.cancel != nil {
		m.cancel()
	}

	// 2. Wait a bit for mainLoop to stop
	time.Sleep(100 * time.Millisecond)

	// 3. Now cancel all orders on exchange
	if err := m.cancelAllOrdersWithContext(ctx); err != nil {
		log.Printf("[SIMPLE_MAKER] WARNING: Cancel failed: %v", err)
	} else {
		log.Printf("[SIMPLE_MAKER] Cancelled all %s orders", m.cfg.BotSide)
	}

	// 4. Clear Redis order list
	if m.redis != nil {
		if err := m.redis.ClearAllOrders(ctx, m.cfg.Symbol); err != nil {
			log.Printf("[SIMPLE_MAKER] WARNING: Failed to clear Redis: %v", err)
		}
	}

	// 5. Stop exchange connection
	if err := m.exch.Stop(ctx); err != nil {
		log.Printf("[SIMPLE_MAKER] WARNING: Exchange stop failed: %v", err)
	}

	log.Printf("[SIMPLE_MAKER] Stopped %s bot", m.cfg.BotSide)
	return nil
}

// loadMarketInfo loads tick size, step size, min notional from exchange
func (m *SimpleMaker) loadMarketInfo() error {
	info, err := m.exch.GetExchangeInfo(m.ctx, m.cfg.Symbol)
	if err != nil {
		return err
	}

	for _, sym := range info.Symbols {
		if sym.Symbol == m.cfg.Symbol {
			m.tickSize = math.Pow10(-sym.QuoteAssetPrecision)
			m.stepSize = math.Pow10(-sym.BaseAssetPrecision)
			for _, f := range sym.Filters {
				if f.FilterType == "MIN_NOTIONAL" || f.FilterType == "NOTIONAL" {
					if f.MinNotional != "" {
						m.minNotional, _ = strconv.ParseFloat(f.MinNotional, 64)
					}
				}
			}
			break
		}
	}

	if m.minNotional <= 0 {
		m.minNotional = 5.0
	}

	log.Printf("[SIMPLE_MAKER] Market info: tickSize=%.8f, stepSize=%.8f, minNotional=%.2f",
		m.tickSize, m.stepSize, m.minNotional)

	return nil
}

// subscribeUserStream subscribes to WebSocket user stream for real-time balance updates
func (m *SimpleMaker) subscribeUserStream() error {
	handlers := exchange.UserStreamHandlers{
		OnAccountUpdate: func(event *types.AccountEvent) {
			// Update balances in Redis when balance changes via WebSocket
			if m.redis == nil || m.cfg.Exchange == "" || m.cfg.BotID == "" {
				return
			}

			var allBalances []store.AssetBalance
			for _, b := range event.Balances {
				if b.Free > 0 || b.Locked > 0 {
					allBalances = append(allBalances, store.AssetBalance{
						Asset:  b.Asset,
						Free:   b.Free,
						Locked: b.Locked,
					})
				}
			}

			if len(allBalances) > 0 {
				if err := m.redis.SetMMBalances(m.ctx, m.cfg.Exchange, m.cfg.Symbol, m.cfg.BotID, allBalances); err != nil {
					log.Printf("[WS] Failed to update balances in Redis: %v", err)
				} else {
					log.Printf("[WS] Updated %d balances in Redis", len(allBalances))
				}
			}
		},
		OnFill: func(event *types.FillEvent) {
			// Log fill event
			log.Printf("[WS] Fill: %s %s @ %.8f x %.6f (order=%s)",
				event.Side, event.Symbol, event.Price, event.Quantity, event.OrderID)

			// Clear cached ladder to force regeneration on next tick
			m.mu.Lock()
			m.cachedLadder = nil
			m.mu.Unlock()
		},
		OnOrderUpdate: func(event *types.OrderEvent) {
			// Log order status changes
			if event.Status == "CANCELED" || event.Status == "FILLED" {
				log.Printf("[WS] Order %s: %s %s @ %.8f (status=%s)",
					event.OrderID, event.Side, event.Symbol, event.Price, event.Status)
			}
		},
		OnError: func(err error) {
			log.Printf("[WS] Error: %v", err)
		},
	}

	return m.exch.SubscribeUserStream(m.ctx, handlers)
}

// getAvailableBalance returns the available balance for trading
// For maker-bid: returns quote asset (USDT) free balance
// For maker-ask: returns base asset (BTC) free balance * mid price (as notional)
// BalanceInfo holds free and locked balance
type BalanceInfo struct {
	Free   float64
	Locked float64
}

func (m *SimpleMaker) getAvailableBalance() (float64, error) {
	info, err := m.getBalanceInfo()
	if err != nil {
		return 0, err
	}
	return info.Free, nil
}

// getBalanceInfo returns both free and locked balance for the target asset
// Also saves all balances to Redis if enabled
func (m *SimpleMaker) getBalanceInfo() (*BalanceInfo, error) {
	acct, err := m.exch.GetAccount(m.ctx)
	if err != nil {
		return nil, err
	}

	// Save ALL balances to Redis: balance:{exchange}:{symbol} -> {botId}: {balances}
	if m.redis != nil && m.cfg.Exchange != "" && m.cfg.BotID != "" {
		var allBalances []store.AssetBalance
		for _, b := range acct.Balances {
			if b.Free > 0 || b.Locked > 0 {
				allBalances = append(allBalances, store.AssetBalance{
					Asset:  b.Asset,
					Free:   b.Free,
					Locked: b.Locked,
				})
			}
		}
		if len(allBalances) > 0 {
			if err := m.redis.SetMMBalances(m.ctx, m.cfg.Exchange, m.cfg.Symbol, m.cfg.BotID, allBalances); err != nil {
				log.Printf("[SIMPLE_MAKER] Failed to save balances to Redis: %v", err)
			}
		}
	}

	var targetAsset string
	if m.cfg.BotSide == BotSideBid {
		targetAsset = m.cfg.QuoteAsset // Need USDT to buy
	} else {
		targetAsset = m.cfg.BaseAsset // Need BTC to sell
	}

	for _, b := range acct.Balances {
		if b.Asset == targetAsset {
			free := b.Free
			locked := b.Locked

			if m.cfg.BotSide == BotSideAsk {
				// For ask, we need to convert base to notional value
				depth, err := m.exch.GetDepth(m.ctx, m.cfg.Symbol)
				if err == nil && len(depth.Bids) > 0 {
					bestBid, _ := strconv.ParseFloat(depth.Bids[0][0], 64)
					free = b.Free * bestBid
					locked = b.Locked * bestBid
				}
			}

			return &BalanceInfo{Free: free, Locked: locked}, nil
		}
	}

	return &BalanceInfo{}, nil
}

// cancelAllOrders cancels all orders for this bot's side
// cancelAllOrders cancels all orders on this bot's side (uses internal context)
func (m *SimpleMaker) cancelAllOrders() error {
	return m.cancelAllOrdersWithContext(m.ctx)
}

// cancelAllOrdersWithContext cancels all orders on this bot's side with given context
func (m *SimpleMaker) cancelAllOrdersWithContext(ctx context.Context) error {
	openOrders, err := m.exch.GetOpenOrders(ctx, m.cfg.Symbol)
	if err != nil {
		return err
	}

	side := "BUY"
	if m.cfg.BotSide == BotSideAsk {
		side = "SELL"
	}

	now := time.Now().UnixMilli()
	cancelledCount := 0
	for _, o := range openOrders {
		if o.Side == side {
			if err := m.exch.CancelOrder(ctx, m.cfg.Symbol, o.OrderID); err != nil {
				log.Printf("[SIMPLE_MAKER] Failed to cancel order %s: %v", o.OrderID, err)
				continue
			}

			log.Printf("[SIMPLE_MAKER] Cancelled %s @ %.8f (id=%s)", o.Side, o.Price, o.OrderID)
			cancelledCount++

			// Delete order from Redis
			if m.redis != nil {
				if err := m.redis.DeleteOrder(ctx, m.cfg.Symbol, o.OrderID); err != nil {
					log.Printf("[SIMPLE_MAKER] Failed to delete order from Redis: %v", err)
				} else {
					log.Printf("[REDIS] Deleted order %s from order:%s", o.OrderID, m.cfg.Symbol)
				}
			}

			// Emit cancel event
			if m.onOrderEvent != nil {
				m.onOrderEvent(OrderEvent{
					Type:      OrderEventCancel,
					Symbol:    m.cfg.Symbol,
					OrderID:   o.OrderID,
					Side:      o.Side,
					Price:     o.Price,
					Qty:       o.Quantity,
					Level:     0,
					Reason:    "shutdown",
					Timestamp: now,
				})
			}
		}
	}

	log.Printf("[SIMPLE_MAKER] Cancelled %d %s orders", cancelledCount, side)
	return nil
}

// mainLoop is the core tick loop
func (m *SimpleMaker) mainLoop() {
	interval := time.Duration(m.cfg.TickIntervalMs) * time.Millisecond
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// First tick immediately
	if err := m.tick(); err != nil {
		log.Printf("[SIMPLE_MAKER] Initial tick error: %v", err)
	}

	for {
		select {
		case <-m.ctx.Done():
			log.Println("[SIMPLE_MAKER] Main loop stopped")
			return
		case <-ticker.C:
			if err := m.tick(); err != nil {
				log.Printf("[SIMPLE_MAKER] Tick error: %v", err)
			}

			// Check for config updates periodically
			m.tickCount++
			if m.tickCount >= m.configCheckInterval {
				m.tickCount = 0
				m.checkConfigUpdate()
			}
		}
	}
}

// checkConfigUpdate checks MongoDB for config updates and applies them
func (m *SimpleMaker) checkConfigUpdate() {
	if m.mongo == nil || m.cfg.BotID == "" {
		return
	}

	update, err := m.mongo.CheckConfigUpdate(m.ctx, m.cfg.BotID)
	if err != nil {
		log.Printf("[CONFIG] Failed to check config update: %v", err)
		return
	}

	if !update.IsUpdated || update.SimpleConfig == nil {
		return
	}

	log.Println("[CONFIG] Config updated, applying new settings...")

	// Build and apply new config
	newCfg := &SimpleMakerConfig{
		SpreadBps:           update.SimpleConfig.SpreadMinBps,
		NumLevels:           update.SimpleConfig.NumLevels,
		TargetDepthNotional: update.SimpleConfig.TargetDepthNotional,
		LadderRegenBps:      update.SimpleConfig.LadderRegenBps,
		MinBalanceToTrade:   update.SimpleConfig.MinBalanceToTrade,
		LevelGapTicksMax:    update.SimpleConfig.LevelGapTicksMax,
		DepthBps:            update.SimpleConfig.DepthBps,
	}

	m.UpdateConfig(newCfg)
	log.Println("[CONFIG] New config applied successfully")
}

// tick executes one cycle: check balance -> get market data -> compute desired orders -> place/cancel
func (m *SimpleMaker) tick() error {
	// Check if still running (prevent executing during shutdown)
	m.mu.RLock()
	running := m.running
	m.mu.RUnlock()
	if !running {
		return nil
	}

	// 1. Check balance first - only trade if we have enough funds
	// Note: getBalanceInfo() also saves ALL balances to Redis
	balanceInfo, err := m.getBalanceInfo()
	if err != nil {
		return fmt.Errorf("get balance failed: %w", err)
	}
	availableBalance := balanceInfo.Free

	// Check if we have minimum balance to trade
	minBalance := m.cfg.MinBalanceToTrade
	if minBalance == 0 {
		minBalance = m.minNotional // default to min notional
	}

	if availableBalance < minBalance {
		// Not enough balance - cancel all orders and wait
		log.Printf("[SIMPLE_MAKER] %s: Insufficient balance (%.4f < %.4f), pausing...",
			m.cfg.BotSide, availableBalance, minBalance)
		return m.cancelAllOrders()
	}

	// 2. Get current market data
	depth, err := m.exch.GetDepth(m.ctx, m.cfg.Symbol)
	if err != nil {
		return fmt.Errorf("get depth failed: %w", err)
	}

	if len(depth.Bids) == 0 || len(depth.Asks) == 0 {
		return fmt.Errorf("empty order book")
	}

	bestBid, _ := strconv.ParseFloat(depth.Bids[0][0], 64)
	bestAsk, _ := strconv.ParseFloat(depth.Asks[0][0], 64)
	mid := (bestBid + bestAsk) / 2.0

	if mid <= 0 || bestBid >= bestAsk {
		return fmt.Errorf("invalid market data: bid=%.8f, ask=%.8f", bestBid, bestAsk)
	}

	// 3. Get current open orders first (to detect fills)
	openOrders, err := m.exch.GetOpenOrders(m.ctx, m.cfg.Symbol)
	if err != nil {
		return fmt.Errorf("get open orders failed: %w", err)
	}

	// Filter orders by side
	var currentOrders []*exchange.Order
	side := "BUY"
	if m.cfg.BotSide == BotSideAsk {
		side = "SELL"
	}
	for _, o := range openOrders {
		if o.Side == side {
			currentOrders = append(currentOrders, o)
		}
	}

	// 4. Check if we need to regenerate ladder or use cached
	desired := m.getOrRegenerateLadder(mid, availableBalance, len(currentOrders))

	// 5. Compute diff and execute
	m.executeOrderDiff(desired, currentOrders, mid)

	log.Printf("[SIMPLE_MAKER] %s: mid=%.6f, balance=%.4f, desired=%d, current=%d",
		m.cfg.BotSide, mid, availableBalance, len(desired), len(currentOrders))

	return nil
}

// shouldRegenerateLadder checks if we need to regenerate the ladder
// Regenerate when: mid moves 0.5%+ OR fill detected (order count decreased)
func (m *SimpleMaker) shouldRegenerateLadder(mid, balance float64, currentOrderCount int) (bool, string) {
	// First time - always regenerate
	if m.cachedMid == 0 || len(m.cachedLadder) == 0 {
		return true, "initial"
	}

	// Check if mid moved significantly (in bps)
	midChangeBps := math.Abs(mid-m.cachedMid) / m.cachedMid * 10000
	if midChangeBps > m.ladderRegenBps {
		return true, fmt.Sprintf("mid_moved_%.1fbps", midChangeBps)
	}

	// Check if fill detected (order count decreased = order got filled)
	if m.lastOrderCount > 0 && currentOrderCount < m.lastOrderCount {
		return true, fmt.Sprintf("fill_detected(%d->%d)", m.lastOrderCount, currentOrderCount)
	}

	return false, ""
}

// getOrRegenerateLadder returns cached ladder or generates new one
func (m *SimpleMaker) getOrRegenerateLadder(mid, balance float64, currentOrderCount int) []SimpleDesiredOrder {
	shouldRegen, reason := m.shouldRegenerateLadder(mid, balance, currentOrderCount)

	if shouldRegen {
		m.cachedLadder = m.computeDesiredOrdersWithBalance(mid, balance)
		m.cachedMid = mid
		m.cachedBalance = balance
		log.Printf("[SIMPLE_MAKER] Regenerated ladder: reason=%s, levels=%d", reason, len(m.cachedLadder))
	}

	// Update order count for next tick
	m.lastOrderCount = currentOrderCount

	return m.cachedLadder
}

// SimpleDesiredOrder represents an order we want to have
type SimpleDesiredOrder struct {
	Side  string
	Price float64
	Qty   float64
	Level int
}

// computeDesiredOrders computes the orders we want based on config
// Adds randomization to price and size to make orders look natural
func (m *SimpleMaker) computeDesiredOrders(mid float64) []SimpleDesiredOrder {
	orders := make([]SimpleDesiredOrder, 0, m.cfg.NumLevels)

	// Size per level = target_depth / num_levels (single side)
	baseSizeNotional := m.cfg.TargetDepthNotional / float64(m.cfg.NumLevels)

	side := "BUY"
	if m.cfg.BotSide == BotSideAsk {
		side = "SELL"
	}

	for level := 0; level < m.cfg.NumLevels; level++ {
		// Base offset increases with level: spread * (level + 1)
		baseOffsetBps := m.cfg.SpreadBps * float64(level+1)

		// Add random jitter to price offset: ±PriceJitterPct
		// e.g., if PriceJitterPct=0.2 and baseOffset=50bps, jitter is ±10bps
		priceJitter := baseOffsetBps * m.cfg.PriceJitterPct * (2*rand.Float64() - 1)
		offsetBps := baseOffsetBps + priceJitter
		if offsetBps < m.cfg.SpreadBps*0.5 {
			offsetBps = m.cfg.SpreadBps * 0.5 // minimum offset
		}

		offsetMult := offsetBps / 10000.0

		var price float64
		if m.cfg.BotSide == BotSideBid {
			// BUY: below mid
			price = mid * (1.0 - offsetMult)
		} else {
			// SELL: above mid
			price = mid * (1.0 + offsetMult)
		}

		// Round price to tick size
		price = m.roundToTick(price)

		// Add random jitter to size: ±SizeJitterPct
		// e.g., if SizeJitterPct=0.3 and baseSize=$200, size ranges from $140 to $260
		sizeJitter := 1.0 + m.cfg.SizeJitterPct*(2*rand.Float64()-1)
		sizeNotional := baseSizeNotional * sizeJitter

		// Calculate quantity
		qty := sizeNotional / price
		qty = m.roundToStep(qty)

		// Skip if below minimum notional
		if price*qty < m.minNotional {
			continue
		}

		orders = append(orders, SimpleDesiredOrder{
			Side:  side,
			Price: price,
			Qty:   qty,
			Level: level,
		})
	}

	return orders
}

// computeDesiredOrdersWithBalance computes orders considering available balance
// This ensures we don't place orders that exceed our available funds
func (m *SimpleMaker) computeDesiredOrdersWithBalance(mid float64, availableBalance float64) []SimpleDesiredOrder {
	orders := make([]SimpleDesiredOrder, 0, m.cfg.NumLevels)

	// Calculate how much depth we can actually provide based on balance
	// Use the smaller of target_depth and available_balance
	effectiveDepth := m.cfg.TargetDepthNotional
	if availableBalance < effectiveDepth {
		effectiveDepth = availableBalance * 0.9 // Use 90% of available to leave buffer
	}

	// Size per level
	baseSizeNotional := effectiveDepth / float64(m.cfg.NumLevels)

	side := "BUY"
	if m.cfg.BotSide == BotSideAsk {
		side = "SELL"
	}

	var totalNotionalUsed float64

	// Calculate depth limit prices (±depth_bps from mid)
	depthBps := m.cfg.DepthBps
	if depthBps <= 0 {
		depthBps = 200 // default 200 bps = 2%
	}
	var minPrice, maxPrice float64
	if m.cfg.BotSide == BotSideBid {
		minPrice = mid * (1.0 - depthBps/10000.0) // lowest bid allowed
		maxPrice = mid                            // highest bid = mid
	} else {
		minPrice = mid                            // lowest ask = mid
		maxPrice = mid * (1.0 + depthBps/10000.0) // highest ask allowed
	}

	// Calculate first level price (at spread_bps from mid)
	offsetMult := m.cfg.SpreadBps / 10000.0
	var firstPrice float64
	if m.cfg.BotSide == BotSideBid {
		firstPrice = mid * (1.0 - offsetMult)
	} else {
		firstPrice = mid * (1.0 + offsetMult)
	}
	firstPrice = m.roundToTick(firstPrice)

	// Max random gap between levels (default 3 ticks)
	maxGapTicks := m.cfg.LevelGapTicksMax
	if maxGapTicks <= 0 {
		maxGapTicks = 3
	}

	// DEBUG: Log ladder calculation params
	priceRange := maxPrice - minPrice
	if m.cfg.BotSide == BotSideBid {
		priceRange = firstPrice - minPrice
	} else {
		priceRange = maxPrice - firstPrice
	}
	ticksAvailable := int(priceRange / m.tickSize)
	log.Printf("[DEBUG] Ladder params: mid=%.8f, tickSize=%.8f, depthBps=%.0f", mid, m.tickSize, depthBps)
	log.Printf("[DEBUG] Price range: min=%.8f, max=%.8f, first=%.8f", minPrice, maxPrice, firstPrice)
	log.Printf("[DEBUG] Ticks available: %d, numLevels=%d, gapTicksMax=%d", ticksAvailable, m.cfg.NumLevels, maxGapTicks)
	log.Printf("[DEBUG] Size per level: $%.2f (total=$%.2f, balance=$%.2f)", baseSizeNotional, effectiveDepth, availableBalance)

	// Current price starts at first level
	currentPrice := firstPrice

	for level := 0; level < m.cfg.NumLevels; level++ {
		// Check if we still have balance left
		remainingBalance := availableBalance - totalNotionalUsed
		if remainingBalance < m.minNotional {
			break // Stop placing orders if not enough balance
		}

		// Price: random gap between levels (1 to maxGapTicks ticks)
		// Level 0 uses firstPrice, subsequent levels have random gap
		var price float64
		if level == 0 {
			price = currentPrice
		} else {
			// Random gap: 1 to maxGapTicks ticks
			gapTicks := 1 + rand.Intn(maxGapTicks)
			if m.cfg.BotSide == BotSideBid {
				currentPrice = currentPrice - m.tickSize*float64(gapTicks)
			} else {
				currentPrice = currentPrice + m.tickSize*float64(gapTicks)
			}
			price = currentPrice
		}
		price = m.roundToTick(price)

		// Check depth limit - stop if price is outside allowed range
		if m.cfg.BotSide == BotSideBid && price < minPrice {
			log.Printf("[SIMPLE_MAKER] Depth limit reached at level %d (price %.8f < min %.8f)", level, price, minPrice)
			break
		}
		if m.cfg.BotSide == BotSideAsk && price > maxPrice {
			log.Printf("[SIMPLE_MAKER] Depth limit reached at level %d (price %.8f > max %.8f)", level, price, maxPrice)
			break
		}

		// Only randomize SIZE, not price
		sizeJitter := 1.0 + m.cfg.SizeJitterPct*(2*rand.Float64()-1)
		sizeNotional := baseSizeNotional * sizeJitter

		// Don't exceed remaining balance
		if sizeNotional > remainingBalance {
			sizeNotional = remainingBalance * 0.95
		}

		// Calculate quantity
		qty := sizeNotional / price
		qty = m.roundToStep(qty)

		// Skip if below minimum notional
		orderNotional := price * qty
		if orderNotional < m.minNotional {
			continue
		}

		orders = append(orders, SimpleDesiredOrder{
			Side:  side,
			Price: price,
			Qty:   qty,
			Level: level,
		})

		totalNotionalUsed += orderNotional
	}

	return orders
}

// executeOrderDiff cancels orders that don't match and places new ones
func (m *SimpleMaker) executeOrderDiff(desired []SimpleDesiredOrder, current []*exchange.Order, mid float64) {
	now := time.Now().UnixMilli()

	// Create map of current orders by price (rounded)
	currentByPrice := make(map[float64]*exchange.Order)
	for _, o := range current {
		priceKey := m.roundToTick(o.Price)
		currentByPrice[priceKey] = o
	}

	// Track which current orders are still needed
	usedOrders := make(map[string]bool)

	// For each desired order, check if we have a matching current order
	for _, d := range desired {
		priceKey := m.roundToTick(d.Price)

		if existing, ok := currentByPrice[priceKey]; ok {
			// Check if qty is close enough (within 5%)
			qtyDiff := math.Abs(existing.Quantity-d.Qty) / d.Qty
			if qtyDiff < 0.05 {
				// Keep this order
				usedOrders[existing.OrderID] = true
				continue
			}
		}

		// Need to place new order
		m.placeOrder(d, now)
	}

	// Cancel orders that are not needed
	for _, o := range current {
		if !usedOrders[o.OrderID] {
			m.cancelOrder(o, now)
		}
	}
}

// placeOrder places a single order
func (m *SimpleMaker) placeOrder(d SimpleDesiredOrder, timestamp int64) {
	clientOrderID := fmt.Sprintf("SM_%d_L%d_%s", timestamp, d.Level, m.cfg.BotSide)

	order, err := m.exch.PlaceOrder(m.ctx, &exchange.OrderRequest{
		Symbol:        m.cfg.Symbol,
		Side:          d.Side,
		Type:          "LIMIT",
		Price:         d.Price,
		Quantity:      d.Qty,
		ClientOrderID: clientOrderID,
	})

	if err != nil {
		log.Printf("[SIMPLE_MAKER] Place order failed: %v", err)
		return
	}

	log.Printf("[SIMPLE_MAKER] Placed %s L%d @ %.8f x %.6f (id=%s)",
		d.Side, d.Level, d.Price, d.Qty, order.OrderID)

	// Save order to Redis
	if m.redis != nil {
		orderInfo := &store.OrderInfo{
			OrderID:       order.OrderID,
			ClientOrderID: clientOrderID,
			Symbol:        m.cfg.Symbol,
			Side:          d.Side,
			Price:         d.Price,
			Quantity:      d.Qty,
			CreatedAt:     timestamp,
			Status:        "NEW",
		}
		if err := m.redis.SaveOrder(m.ctx, orderInfo); err != nil {
			log.Printf("[SIMPLE_MAKER] Failed to save order to Redis: %v", err)
		} else {
			log.Printf("[REDIS] Saved order %s to order:%s", order.OrderID, m.cfg.Symbol)
		}
	} else {
		log.Printf("[SIMPLE_MAKER] WARNING: Redis is nil, cannot save order")
	}

	// Emit event
	if m.onOrderEvent != nil {
		m.onOrderEvent(OrderEvent{
			Type:      OrderEventPlace,
			Symbol:    m.cfg.Symbol,
			OrderID:   order.OrderID,
			Side:      d.Side,
			Price:     d.Price,
			Qty:       d.Qty,
			Level:     d.Level,
			Reason:    "new_level",
			Timestamp: timestamp,
		})
	}
}

// cancelOrder cancels a single order
func (m *SimpleMaker) cancelOrder(o *exchange.Order, timestamp int64) {
	err := m.exch.CancelOrder(m.ctx, m.cfg.Symbol, o.OrderID)
	if err != nil {
		log.Printf("[SIMPLE_MAKER] Cancel order failed: %v", err)
		return
	}

	log.Printf("[SIMPLE_MAKER] Cancelled %s @ %.8f (id=%s)", o.Side, o.Price, o.OrderID)

	// Delete order from Redis
	if m.redis != nil {
		if err := m.redis.DeleteOrder(m.ctx, m.cfg.Symbol, o.OrderID); err != nil {
			log.Printf("[SIMPLE_MAKER] Failed to delete order from Redis: %v", err)
		} else {
			log.Printf("[REDIS] Deleted order %s from order:%s", o.OrderID, m.cfg.Symbol)
		}
	} else {
		log.Printf("[SIMPLE_MAKER] WARNING: Redis is nil, cannot delete order")
	}

	// Emit event
	if m.onOrderEvent != nil {
		m.onOrderEvent(OrderEvent{
			Type:      OrderEventCancel,
			Symbol:    m.cfg.Symbol,
			OrderID:   o.OrderID,
			Side:      o.Side,
			Price:     o.Price,
			Qty:       o.Quantity,
			Level:     0,
			Reason:    "not_needed",
			Timestamp: timestamp,
		})
	}
}

// roundToTick rounds price to tick size
func (m *SimpleMaker) roundToTick(price float64) float64 {
	if m.tickSize <= 0 {
		return price
	}
	return math.Round(price/m.tickSize) * m.tickSize
}

// roundToStep rounds quantity to step size
func (m *SimpleMaker) roundToStep(qty float64) float64 {
	if m.stepSize <= 0 {
		return qty
	}
	return math.Floor(qty/m.stepSize) * m.stepSize
}

// GetSide returns the bot side
func (m *SimpleMaker) GetSide() BotSide {
	return m.cfg.BotSide
}

// UpdateConfig updates the maker config and clears cached ladder to force regeneration
func (m *SimpleMaker) UpdateConfig(newCfg *SimpleMakerConfig) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Update config fields that can be changed at runtime
	m.cfg.SpreadBps = newCfg.SpreadBps
	m.cfg.NumLevels = newCfg.NumLevels
	m.cfg.TargetDepthNotional = newCfg.TargetDepthNotional
	m.cfg.TickIntervalMs = newCfg.TickIntervalMs
	m.cfg.LadderRegenBps = newCfg.LadderRegenBps
	m.cfg.MinBalanceToTrade = newCfg.MinBalanceToTrade
	m.cfg.LevelGapTicksMax = newCfg.LevelGapTicksMax
	m.cfg.SizeJitterPct = newCfg.SizeJitterPct
	m.cfg.DepthBps = newCfg.DepthBps

	// Clear cached ladder to force regeneration with new config
	m.cachedLadder = nil
	m.cachedMid = 0
	m.cachedBalance = 0

	log.Printf("[SIMPLE_MAKER] Config updated: spread=%.0fbps, levels=%d, depth=$%.0f, depthBps=%.0f, regenBps=%.0f",
		m.cfg.SpreadBps, m.cfg.NumLevels, m.cfg.TargetDepthNotional, m.cfg.DepthBps, m.cfg.LadderRegenBps)
}
