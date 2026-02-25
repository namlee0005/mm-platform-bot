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

	// Tick tracking
	tickCount           int
	configCheckInterval int
	lastTickTime        int64

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
		cfg.BatchSize = 20
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
	}

	// Initialize components
	bot.balanceTracker = NewBalanceTracker(cfg.BaseAsset, cfg.QuoteAsset)
	bot.orderTracker = NewOrderTracker()
	bot.marketData = NewMarketDataCache()

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

	// 4. Clear orders from Redis
	if b.redis != nil && b.cfg.BotID != "" {
		removed, err := b.redis.ClearOrdersByBotID(shutdownCtx, b.cfg.Symbol, b.cfg.BotID)
		if err != nil {
			log.Printf("[%s] WARNING: Failed to clear orders from Redis: %v", b.strategy.Name(), err)
		} else {
			log.Printf("[%s] Cleared %d orders from Redis", b.strategy.Name(), removed)
		}
	}

	// 5. Clear balances from Redis
	if b.redis != nil && b.cfg.BotID != "" && b.cfg.Exchange != "" {
		if err := b.redis.ClearMMBalances(shutdownCtx, b.cfg.Exchange, b.cfg.Symbol, b.cfg.BotID); err != nil {
			log.Printf("[%s] WARNING: Failed to clear balances from Redis: %v", b.strategy.Name(), err)
		} else {
			log.Printf("[%s] Cleared balances from Redis", b.strategy.Name())
		}
	}

	// 6. Stop exchange connection
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

	// 1. Get current market snapshot
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

	// 3. Get live orders
	liveOrders := b.orderTracker.GetAll()

	// 4. Build tick input
	input := &TickInput{
		Snapshot:   snap,
		Balance:    balance,
		LiveOrders: liveOrders,
		Timestamp:  now,
		Mode:       b.mode,
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

	// 7. Execute the action
	switch output.Action {
	case TickActionCancelAll:
		return b.cancelAllOrders(output.Reason)
	case TickActionKeep:
		// Do nothing
		log.Printf("[%s] Keep orders: %s", b.strategy.Name(), output.Reason)
	case TickActionReplace:
		return b.replaceOrders(output.DesiredOrders, output.Reason)
	}

	// Log tick summary
	invRatio, invDev := balance.ComputeInventory(snap.Mid, 0.5) // TODO: get target ratio from strategy
	log.Printf("[%s] mid=%.6f, inv=%.2f%% (dev=%.2f%%), orders=%d, action=%s",
		b.strategy.Name(), snap.Mid, invRatio*100, invDev*100, len(liveOrders), output.Action)

	return nil
}

// subscribeUserStream sets up WebSocket event handlers
func (b *BaseBot) subscribeUserStream() error {
	handlers := exchange.UserStreamHandlers{
		OnAccountUpdate: b.handleAccountUpdate,
		OnOrderUpdate:   b.handleOrderUpdate,
		OnFill:          b.handleFill,
		OnError: func(err error) {
			log.Printf("[%s] WebSocket error: %v", b.strategy.Name(), err)
		},
	}

	return b.exch.SubscribeUserStream(b.ctx, handlers)
}

