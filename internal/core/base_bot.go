package core

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"mm-platform-engine/internal/exchange"
	"mm-platform-engine/internal/store"
	"mm-platform-engine/internal/types"
)

// BaseBot provides common infrastructure for all market-making bots.
// It handles lifecycle management, tick loop, WS subscriptions, and
// delegates strategy-specific logic to the Strategy interface.
type BaseBot struct {
	cfg      *BaseBotConfig
	strategy Strategy

	// Dependencies
	exch  exchange.Exchange
	redis *store.RedisStore
	mongo *store.MongoStore

	// Components
	balanceTracker *BalanceTracker
	orderTracker   *OrderTracker
	marketData     *MarketDataCache

	// State
	mu      sync.RWMutex
	running bool
	ctx     context.Context
	cancel  context.CancelFunc
	mode    Mode

	// Replace lock - prevent race condition with double orders
	replaceMu    sync.Mutex
	replacing    bool
	replaceStart time.Time

	// Tick tracking
	tickCount           int
	configCheckInterval int
	lastTickTime        int64
	lastSyncTime        int64 // Last order sync timestamp (ms)

	// Track recently filled orders (to ignore late NEW events)
	filledOrders map[string]int64 // orderID -> fill timestamp

	// Track WS-confirmed orders (to prevent duplicate WS emissions)
	confirmedOrders map[string]bool // orderID -> confirmed

	// Callbacks
	onOrderEvent OrderEventCallback
}

// NewBaseBot creates a new BaseBot with the given configuration and strategy.
func NewBaseBot(
	cfg *BaseBotConfig,
	strategy Strategy,
	exch exchange.Exchange,
	redis *store.RedisStore,
	mongo *store.MongoStore,
) *BaseBot {
	// Set defaults
	if cfg.TickIntervalMs == 0 {
		cfg.TickIntervalMs = 5000 // 5 seconds
	}
	if cfg.RateLimitOrdersPerSec == 0 {
		cfg.RateLimitOrdersPerSec = 10
	}
	if cfg.BatchSize == 0 {
		cfg.BatchSize = 10
	}

	// Calculate config check interval (every 10 seconds worth of ticks)
	configCheckInterval := cfg.ConfigCheckInterval
	if configCheckInterval == 0 {
		configCheckInterval = 10000 / cfg.TickIntervalMs
		if configCheckInterval < 1 {
			configCheckInterval = 1
		}
	}

	bot := &BaseBot{
		cfg:                 cfg,
		strategy:            strategy,
		exch:                exch,
		redis:               redis,
		mongo:               mongo,
		mode:                ModeNormal,
		configCheckInterval: configCheckInterval,
		filledOrders:        make(map[string]int64),
		confirmedOrders:     make(map[string]bool),
	}

	// Initialize components
	bot.balanceTracker = NewBalanceTracker(cfg.BaseAsset, cfg.QuoteAsset)
	bot.orderTracker = NewOrderTracker()
	bot.marketData = NewMarketDataCache()

	// Use last trade price by default (better for thin orderbooks where we are the only MM)
	bot.marketData.UseLastTradePrice(true)

	return bot
}

// SetOrderEventCallback sets callback for order events (for WS broadcasting)
func (b *BaseBot) SetOrderEventCallback(cb OrderEventCallback) {
	b.onOrderEvent = cb
}

// Start initializes and runs the bot
func (b *BaseBot) Start(ctx context.Context) error {
	b.mu.Lock()
	if b.running {
		b.mu.Unlock()
		return fmt.Errorf("bot already running")
	}
	b.ctx, b.cancel = context.WithCancel(ctx)
	b.running = true
	b.mu.Unlock()

	log.Printf("[%s] Starting %s bot for %s...", b.strategy.Name(), b.cfg.BotType, b.cfg.Symbol)

	// 1. Start exchange
	if err := b.exch.Start(b.ctx); err != nil {
		return fmt.Errorf("exchange start failed: %w", err)
	}

	// 2. Cancel any stale orders from previous run
	if err := b.exch.CancelAllOrders(b.ctx, b.cfg.Symbol); err != nil {
		log.Printf("[%s] WARNING: Failed to cancel stale orders: %v", b.strategy.Name(), err)
	}

	// 3. Subscribe to WebSocket user stream
	if err := b.subscribeUserStream(); err != nil {
		log.Printf("[%s] WARNING: Failed to subscribe user stream: %v", b.strategy.Name(), err)
		// Continue without WebSocket - will use polling instead
	}

	// 4. Load market info
	if err := b.loadMarketInfo(); err != nil {
		return fmt.Errorf("failed to load market info: %w", err)
	}

	// 5. Sync current open orders
	if err := b.syncLiveOrders(); err != nil {
		log.Printf("[%s] WARNING: Failed to sync live orders: %v", b.strategy.Name(), err)
	}

	// 6. Get initial balance and market snapshot
	balance, err := b.getBalanceState()
	if err != nil {
		return fmt.Errorf("failed to get initial balance: %w", err)
	}

	// Publish initial balances to Redis
	b.publishAllBalancesToRedis()

	snap, err := b.getSnapshot()
	if err != nil {
		return fmt.Errorf("failed to get initial snapshot: %w", err)
	}

	// 7. Initialize strategy
	if err := b.strategy.Init(b.ctx, snap, balance); err != nil {
		return fmt.Errorf("strategy init failed: %w", err)
	}

	// 8. Start main loop
	go b.mainLoop()

	log.Printf("[%s] Started successfully: symbol=%s, tick=%dms",
		b.strategy.Name(), b.cfg.Symbol, b.cfg.TickIntervalMs)

	return nil
}

// Stop gracefully shuts down the bot
func (b *BaseBot) Stop(ctx context.Context) error {
	b.mu.Lock()
	if !b.running {
		b.mu.Unlock()
		return nil
	}
	b.running = false
	b.mu.Unlock()

	log.Printf("[%s] Stopping...", b.strategy.Name())

	// 1. Stop mainLoop first by cancelling context
	if b.cancel != nil {
		b.cancel()
	}

	// 2. Wait a bit for mainLoop to stop
	time.Sleep(100 * time.Millisecond)

	// 3. Cancel all orders on exchange
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := b.exch.CancelAllOrders(shutdownCtx, b.cfg.Symbol); err != nil {
		log.Printf("[%s] WARNING: Cancel failed on shutdown: %v", b.strategy.Name(), err)
	} else {
		log.Printf("[%s] Cancelled all orders", b.strategy.Name())
	}

	// Wait for cancels to propagate before proceeding
	log.Printf("[%s] Waiting for cancel confirmations...", b.strategy.Name())
	time.Sleep(2 * time.Second)

	// 4. Emit cancel events for all tracked orders (for WebSocket broadcast)
	if b.onOrderEvent != nil {
		allOrders := b.orderTracker.GetAll()
		for _, order := range allOrders {
			b.onOrderEvent(BotOrderEvent{
				Type:      OrderEventTypeCancel,
				Symbol:    b.cfg.Symbol,
				OrderID:   order.OrderID,
				Side:      order.Side,
				Price:     order.Price,
				Qty:       order.Qty,
				Reason:    "shutdown",
				Timestamp: time.Now().UnixMilli(),
			})
		}
		if len(allOrders) > 0 {
			log.Printf("[%s] Emitted %d cancel events for shutdown", b.strategy.Name(), len(allOrders))
		}
	}

	// 5. Clear orders from Redis
	if b.redis != nil && b.cfg.BotID != "" {
		removed, err := b.redis.ClearOrdersByBotID(shutdownCtx, b.cfg.Exchange, b.cfg.Symbol, b.cfg.BotID)
		if err != nil {
			log.Printf("[%s] WARNING: Failed to clear orders from Redis: %v", b.strategy.Name(), err)
		} else {
			log.Printf("[%s] Cleared %d orders from Redis", b.strategy.Name(), removed)
		}
	}

	// 6. Clear balances from Redis
	if b.redis != nil && b.cfg.BotID != "" && b.cfg.Exchange != "" {
		if err := b.redis.ClearMMBalances(shutdownCtx, b.cfg.Exchange, b.cfg.Symbol, b.cfg.BotID); err != nil {
			log.Printf("[%s] WARNING: Failed to clear balances from Redis: %v", b.strategy.Name(), err)
		} else {
			log.Printf("[%s] Cleared balances from Redis", b.strategy.Name())
		}
	}

	// 7. Stop exchange connection
	if err := b.exch.Stop(shutdownCtx); err != nil {
		log.Printf("[%s] WARNING: Exchange stop failed: %v", b.strategy.Name(), err)
	}

	log.Printf("[%s] Stopped", b.strategy.Name())
	return nil
}

// mainLoop is the core tick loop
func (b *BaseBot) mainLoop() {
	interval := time.Duration(b.cfg.TickIntervalMs) * time.Millisecond
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// First tick immediately
	if err := b.tick(); err != nil {
		log.Printf("[%s] Initial tick error: %v", b.strategy.Name(), err)
	}

	for {
		select {
		case <-b.ctx.Done():
			log.Printf("[%s] Main loop stopped", b.strategy.Name())
			return
		case <-ticker.C:
			if err := b.tick(); err != nil {
				log.Printf("[%s] Tick error: %v", b.strategy.Name(), err)
			}

			// Check for config updates periodically
			b.tickCount++
			if b.tickCount >= b.configCheckInterval {
				b.tickCount = 0
				b.checkConfigUpdate()
			}
		}
	}
}