func (b *BaseBot) handleAccountUpdate(event *types.AccountEvent) {
	// Update balance tracker
	b.balanceTracker.UpdateFromEvent(event)

	// Publish to Redis
	if b.redis != nil && b.cfg.Exchange != "" && b.cfg.BotID != "" {
		var allBalances []store.AssetBalance
		for _, bal := range event.Balances {
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

func (b *BaseBot) handleOrderUpdate(event *types.OrderEvent) {
	// Update order tracker
	switch event.Status {
	case "NEW":
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
	case "PARTIALLY_FILLED":
		b.orderTracker.UpdateRemaining(event.OrderID, event.Quantity-event.ExecutedQty)
	case "FILLED", "CANCELED", "EXPIRED", "REJECTED":
		b.orderTracker.Remove(event.OrderID)
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

	// Delete from Redis on cancel/fill
	if event.Status == "CANCELED" || event.Status == "FILLED" {
		if b.redis != nil {
			b.redis.DeleteOrder(b.ctx, b.cfg.Symbol, event.OrderID)
		}
	}

	// Publish to Redis
	if b.redis != nil {
		b.redis.PublishOrderUpdate(b.ctx, event)
	}
}

func (b *BaseBot) handleFill(event *types.FillEvent) {
	log.Printf("[FILL] %s %s @ %.8f x %.6f (order=%s)",
		event.Side, event.Symbol, event.Price, event.Quantity, event.OrderID)

	// Forward to strategy
	b.strategy.OnFill(&FillEvent{
		OrderID:         event.OrderID,
		ClientOrderID:   "", // Not available in types.FillEvent
		Symbol:          event.Symbol,
		Side:            event.Side,
		Price:           event.Price,
		Quantity:        event.Quantity,
		Commission:      event.Commission,
		CommissionAsset: event.CommissionAsset,
		TradeID:         event.TradeID,
		Timestamp:       event.Timestamp,
	})

	// Emit fill event callback
	if b.onOrderEvent != nil {
		b.onOrderEvent(BotOrderEvent{
			Type:      OrderEventTypeFill,
			Symbol:    event.Symbol,
			OrderID:   event.OrderID,
			Side:      event.Side,
			Price:     event.Price,
			Qty:       event.Quantity,
			Level:     0,
			Reason:    "filled",
			Timestamp: event.Timestamp.UnixMilli(),
		})
	}

	// Publish to Redis + MongoDB
	if b.redis != nil {
		b.redis.PublishFill(b.ctx, event)
	}
	if b.mongo != nil {
		b.mongo.SaveFill(b.ctx, event)
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

// syncLiveOrders fetches current open orders from exchange
func (b *BaseBot) syncLiveOrders() error {
	orders, err := b.exch.GetOpenOrders(b.ctx, b.cfg.Symbol)
	if err != nil {
		return err
	}

	b.orderTracker.Clear()
	for _, o := range orders {
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

	log.Printf("[%s] Synced %d live orders from exchange", b.strategy.Name(), len(orders))
	return nil
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
	// Try cached balance first
	if bal := b.balanceTracker.Get(); bal != nil {
		return bal, nil
	}

	// Fetch from REST API
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
		b.redis.ClearOrdersByBotID(b.ctx, b.cfg.Symbol, b.cfg.BotID)
	}

	return nil
}

// replaceOrders cancels all orders and places new ones
func (b *BaseBot) replaceOrders(desired []DesiredOrder, reason string) error {
	log.Printf("[%s] Replacing orders (%d new): %s", b.strategy.Name(), len(desired), reason)

	// Cancel all existing orders
	if err := b.exch.CancelAllOrders(b.ctx, b.cfg.Symbol); err != nil {
		return fmt.Errorf("cancel failed: %w", err)
	}

	b.orderTracker.Clear()

	// Clear from Redis
	if b.redis != nil && b.cfg.BotID != "" {
		b.redis.ClearOrdersByBotID(b.ctx, b.cfg.Symbol, b.cfg.BotID)
	}

	if len(desired) == 0 {
		return nil
	}

	// Build order requests
	reqs := make([]*exchange.OrderRequest, len(desired))
	for i, d := range desired {
		reqs[i] = &exchange.OrderRequest{
			Symbol:        b.cfg.Symbol,
			Side:          d.Side,
			Type:          "LIMIT",
			Price:         d.Price,
			Quantity:      d.Qty,
			ClientOrderID: d.Tag,
		}
	}

	// Place orders via batch API
	resp, err := b.exch.BatchPlaceOrders(b.ctx, reqs)
	if err != nil {
		// Fallback to individual orders
		log.Printf("[%s] Batch failed, falling back to individual orders: %v", b.strategy.Name(), err)
		for _, req := range reqs {
			order, err := b.exch.PlaceOrder(b.ctx, req)
			if err != nil {
				log.Printf("[%s] Place order failed: %v", b.strategy.Name(), err)
				continue
			}
			b.logOrderPlaced(order, req.ClientOrderID)
		}
		return nil
	}

	// Log results
	for _, order := range resp.Orders {
		b.logOrderPlaced(order, order.ClientOrderID)
	}
	for _, errMsg := range resp.Errors {
		log.Printf("[%s] Order error: %s", b.strategy.Name(), errMsg)
	}

	return nil
}

func (b *BaseBot) logOrderPlaced(order *exchange.Order, tag string) {
	level := parseLevelFromTag(tag)
	log.Printf("[%s] Placed %s L%d @ %.8f x %.6f (id=%s)",
		b.strategy.Name(), order.Side, level, order.Price, order.Quantity, order.OrderID)

	// Emit event
	if b.onOrderEvent != nil {
		b.onOrderEvent(BotOrderEvent{
			Type:      OrderEventTypePlace,
			Symbol:    b.cfg.Symbol,
			OrderID:   order.OrderID,
			Side:      order.Side,
			Price:     order.Price,
			Qty:       order.Quantity,
			Level:     level,
			Reason:    "new_level",
			Timestamp: time.Now().UnixMilli(),
		})
	}

	// Save to Redis
	if b.redis != nil {
		b.redis.SaveOrder(b.ctx, &store.OrderInfo{
			OrderID:       order.OrderID,
			ClientOrderID: tag,
			Symbol:        b.cfg.Symbol,
			Side:          order.Side,
			Price:         order.Price,
			Quantity:      order.Quantity,
			CreatedAt:     time.Now().UnixMilli(),
			Status:        "NEW",
			BotID:         b.cfg.BotID,
		})
	}
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