// tick executes one tick cycle
func (b *BaseBot) tick() error {
	// Check if still running
	b.mu.RLock()
	running := b.running
	b.mu.RUnlock()
	if !running {
		return nil
	}

	now := time.Now().UnixMilli()
	b.lastTickTime = now

	// 1. Fetch last trade price (before snapshot so it can be used as mid)
	if ticker, err := b.exch.GetTicker(b.ctx, b.cfg.Symbol); err == nil && ticker > 0 {
		b.marketData.SetLastTradePrice(ticker)
	}

	// 2. Get current market snapshot
	snap, err := b.getSnapshot()
	if err != nil {
		return fmt.Errorf("snapshot failed: %w", err)
	}

	// Validate market data
	if snap.Mid <= 0 || snap.BestBid >= snap.BestAsk {
		b.mode = ModePaused
		return b.cancelAllOrders("invalid market data")
	}

	// 2. Get current balance
	balance, err := b.getBalanceState()
	if err != nil {
		return fmt.Errorf("balance failed: %w", err)
	}

	// 3. Get live orders (sync with exchange based on interval)
	// SyncOrdersIntervalMs: 0 = every tick, >0 = every N ms
	shouldSync := false
	if b.cfg.SyncOrdersIntervalMs == 0 {
		// Sync every tick
		shouldSync = true
	} else if b.cfg.SyncOrdersIntervalMs > 0 {
		// Sync based on interval
		elapsed := now - b.lastSyncTime
		if elapsed >= int64(b.cfg.SyncOrdersIntervalMs) {
			shouldSync = true
		}
	}
	// Also sync if tracker is empty (first tick or after clear)
	if b.orderTracker.Count() == 0 {
		shouldSync = true
	}

	if shouldSync {
		if err := b.syncLiveOrders(); err != nil {
			log.Printf("[%s] WARNING: Failed to sync live orders: %v", b.strategy.Name(), err)
			// Continue with cached tracker data
		} else {
			b.lastSyncTime = now
		}
	}
	liveOrders := b.orderTracker.GetAll()

	// 4. Get recent trades (for VWAP fair price calculation)
	// Use 100 trades limit (should cover ~10-15 minutes on most markets)
	recentTrades, err := b.exch.GetRecentTrades(b.ctx, b.cfg.Symbol, 100)
	if err != nil {
		// Log warning but continue - strategy will use fallback
		log.Printf("[BaseBot] ⚠️  Failed to get recent trades: %v (will use fallback)", err)
		recentTrades = []exchange.Trade{} // Empty trades
	}

	// 5. Build tick input
	input := &TickInput{
		Snapshot:     snap,
		Balance:      balance,
		LiveOrders:   liveOrders,
		Timestamp:    now,
		Mode:         b.mode,
		RecentTrades: recentTrades,
	}

	// 5. Execute strategy tick
	output, err := b.strategy.Tick(b.ctx, input)
	if err != nil {
		return fmt.Errorf("strategy tick failed: %w", err)
	}

	// 6. Update mode if strategy changed it
	if output.NewMode != "" {
		b.mode = output.NewMode
	}

	// 7. Log tick summary (before action, so it always logs)
	invRatio, invDev := balance.ComputeInventory(snap.Mid, 0.5) // TODO: get target ratio from strategy
	log.Printf("[%s] mid=%.8f, inv=%.2f%% (dev=%.2f%%), bid=%.4f, ask=%.4f, orders=%d, action=%s",
		b.strategy.Name(), snap.Mid, invRatio*100, invDev*100, balance.QuoteLocked, balance.BaseLocked, len(liveOrders), output.Action)

	// 8. Execute the action
	switch output.Action {
	case TickActionCancelAll:
		return b.cancelAllOrders(output.Reason)
	case TickActionKeep:
		// Do nothing
	case TickActionReplace:
		if err := b.replaceOrders(output.DesiredOrders, output.Reason); err != nil {
			return err
		}
		b.updateStrategyPrevSnapshot()
	case TickActionAmend:
		if err := b.amendOrders(output.OrdersToCancel, output.OrdersToAdd, output.Reason); err != nil {
			return err
		}
		b.updateStrategyPrevSnapshot()
	}

	return nil
}

// updateStrategyPrevSnapshot fetches fresh live orders and balance after order execution,
// then updates the strategy's prev snapshot for accurate fill detection next cycle.
func (b *BaseBot) updateStrategyPrevSnapshot() {
	if err := b.syncLiveOrders(); err != nil {
		log.Printf("[%s] WARNING: post-execute sync failed: %v", b.strategy.Name(), err)
		return
	}
	bal, err := b.getBalanceState()
	if err != nil {
		log.Printf("[%s] WARNING: post-execute balance failed: %v", b.strategy.Name(), err)
		return
	}
	b.strategy.UpdatePrevSnapshot(b.orderTracker.GetAll(), bal)
}

// subscribeUserStream sets up WebSocket event handlers
func (b *BaseBot) subscribeUserStream() error {
	handlers := exchange.UserStreamHandlers{
		OnAccountUpdate: b.handleAccountUpdate,
		OnOrderUpdate:   b.handleOrderUpdate,
		OnError: func(err error) {
			log.Printf("[%s] WebSocket error: %v", b.strategy.Name(), err)
		},
	}

	return b.exch.SubscribeUserStream(b.ctx, handlers)
}

func (b *BaseBot) handleAccountUpdate(event *types.AccountEvent) {
	// Update balance tracker
	b.balanceTracker.UpdateFromEvent(event)

	// Publish ALL cached balances to Redis (not just changed ones from event)
	if b.redis != nil && b.cfg.Exchange != "" && b.cfg.BotID != "" {
		cachedBalances := b.balanceTracker.GetAllBalances()
		var allBalances []store.AssetBalance
		for _, bal := range cachedBalances {
			if bal.Free > 0 || bal.Locked > 0 {
				allBalances = append(allBalances, store.AssetBalance{
					Asset:  bal.Asset,
					Free:   bal.Free,
					Locked: bal.Locked,
				})
			}
		}
		if len(allBalances) > 0 {
			b.redis.SetMMBalances(b.ctx, b.cfg.Exchange, b.cfg.Symbol, b.cfg.BotID, allBalances)
		}
	}
}

// publishAllBalancesToRedis publishes all cached balances to Redis
func (b *BaseBot) publishAllBalancesToRedis() {
	if b.redis == nil || b.cfg.Exchange == "" || b.cfg.BotID == "" {
		return
	}

	cachedBalances := b.balanceTracker.GetAllBalances()
	var allBalances []store.AssetBalance
	for _, bal := range cachedBalances {
		if bal.Free > 0 || bal.Locked > 0 {
			allBalances = append(allBalances, store.AssetBalance{
				Asset:  bal.Asset,
				Free:   bal.Free,
				Locked: bal.Locked,
			})
		}
	}
	if len(allBalances) > 0 {
		b.redis.SetMMBalances(b.ctx, b.cfg.Exchange, b.cfg.Symbol, b.cfg.BotID, allBalances)
	}
}

func (b *BaseBot) handleOrderUpdate(event *types.OrderEvent) {
	// WatchOrders is the single source of truth for all order state changes.
	// Process all statuses — no early returns for FILLED.

	switch event.Status {
	case "NEW":
		// Ignore late NEW for already-filled or already-confirmed orders
		b.mu.RLock()
		_, wasFilled := b.filledOrders[event.OrderID]
		wasConfirmed := b.confirmedOrders[event.OrderID]
		b.mu.RUnlock()
		if wasFilled || wasConfirmed {
			return
		}

		b.mu.Lock()
		b.confirmedOrders[event.OrderID] = true
		b.mu.Unlock()

		if existing := b.orderTracker.Get(event.OrderID); existing == nil {
			b.orderTracker.Add(&LiveOrder{
				OrderID:       event.OrderID,
				ClientOrderID: event.ClientOrderID,
				Side:          event.Side,
				Price:         event.Price,
				Qty:           event.Quantity,
				RemainingQty:  event.Quantity,
				LevelIndex:    parseLevelFromTag(event.ClientOrderID),
				PlacedAt:      event.Timestamp,
			})
		}

		if b.redis != nil {
			b.redis.SaveOrder(b.ctx, &store.OrderInfo{
				OrderID:       event.OrderID,
				ClientOrderID: event.ClientOrderID,
				Exchange:      b.cfg.Exchange,
				Symbol:        event.Symbol,
				Side:          event.Side,
				Price:         event.Price,
				Quantity:      event.Quantity,
				CreatedAt:     event.Timestamp.UnixMilli(),
				Status:        "NEW",
				BotID:         b.cfg.BotID,
			})
		}

	case "PARTIALLY_FILLED":
		b.orderTracker.UpdateRemaining(event.OrderID, event.Quantity-event.ExecutedQty)

	case "FILLED", "CANCELED", "EXPIRED", "REJECTED":
		// Guard: Bybit WatchOrders sends full snapshots that repeat closed/canceled orders.
		// Only process terminal state once — skip if already handled.
		b.mu.RLock()
		_, alreadyTerminal := b.filledOrders[event.OrderID]
		b.mu.RUnlock()
		if alreadyTerminal {
			return
		}

		existingOrder := b.orderTracker.Get(event.OrderID) // capture before removal
		b.orderTracker.Remove(event.OrderID)
		b.mu.Lock()
		delete(b.confirmedOrders, event.OrderID)
		b.filledOrders[event.OrderID] = time.Now().UnixMilli()
		cutoff := time.Now().UnixMilli() - 300000 // 5 minutes
		for id, ts := range b.filledOrders {
			if ts < cutoff {
				delete(b.filledOrders, id)
			}
		}
		b.mu.Unlock()

		if b.redis != nil {
			b.redis.DeleteOrder(b.ctx, b.cfg.Exchange, b.cfg.Symbol, event.OrderID)
		}

		// Fill-specific handling (FILLED only)
		if event.Status == "FILLED" {
			now := time.Now()
			latency := now.Sub(event.Timestamp)
			log.Printf("[FILL] %s %s @ %.8f x %.6f (order=%s) latency=%v",
				event.Side, event.Symbol, event.Price, event.ExecutedQty, event.OrderID, latency)
			b.marketData.SetLastTradePrice(event.Price)

			clientOrderID := event.ClientOrderID
			levelIndex := 0
			if existingOrder != nil {
				if existingOrder.ClientOrderID != "" {
					clientOrderID = existingOrder.ClientOrderID
				}
				levelIndex = existingOrder.LevelIndex
			}
			b.strategy.OnFill(&FillEvent{
				OrderID:       event.OrderID,
				ClientOrderID: clientOrderID,
				Symbol:        event.Symbol,
				Side:          event.Side,
				Price:         event.Price,
				Quantity:      event.ExecutedQty,
				Timestamp:     event.Timestamp,
			})

			// Persist fill to MongoDB
			if b.mongo != nil {
				b.mongo.SaveFill(b.ctx, &types.FillEvent{
					OrderID:       event.OrderID,
					ClientOrderID: clientOrderID,
					Symbol:        event.Symbol,
					Side:          event.Side,
					Price:         event.Price,
					Quantity:      event.ExecutedQty,
					Timestamp:     event.Timestamp,
					BotID:         b.cfg.BotID,
					Exchange:      b.cfg.Exchange,
				})
			}
			_ = levelIndex
		}
	}

	// Forward to strategy
	b.strategy.OnOrderUpdate(&OrderEvent{
		OrderID:       event.OrderID,
		ClientOrderID: event.ClientOrderID,
		Symbol:        event.Symbol,
		Side:          event.Side,
		Status:        event.Status,
		Price:         event.Price,
		Quantity:      event.Quantity,
		ExecutedQty:   event.ExecutedQty,
		Timestamp:     event.Timestamp,
	})

	// Emit order event callback
	if b.onOrderEvent != nil {
		var eventType OrderEventType
		switch event.Status {
		case "NEW":
			eventType = OrderEventTypePlace
		case "CANCELED":
			eventType = OrderEventTypeCancel
		case "PARTIALLY_FILLED":
			eventType = OrderEventTypePartialFill
		case "FILLED":
			eventType = OrderEventTypeFill
		default:
			eventType = OrderEventType(event.Status)
		}
		b.onOrderEvent(BotOrderEvent{
			Type:      eventType,
			Symbol:    event.Symbol,
			OrderID:   event.OrderID,
			Side:      event.Side,
			Price:     event.Price,
			Qty:       event.Quantity,
			Level:     parseLevelFromTag(event.ClientOrderID),
			Reason:    event.Status,
			Timestamp: event.Timestamp.UnixMilli(),
		})
	}

	// Publish all status changes to Redis Stream
	if b.redis != nil {
		if err := b.redis.PublishOrderUpdate(b.ctx, event); err != nil {
			log.Printf("[%s] Failed to publish order update to Redis: %v", b.strategy.Name(), err)
		}
	}
}

// loadMarketInfo loads tick size, step size, min notional from exchange
func (b *BaseBot) loadMarketInfo() error {
	info, err := b.exch.GetExchangeInfo(b.ctx, b.cfg.Symbol)
	if err != nil {
		return err
	}

	return b.marketData.UpdateFromExchangeInfo(info, b.cfg.Symbol)
}

// syncLiveOrders fetches current open orders from exchange and reconciles Redis.
// Any order present on exchange but missing from Redis is pushed before returning.
// Any order in Redis but gone from exchange is removed.
func (b *BaseBot) syncLiveOrders() error {
	orders, err := b.exch.GetOpenOrders(b.ctx, b.cfg.Symbol)
	if err != nil {
		return err
	}

	// Save recently placed orders before clearing (exchange may not list them yet)
	const recentGrace = 10 * time.Second
	recentOrders := b.orderTracker.GetAll()

	b.orderTracker.Clear()
	exchangeIDs := make(map[string]struct{}, len(orders))
	for _, o := range orders {
		exchangeIDs[o.OrderID] = struct{}{}
		b.orderTracker.Add(&LiveOrder{
			OrderID:       o.OrderID,
			ClientOrderID: o.ClientOrderID,
			Side:          o.Side,
			Price:         o.Price,
			Qty:           o.Quantity,
			RemainingQty:  o.Quantity - o.ExecutedQty,
			LevelIndex:    parseLevelFromTag(o.ClientOrderID),
			PlacedAt:      time.Now(),
		})
	}

	// Re-add recently placed orders not yet visible on exchange
	now := time.Now()
	reAdded := 0
	for i := range recentOrders {
		o := &recentOrders[i]
		if _, exists := exchangeIDs[o.OrderID]; !exists && now.Sub(o.PlacedAt) < recentGrace {
			b.orderTracker.Add(o)
			reAdded++
		}
	}

	if reAdded > 0 {
		log.Printf("[%s] Synced %d live orders from exchange (+%d pending)", b.strategy.Name(), len(orders), reAdded)
	} else {
		log.Printf("[%s] Synced %d live orders from exchange", b.strategy.Name(), len(orders))
	}

	// Reconcile Redis: push missing orders, remove stale ones
	if b.redis != nil && b.cfg.BotID != "" {
		b.reconcileRedisOrders(orders)
	}

	return nil
}

// reconcileRedisOrders ensures Redis matches the current live orders from exchange.
// Only performs reconciliation if the Redis count for this bot differs from live order count.
func (b *BaseBot) reconcileRedisOrders(liveOrders []*exchange.Order) {
	ctx := b.ctx

	// Get current Redis orders for this bot
	redisOrders, err := b.redis.GetAllOrders(ctx, b.cfg.Exchange, b.cfg.Symbol, b.cfg.BotID)
	if err != nil {
		log.Printf("[%s] Redis reconcile: failed to get orders: %v", b.strategy.Name(), err)
		return
	}

	// Count Redis orders belonging to this bot
	var botRedisCount int
	for _, o := range redisOrders {
		if o.BotID == b.cfg.BotID {
			botRedisCount++
		}
	}

	// Skip reconciliation if counts already match
	if botRedisCount == len(liveOrders) {
		return
	}

	log.Printf("[%s] Redis reconcile: live=%d redis=%d — reconciling", b.strategy.Name(), len(liveOrders), botRedisCount)

	// Build set of live orderIDs from exchange
	liveSet := make(map[string]struct{}, len(liveOrders))
	for _, o := range liveOrders {
		liveSet[o.OrderID] = struct{}{}
	}

	// Build set of Redis orderIDs for this bot, remove stale ones
	redisSet := make(map[string]struct{}, len(redisOrders))
	for _, o := range redisOrders {
		if o.BotID != b.cfg.BotID {
			continue
		}
		redisSet[o.OrderID] = struct{}{}
		// Remove from Redis if no longer on exchange
		if _, exists := liveSet[o.OrderID]; !exists {
			b.redis.DeleteOrder(ctx, b.cfg.Exchange, b.cfg.Symbol, o.OrderID)
		}
	}

	// Push to Redis if on exchange but missing from Redis
	for _, o := range liveOrders {
		if _, exists := redisSet[o.OrderID]; !exists {
			b.redis.SaveOrder(ctx, &store.OrderInfo{
				OrderID:       o.OrderID,
				ClientOrderID: o.ClientOrderID,
				Exchange:      b.cfg.Exchange,
				Symbol:        b.cfg.Symbol,
				Side:          o.Side,
				Price:         o.Price,
				Quantity:      o.Quantity,
				CreatedAt:     time.Now().UnixMilli(),
				Status:        "NEW",
				BotID:         b.cfg.BotID,
			})
		}
	}
}

// getSnapshot fetches current market snapshot
func (b *BaseBot) getSnapshot() (*Snapshot, error) {
	// Get depth
	depth, err := b.exch.GetDepth(b.ctx, b.cfg.Symbol)
	if err != nil {
		return nil, fmt.Errorf("get depth: %w", err)
	}
	if len(depth.Bids) == 0 || len(depth.Asks) == 0 {
		return nil, fmt.Errorf("empty order book")
	}

	return b.marketData.BuildSnapshot(depth)
}

// getBalanceState returns current balance state
func (b *BaseBot) getBalanceState() (*BalanceState, error) {
	// Prefer WebSocket cached balance (has accurate locked values for Bybit)
	if bal := b.balanceTracker.Get(); bal != nil {
		return bal, nil
	}

	// Fallback to REST API if no cache
	acct, err := b.exch.GetAccount(b.ctx)
	if err != nil {
		return nil, err
	}

	return b.balanceTracker.UpdateFromAccount(acct), nil
}

// cancelAllOrders cancels all orders
func (b *BaseBot) cancelAllOrders(reason string) error {
	log.Printf("[%s] Cancelling all orders: %s", b.strategy.Name(), reason)

	if err := b.exch.CancelAllOrders(b.ctx, b.cfg.Symbol); err != nil {
		return err
	}

	b.orderTracker.Clear()

	// Clear from Redis
	if b.redis != nil && b.cfg.BotID != "" {
		b.redis.ClearOrdersByBotID(b.ctx, b.cfg.Exchange, b.cfg.Symbol, b.cfg.BotID)
	}

	return nil
}

// replaceOrders cancels all orders and places new ones
func (b *BaseBot) replaceOrders(desired []DesiredOrder, reason string) error {
	// Prevent race condition - skip if already replacing
	b.replaceMu.Lock()
	if b.replacing {
		elapsed := time.Since(b.replaceStart)
		b.replaceMu.Unlock()
		log.Printf("[%s] Skip REPLACE: already in progress (%v ago)", b.strategy.Name(), elapsed)
		return nil
	}
	// Also skip if last replace was < 2 seconds ago (cooldown)
	if time.Since(b.replaceStart) < 2*time.Second {
		b.replaceMu.Unlock()
		log.Printf("[%s] Skip REPLACE: cooldown active", b.strategy.Name())
		return nil
	}
	b.replacing = true
	b.replaceStart = time.Now()
	b.replaceMu.Unlock()

	defer func() {
		b.replaceMu.Lock()
		b.replacing = false
		b.replaceMu.Unlock()
	}()

	log.Printf("[%s] Replacing orders (%d new): %s", b.strategy.Name(), len(desired), reason)

	// Cancel all existing orders
	if err := b.exch.CancelAllOrders(b.ctx, b.cfg.Symbol); err != nil {
		return fmt.Errorf("cancel failed: %w", err)
	}

	b.orderTracker.Clear()

	// Clear from Redis
	if b.redis != nil && b.cfg.BotID != "" {
		b.redis.ClearOrdersByBotID(b.ctx, b.cfg.Exchange, b.cfg.Symbol, b.cfg.BotID)
	}

	if len(desired) == 0 {
		return nil
	}

	// Build order requests and track requested values
	// (CCXT may not return price/qty in response for some exchanges like Bybit)
	type reqInfo struct {
		price float64
		qty   float64
		side  string
	}
	reqs := make([]*exchange.OrderRequest, len(desired))
	reqInfos := make(map[string]reqInfo) // clientOrderID -> requested info
	for i, d := range desired {
		reqs[i] = &exchange.OrderRequest{
			Symbol:        b.cfg.Symbol,
			Side:          d.Side,
			Type:          "LIMIT",
			Price:         d.Price,
			Quantity:      d.Qty,
			ClientOrderID: d.Tag,
		}
		reqInfos[d.Tag] = reqInfo{price: d.Price, qty: d.Qty, side: d.Side}
	}

	// Place orders in batches of 5 to avoid rate limits
	batchSize := b.cfg.BatchSize
	if batchSize <= 0 || batchSize > 20 {
		batchSize = 5 // Default batch size
	}

	for i := 0; i < len(reqs); i += batchSize {
		end := i + batchSize
		if end > len(reqs) {
			end = len(reqs)
		}
		batch := reqs[i:end]

		// Add delay between batches (except first one)
		if i > 0 {
			time.Sleep(1 * time.Second)
		}

		// Place batch
		resp, err := b.exch.BatchPlaceOrders(b.ctx, batch)
		if err != nil {
			// Fallback to individual orders with longer delay for rate limit
			log.Printf("[%s] Batch %d failed, falling back to individual: %v", b.strategy.Name(), i/batchSize, err)
			time.Sleep(5 * time.Second) // Wait for rate limit to reset
			for j, req := range batch {
				if j > 0 {
					time.Sleep(1 * time.Second)
				}
				order, err := b.exch.PlaceOrder(b.ctx, req)
				if err != nil {
					log.Printf("[%s] Place order failed: %v", b.strategy.Name(), err)
					continue
				}
				info := reqInfos[req.ClientOrderID]
				b.logOrderPlaced(order, req.ClientOrderID, info.price, info.qty, info.side)
			}
			continue
		}

		// Log results — Bybit batch API may not echo ClientOrderID, fallback to price+side lookup
		priceSideTag := make(map[string]string, len(batch))
		for _, req := range batch {
			key := fmt.Sprintf("%.8f_%s", req.Price, req.Side)
			priceSideTag[key] = req.ClientOrderID
		}
		for _, order := range resp.Orders {
			tag := order.ClientOrderID
			if tag == "" {
				key := fmt.Sprintf("%.8f_%s", order.Price, order.Side)
				tag = priceSideTag[key]
			}
			info := reqInfos[tag]
			b.logOrderPlaced(order, tag, info.price, info.qty, info.side)
		}
		for _, errMsg := range resp.Errors {
			log.Printf("[%s] Order error: %s", b.strategy.Name(), errMsg)
		}
	}

	return nil
}

// amendOrders performs incremental update: cancel specific orders + add new orders
func (b *BaseBot) amendOrders(toCancel []string, toAdd []DesiredOrder, reason string) error {
	log.Printf("[%s] Amending orders (cancel=%d, add=%d): %s",
		b.strategy.Name(), len(toCancel), len(toAdd), reason)

	// Cancel specific orders
	for _, orderID := range toCancel {
		if err := b.exch.CancelOrder(b.ctx, b.cfg.Symbol, orderID); err != nil {
			log.Printf("[%s] Cancel order %s failed: %v", b.strategy.Name(), orderID, err)
			// Continue with other cancels
		} else {
			b.orderTracker.Remove(orderID)
			log.Printf("[%s] Cancelled order %s", b.strategy.Name(), orderID)
		}
	}

	// Add new orders
	if len(toAdd) == 0 {
		return nil
	}

	// Build order requests and track requested values
	// (CCXT may not return price/qty in response for some exchanges like Bybit)
	type reqInfo struct {
		price float64
		qty   float64
		side  string
	}
	reqs := make([]*exchange.OrderRequest, len(toAdd))
	reqInfos := make(map[string]reqInfo) // clientOrderID -> requested info
	for i, d := range toAdd {
		reqs[i] = &exchange.OrderRequest{
			Symbol:        b.cfg.Symbol,
			Side:          d.Side,
			Type:          "LIMIT",
			Price:         d.Price,
			Quantity:      d.Qty,
			ClientOrderID: d.Tag,
		}
		reqInfos[d.Tag] = reqInfo{price: d.Price, qty: d.Qty, side: d.Side}
	}

	// Place orders in batches of 5 to avoid rate limits
	batchSize := b.cfg.BatchSize
	if batchSize <= 0 || batchSize > 20 {
		batchSize = 5
	}

	for i := 0; i < len(reqs); i += batchSize {
		end := i + batchSize
		if end > len(reqs) {
			end = len(reqs)
		}
		batch := reqs[i:end]

		// Add delay between batches (except first one)
		if i > 0 {
			time.Sleep(1 * time.Second)
		}

		// Place batch
		resp, err := b.exch.BatchPlaceOrders(b.ctx, batch)
		if err != nil {
			// Fallback to individual orders with longer delay for rate limit
			log.Printf("[%s] Batch add %d failed, falling back to individual: %v", b.strategy.Name(), i/batchSize, err)
			time.Sleep(5 * time.Second) // Wait for rate limit to reset
			for j, req := range batch {
				if j > 0 {
					time.Sleep(1 * time.Second)
				}
				order, err := b.exch.PlaceOrder(b.ctx, req)
				if err != nil {
					log.Printf("[%s] Place order failed: %v", b.strategy.Name(), err)
					continue
				}
				info := reqInfos[req.ClientOrderID]
				b.logOrderPlaced(order, req.ClientOrderID, info.price, info.qty, info.side)
			}
			continue
		}

		// Log results — Bybit batch API may not echo ClientOrderID, fallback to price+side lookup
		priceSideTag := make(map[string]string, len(batch))
		for _, req := range batch {
			key := fmt.Sprintf("%.8f_%s", req.Price, req.Side)
			priceSideTag[key] = req.ClientOrderID
		}
		for _, order := range resp.Orders {
			tag := order.ClientOrderID
			if tag == "" {
				key := fmt.Sprintf("%.8f_%s", order.Price, order.Side)
				tag = priceSideTag[key]
			}
			info := reqInfos[tag]
			b.logOrderPlaced(order, tag, info.price, info.qty, info.side)
		}
		for _, errMsg := range resp.Errors {
			log.Printf("[%s] Order error: %s", b.strategy.Name(), errMsg)
		}
	}

	return nil
}

func (b *BaseBot) logOrderPlaced(order *exchange.Order, tag string, requestedPrice, requestedQty float64, requestedSide string) {
	level := parseLevelFromTag(tag)

	// Use requested values for logging (CCXT may not return these in response for some exchanges)
	logPrice := requestedPrice
	logQty := requestedQty
	logSide := requestedSide
	if logPrice == 0 {
		logPrice = order.Price
	}
	if logQty == 0 {
		logQty = order.Quantity
	}
	if logSide == "" {
		logSide = order.Side
	}

	log.Printf("[%s] Placed %s L%d @ %.8f x %.6f (id=%s)",
		b.strategy.Name(), logSide, level, logPrice, logQty, order.OrderID)

	// Add to orderTracker immediately (don't wait for WebSocket NEW event)
	b.orderTracker.Add(&LiveOrder{
		OrderID:       order.OrderID,
		ClientOrderID: tag,
		Side:          logSide,
		Price:         logPrice,
		Qty:           logQty,
		RemainingQty:  logQty,
		LevelIndex:    level,
		PlacedAt:      time.Now(),
	})

	// Note: Events and Redis handled by handleOrderUpdate when WebSocket NEW arrives
}

// checkConfigUpdate checks for config updates from MongoDB
func (b *BaseBot) checkConfigUpdate() {
	if b.mongo == nil || b.cfg.BotID == "" {
		return
	}

	update, err := b.mongo.CheckConfigUpdate(b.ctx, b.cfg.BotID)
	if err != nil {
		log.Printf("[%s] Config check failed: %v", b.strategy.Name(), err)
		return
	}

	if !update.IsUpdated || update.SimpleConfig == nil {
		return
	}

	log.Printf("[%s] Config updated, applying new settings...", b.strategy.Name())

	// Forward to strategy
	if err := b.strategy.UpdateConfig(update.SimpleConfig); err != nil {
		log.Printf("[%s] Config update failed: %v", b.strategy.Name(), err)
	} else {
		log.Printf("[%s] Config applied successfully", b.strategy.Name())
	}
}

// GetMode returns current operating mode
func (b *BaseBot) GetMode() Mode {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.mode
}

// GetStrategy returns the underlying strategy (for type assertion if needed)
func (b *BaseBot) GetStrategy() Strategy {
	return b.strategy
}

// ForceMode allows manual mode override (delegates to strategy if supported)
func (b *BaseBot) ForceMode(mode Mode, reason string) {
	if fm, ok := b.strategy.(interface{ ForceMode(Mode, string) }); ok {
		fm.ForceMode(mode, reason)
	}
	b.mu.Lock()
	b.mode = mode
	b.mu.Unlock()
}

// parseLevelFromTag extracts level index from tag like "MM_123_L2_BID"
func parseLevelFromTag(tag string) int {
	for i := 0; i < len(tag)-1; i++ {
		if tag[i] == 'L' && tag[i+1] >= '0' && tag[i+1] <= '9' {
			level := int(tag[i+1] - '0')
			if i+2 < len(tag) && tag[i+2] >= '0' && tag[i+2] <= '9' {
				level = level*10 + int(tag[i+2]-'0')
			}
			return level
		}
	}
	return 0
}
