package engine

import (
	"context"
	"fmt"
	"log"
	"math"
	"strings"
	"sync/atomic"
	"time"

	"mm-platform-engine/internal/core"
	"mm-platform-engine/internal/exchange"
	"mm-platform-engine/internal/store"
	"mm-platform-engine/internal/types"
)

// Engine is the event-driven core of the market-making bot.
// A single goroutine runs the select loop — all state mutations
// happen inside that goroutine, eliminating the need for mutexes.
type Engine struct {
	cfg      *EngineConfig
	strategy core.Strategy
	exch     exchange.Exchange
	executor *Executor
	bus      *EventBus
	wsFeed   *WSFeedManager
	state    *EngineState

	// Persistence
	redis *store.RedisStore
	mongo *store.MongoStore

	// Notifications
	fillNotifier core.FillNotifier

	// Coalescing state (owned by engine goroutine)
	recalcNeeded   bool
	firstEventTime time.Time

	// Fallback guard — prevent multiple restFallbackLoop goroutines
	fallbackRunning atomic.Bool
	fallbackCancel  context.CancelFunc

	// Callbacks
	onOrderEvent core.OrderEventCallback
}

// NewEngine creates a new event-driven Engine.
func NewEngine(
	cfg *EngineConfig,
	strategy core.Strategy,
	exch exchange.Exchange,
	executor *Executor,
	redis *store.RedisStore,
	mongo *store.MongoStore,
) *Engine {
	cfg.ApplyDefaults()

	bus := NewEventBus(cfg.BusCapacity)
	state := NewEngineState(cfg.BaseAsset, cfg.QuoteAsset, cfg.UseLastTradePrice)
	wsFeed := NewWSFeedManager(exch, bus, cfg.Symbol, cfg.BotID)

	eng := &Engine{
		cfg:      cfg,
		strategy: strategy,
		exch:     exch,
		executor: executor,
		bus:      bus,
		wsFeed:   wsFeed,
		state:    state,
		redis:    redis,
		mongo:    mongo,
	}

	// Wire executor callback: immediately add placed orders to state tracker
	// so backstop doesn't see missing orders and create duplicates.
	executor.SetOnOrderPlaced(func(order *exchange.Order, levelIndex int) {
		state.orderTracker.Add(&core.LiveOrder{
			OrderID:      order.OrderID,
			Side:         order.Side,
			Price:        order.Price,
			Qty:          order.Quantity,
			RemainingQty: order.Quantity,
			LevelIndex:   levelIndex,
			PlacedAt:     time.Now(),
		})
	})

	// Wire executor callback: remove ghost orders when cancel returns "already gone".
	// Without this, orders from a previous session (or already canceled by CancelAll REST)
	// stay in the tracker forever because no WS CANCELED event will arrive for them.
	executor.SetOnOrderGone(func(orderID string) {
		state.orderTracker.Remove(orderID)
	})

	return eng
}

// SetFillNotifier sets the Telegram fill notifier.
func (e *Engine) SetFillNotifier(n core.FillNotifier) {
	e.fillNotifier = n
}

// SetTimerOnlyMode enables timer-only mode: Tick() runs at the given interval (ms)
// instead of being triggered by WS events. Events are still applied for state tracking.
func (e *Engine) SetTimerOnlyMode(intervalMs int) {
	e.cfg.TimerOnlyMode = true
	e.cfg.BackstopIntervalMs = intervalMs
}

// SetOrderEventCallback sets the callback for order events (Redis broadcast).
func (e *Engine) SetOrderEventCallback(cb core.OrderEventCallback) {
	e.onOrderEvent = cb
	e.executor.SetOrderEventCallback(cb)
}

// Start initializes the engine: loads market info, syncs initial state,
// starts WS feeds, and enters the reactor loop.
func (e *Engine) Start(ctx context.Context) error {
	log.Printf("[ENGINE] Starting %s on %s/%s", e.strategy.Name(), e.cfg.Exchange, e.cfg.Symbol)

	// 1. Load market info (tick size, step size, etc.)
	if err := e.loadMarketInfo(ctx); err != nil {
		return fmt.Errorf("load market info: %w", err)
	}

	// 2. Initial REST sync (balance + orders + depth)
	if err := e.initialSync(ctx); err != nil {
		return fmt.Errorf("initial sync: %w", err)
	}

	// 3. Initialize strategy
	snap := e.state.BuildSnapshot()
	balance := e.state.GetBalance()
	if balance == nil {
		balance = &core.BalanceState{}
	}
	if err := e.strategy.Init(ctx, snap, balance); err != nil {
		return fmt.Errorf("strategy init: %w", err)
	}

	// 4. Start WebSocket feeds
	if err := e.wsFeed.Start(ctx); err != nil {
		log.Printf("[ENGINE] WS feed start failed: %v — using REST fallback", err)
		e.state.wsConnected = false
	} else {
		e.state.wsConnected = true
	}

	// 5. Run reactor loop
	go e.run(ctx)

	// 6. Start order history sync job (fetch filled orders via API → save to MongoDB)
	go e.orderHistorySyncLoop(ctx)

	log.Printf("[ENGINE] Started %s", e.strategy.Name())
	return nil
}

// Stop gracefully shuts down the engine: cancels all orders, stops WS.
func (e *Engine) Stop(ctx context.Context) error {
	log.Printf("[ENGINE] Stopping %s", e.strategy.Name())

	// Cancel all orders
	if err := e.executor.CancelAll(ctx, "engine_stop"); err != nil {
		log.Printf("[ENGINE] CancelAll on stop failed: %v", err)
	}

	// Stop WS feeds
	e.wsFeed.Stop()

	// Clear Redis state
	if e.redis != nil && e.cfg.BotID != "" {
		if err := e.redis.ClearOrdersByBot(ctx, e.cfg.Exchange, e.cfg.Symbol, e.cfg.BotID); err != nil {
			log.Printf("[ENGINE] Clear Redis orders failed: %v", err)
		}
		if err := e.redis.ClearMMBalances(ctx, e.cfg.Exchange, e.cfg.Symbol, e.cfg.BotID); err != nil {
			log.Printf("[ENGINE] Clear Redis balances failed: %v", err)
		}
	}

	// Stop exchange
	if err := e.exch.Stop(ctx); err != nil {
		log.Printf("[ENGINE] Exchange stop failed: %v", err)
	}

	log.Printf("[ENGINE] Stopped %s", e.strategy.Name())
	return nil
}

// run is the reactor loop — the heart of the event-driven engine.
// It runs in a single goroutine, processing events from the EventBus.
func (e *Engine) run(ctx context.Context) {
	backstop := time.NewTicker(time.Duration(e.cfg.BackstopIntervalMs) * time.Millisecond)
	defer backstop.Stop()

	configCheck := time.NewTicker(time.Duration(e.cfg.ConfigCheckIntervalMs) * time.Millisecond)
	defer configCheck.Stop()

	// Periodic REST reconcile — sync inventory & orders with exchange
	// Catches WS drift, missed events, phantom orders
	reconcile := time.NewTicker(time.Duration(e.cfg.ReconcileIntervalMs) * time.Millisecond)
	defer reconcile.Stop()

	// Coalesce timer — starts stopped
	coalesceTimer := time.NewTimer(time.Hour)
	coalesceTimer.Stop()
	defer coalesceTimer.Stop()

	// First tick immediately
	log.Printf("[ENGINE] DEBUG: Running initial recalc")
	e.executeRecalc(ctx)

	for {
		select {
		case <-ctx.Done():
			log.Printf("[ENGINE] Reactor loop stopped: %v", ctx.Err())
			return

		case evt := <-e.bus.C():
			log.Printf("[ENGINE] DEBUG: Event received: %s (bus len=%d)", evt.Type, len(e.bus.ch))
			e.applyEvent(ctx, evt)

			if e.shouldTriggerRecalc(evt) {
				if !e.recalcNeeded {
					e.firstEventTime = time.Now()
					coalesceTimer.Reset(time.Duration(e.cfg.CoalesceWindowMs) * time.Millisecond)
					log.Printf("[ENGINE] DEBUG: Coalesce started (window=%dms)", e.cfg.CoalesceWindowMs)
				} else if time.Since(e.firstEventTime) > time.Duration(e.cfg.MaxCoalesceMs)*time.Millisecond {
					coalesceTimer.Reset(0)
					log.Printf("[ENGINE] DEBUG: Coalesce max exceeded (>%dms) — firing now", e.cfg.MaxCoalesceMs)
				}
				e.recalcNeeded = true
			}

		case <-coalesceTimer.C:
			if e.recalcNeeded {
				e.recalcNeeded = false
				e.firstEventTime = time.Time{}
				log.Printf("[ENGINE] DEBUG: Coalesce timer fired — executing recalc")
				e.executeRecalc(ctx)
			}

		case <-backstop.C:
			// Always refresh orderbook/ticker from REST since we don't have WS public streams.
			// Balance is refreshed by reconcile timer separately.
			e.refreshMarketData(ctx)
			e.executeRecalc(ctx)

		case <-reconcile.C:
			log.Printf("[ENGINE] DEBUG: Reconcile timer fired")
			e.reconcileFromREST(ctx)

		case <-configCheck.C:
			e.checkConfigUpdate(ctx)
		}
	}
}

// shouldTriggerRecalc decides if an event warrants a strategy recalculation.
func (e *Engine) shouldTriggerRecalc(evt Event) bool {
	// Timer-only mode: only config reload triggers recalc from events.
	// All other recalcs come from the backstop timer.
	if e.cfg.TimerOnlyMode {
		return evt.Type == EventConfigReload || evt.Type == EventForceRecalc
	}

	switch evt.Type {
	case EventOrderBookUpdate:
		data, ok := evt.Data.(*OrderBookUpdateData)
		if !ok {
			return false
		}
		newMid := (data.BestBid + data.BestAsk) / 2
		if newMid <= 0 {
			return false
		}
		if e.state.lastRecalcMid > 0 {
			changeBps := math.Abs(newMid-e.state.lastRecalcMid) / e.state.lastRecalcMid * 10000
			return changeBps > float64(e.cfg.RepriceThresholdBps)
		}
		return true // First update

	case EventOrderUpdate:
		data, ok := evt.Data.(*types.OrderEvent)
		if !ok {
			return false
		}
		// Recalc on any terminal state or fill (partial/full)
		switch data.Status {
		case "FILLED", "PARTIALLY_FILLED", "CANCELED", "EXPIRED":
			return true
		}
		return false

	case EventConfigReload:
		return true

	case EventWSReconnect:
		return true

	case EventTimerTick:
		return true

	case EventForceRecalc:
		return true

	default:
		return false
	}
}

// applyEvent mutates EngineState based on the event.
// This is lightweight bookkeeping — not a full strategy recalc.
func (e *Engine) applyEvent(ctx context.Context, evt Event) {
	switch evt.Type {
	case EventOrderBookUpdate:
		if data, ok := evt.Data.(*OrderBookUpdateData); ok {
			log.Printf("[ENGINE] DEBUG: OrderBook update bid=%.8f ask=%.8f (levels: %d/%d)",
				data.BestBid, data.BestAsk, len(data.Bids), len(data.Asks))
			e.state.ApplyOrderBookUpdate(data)
		}

	case EventPublicTrade:
		if data, ok := evt.Data.(*PublicTradeData); ok {
			log.Printf("[ENGINE] DEBUG: Public trade %s %.8f x %.6f", data.Side, data.Price, data.Quantity)
			e.state.ApplyPublicTrade(data)
		}

	case EventOrderUpdate:
		if data, ok := evt.Data.(*types.OrderEvent); ok {
			log.Printf("[ENGINE] DEBUG: OrderUpdate order=%s status=%s side=%s price=%.8f qty=%.6f execQty=%.6f",
				data.OrderID, data.Status, data.Side, data.Price, data.Quantity, data.ExecutedQty)

			// ApplyOrderUpdate returns fill delta (computed from cumulative ExecutedQty)
			fillDelta, isFull := e.state.ApplyOrderUpdate(data)
			if fillDelta > 0 {
				log.Printf("[ENGINE] DEBUG: Fill detected — delta=%.6f isFull=%v (remaining orders=%d)",
					fillDelta, isFull, e.state.OrderCount())
			}

			// Forward order update to strategy
			e.strategy.OnOrderUpdate(&core.OrderEvent{
				OrderID:       data.OrderID,
				ClientOrderID: data.ClientOrderID,
				Symbol:        data.Symbol,
				Side:          data.Side,
				Status:        data.Status,
				Price:         data.Price,
				Quantity:      data.Quantity,
				ExecutedQty:   data.ExecutedQty,
				Timestamp:     data.Timestamp,
			})

			// Handle fill if delta > 0 (partial or full)
			if fillDelta > 0 {
				e.strategy.OnFill(&core.FillEvent{
					OrderID:       data.OrderID,
					ClientOrderID: data.ClientOrderID,
					Symbol:        data.Symbol,
					Side:          data.Side,
					Price:         data.Price,
					Quantity:      fillDelta,
					Timestamp:     data.Timestamp,
				})

				// Persist fill to MongoDB
				if e.mongo != nil {
					if err := e.mongo.SaveFill(ctx, &types.FillEvent{
						OrderID:       data.OrderID,
						ClientOrderID: data.ClientOrderID,
						Symbol:        data.Symbol,
						Side:          data.Side,
						Price:         data.Price,
						Quantity:      fillDelta,
						Timestamp:     data.Timestamp,
						BotID:         e.cfg.BotID,
						Exchange:      e.cfg.Exchange,
					}); err != nil {
						log.Printf("[ENGINE] SaveFill failed: %v", err)
					}
				}

				// Telegram notification
				if e.fillNotifier != nil {
					notional := data.Price * fillDelta
					e.fillNotifier.NotifyFill(data.Side, data.Price, fillDelta, notional, isFull, data.OrderID)
				}

				// Redis stream — fill event
				eventType := core.OrderEventTypeFill
				reason := "fill"
				if !isFull {
					eventType = core.OrderEventTypePartialFill
					reason = "partial_fill"
				}
				e.emitMMOrderEvent(eventType, data.OrderID, data.Side, data.Price, fillDelta, 0, reason)

				fillType := "FILL"
				if !isFull {
					fillType = "PARTIAL"
				}
				log.Printf("[%s] %s %s @ %.8f x %.6f (order=%s)",
					fillType, data.Side, data.Symbol, data.Price, fillDelta, data.OrderID)
			}

			// Redis stream — non-fill order events
			switch data.Status {
			case "NEW":
				e.emitMMOrderEvent(core.OrderEventTypePlace, data.OrderID, data.Side, data.Price, data.Quantity, 0, "new")
			case "CANCELED":
				e.emitMMOrderEvent(core.OrderEventTypeCancel, data.OrderID, data.Side, data.Price, data.Quantity, 0, "canceled")
			}
		}

	case EventBalanceUpdate:
		if data, ok := evt.Data.(*types.AccountEvent); ok {
			log.Printf("[ENGINE] DEBUG: Balance update — %d assets changed", len(data.Balances))
			e.state.ApplyBalanceUpdate(data)
			bal := e.state.GetBalance()
			if bal != nil {
				log.Printf("[ENGINE] DEBUG: Balance state — baseFree=%.6f baseLocked=%.6f quoteFree=%.4f quoteLocked=%.4f",
					bal.BaseFree, bal.BaseLocked, bal.QuoteFree, bal.QuoteLocked)
			}
			go e.publishBalancesToRedis(ctx)
		}

	case EventWSDisconnect:
		e.state.wsConnected = false
		log.Printf("[ENGINE] DEBUG: WS disconnected — fallbackRunning=%v", e.fallbackRunning.Load())
		if !e.fallbackRunning.Load() {
			e.fallbackRunning.Store(true)
			fbCtx, fbCancel := context.WithCancel(ctx)
			e.fallbackCancel = fbCancel
			log.Printf("[ENGINE] WS disconnected — starting REST fallback")
			go e.restFallbackLoop(fbCtx)
		} else {
			log.Printf("[ENGINE] DEBUG: Fallback already running — skipping spawn")
		}

	case EventWSReconnect:
		e.state.wsConnected = true
		log.Printf("[ENGINE] DEBUG: WS reconnected — fallbackRunning=%v", e.fallbackRunning.Load())
		if e.fallbackRunning.Load() && e.fallbackCancel != nil {
			e.fallbackCancel()
			e.fallbackRunning.Store(false)
			log.Printf("[ENGINE] DEBUG: Fallback loop cancelled")
		}
		log.Printf("[ENGINE] WS reconnected — full resync")
		e.restFallbackRefresh(ctx)

	case EventConfigReload:
		log.Printf("[ENGINE] DEBUG: Config reload event received")
		if data, ok := evt.Data.(*ConfigReloadData); ok {
			if err := e.strategy.UpdateConfig(data.NewCfg); err != nil {
				log.Printf("[ENGINE] Config update failed: %v", err)
			} else {
				log.Printf("[ENGINE] DEBUG: Config updated successfully")
			}
		}
	}
}

// executeRecalc calls strategy.Tick() with current state and executes the result.
func (e *Engine) executeRecalc(ctx context.Context) {
	snap := e.state.BuildSnapshot()
	balance := e.state.GetBalance()
	liveOrders := e.state.GetLiveOrders()

	// Validate
	if snap.Mid <= 0 {
		log.Printf("[ENGINE] DEBUG: Skipping recalc — no valid mid price")
		return
	}
	if balance == nil {
		log.Printf("[ENGINE] DEBUG: No balance cached — using empty")
		balance = &core.BalanceState{}
	}

	log.Printf("[ENGINE] DEBUG: Recalc input — mid=%.8f bid=%.8f ask=%.8f orders=%d trades=%d mode=%s",
		snap.Mid, snap.BestBid, snap.BestAsk, len(liveOrders), len(e.state.recentTrades), e.state.mode)

	// Build tick input
	input := &core.TickInput{
		Snapshot:     snap,
		Balance:      balance,
		LiveOrders:   liveOrders,
		Timestamp:    time.Now().UnixMilli(),
		Mode:         e.state.mode,
		RecentTrades: e.state.GetRecentTrades(),
	}

	// Call strategy
	start := time.Now()
	output, err := e.strategy.Tick(ctx, input)
	tickDuration := time.Since(start)
	if err != nil {
		log.Printf("[ENGINE] Strategy tick failed: %v", err)
		return
	}

	log.Printf("[ENGINE] DEBUG: Strategy.Tick() took %v — action=%s reason=%s desired=%d",
		tickDuration, output.Action, output.Reason, len(output.DesiredOrders))

	// Log orderbook + desired orders for debugging
	e.logRecalcSummary(snap, liveOrders, output)

	// Update mode
	if output.NewMode != "" {
		log.Printf("[ENGINE] DEBUG: Mode changed: %s → %s", e.state.mode, output.NewMode)
		e.state.mode = output.NewMode
	}

	// Record last recalc mid
	e.state.lastRecalcMid = snap.Mid
	e.state.lastRecalcTime = time.Now()

	// Log summary
	invRatio, invDev := balance.ComputeInventory(snap.Mid, 0.5)
	log.Printf("[ENGINE] %s mid=%.8f inv=%.2f%% (dev=%.2f%%) orders=%d action=%s reason=%s",
		e.strategy.Name(), snap.Mid, invRatio*100, invDev*100,
		len(liveOrders), output.Action, output.Reason)

	// Execute
	e.executor.Execute(ctx, output, liveOrders)

	// Update strategy's prev snapshot
	e.strategy.UpdatePrevSnapshot(liveOrders, balance)

	// Only sync orders to Redis when orders actually changed
	if output.Action != core.TickActionKeep {
		ordersCopy := make([]core.LiveOrder, len(liveOrders))
		copy(ordersCopy, liveOrders)
		go e.syncOrdersToRedis(ctx, ordersCopy)
	}
}

// logRecalcSummary logs our live orders, desired orders, and mid price for each recalc tick.
func (e *Engine) logRecalcSummary(snap *core.Snapshot, liveOrders []core.LiveOrder, output *core.TickOutput) {
	var sb strings.Builder

	sb.WriteString("\n========== RECALC SUMMARY ==========\n")
	sb.WriteString(fmt.Sprintf("  MID: %.8f  spread: %.8f (%.2f bps)\n",
		snap.Mid, snap.BestAsk-snap.BestBid,
		(snap.BestAsk-snap.BestBid)/snap.Mid*10000))

	// Live orders (our orders on exchange)
	var bidNotional, askNotional float64
	var bidCount, askCount int
	var ourBestBid, ourBestAsk float64
	for _, o := range liveOrders {
		if o.Side == "BUY" {
			bidNotional += o.RemainingQty * o.Price
			bidCount++
			if o.Price > ourBestBid {
				ourBestBid = o.Price
			}
		} else {
			askNotional += o.RemainingQty * o.Price
			askCount++
			if ourBestAsk == 0 || o.Price < ourBestAsk {
				ourBestAsk = o.Price
			}
		}
	}
	ourSpread := ourBestAsk - ourBestBid
	ourSpreadBps := 0.0
	if snap.Mid > 0 {
		ourSpreadBps = ourSpread / snap.Mid * 10000
	}
	sb.WriteString(fmt.Sprintf("--- LIVE ORDERS (%d) bid=%d/$%.2f  ask=%d/$%.2f  our_spread=%.2f bps ---\n",
		len(liveOrders), bidCount, bidNotional, askCount, askNotional, ourSpreadBps))
	for _, o := range liveOrders {
		sb.WriteString(fmt.Sprintf("  %s L%d: %.8f x %.6f (rem=%.6f) id=%s\n",
			o.Side, o.LevelIndex, o.Price, o.Qty, o.RemainingQty, o.OrderID))
	}

	// Desired orders from strategy
	sb.WriteString(fmt.Sprintf("--- STRATEGY OUTPUT: %s reason=%s desired=%d ---\n",
		output.Action, output.Reason, len(output.DesiredOrders)))
	for _, d := range output.DesiredOrders {
		sb.WriteString(fmt.Sprintf("  %s L%d: %.8f x %.6f  tag=%s\n",
			d.Side, d.LevelIndex, d.Price, d.Qty, d.Tag))
	}
	sb.WriteString("====================================")

	log.Print(sb.String())
}

// loadMarketInfo fetches exchange info (tick size, step size, etc.) via REST.
func (e *Engine) loadMarketInfo(ctx context.Context) error {
	info, err := e.exch.GetExchangeInfo(ctx, e.cfg.Symbol)
	if err != nil {
		return fmt.Errorf("get exchange info: %w", err)
	}

	for _, sym := range info.Symbols {
		if sym.Symbol == e.cfg.Symbol {
			var tickSize, stepSize, minNotional, maxOrderQty float64
			for _, f := range sym.Filters {
				switch f.FilterType {
				case "PRICE_FILTER":
					tickSize = parseFilterFloat(f.BidMultiplierUp)
				case "LOT_SIZE":
					maxOrderQty = parseFilterFloat(f.MaxQty)
				case "NOTIONAL", "MIN_NOTIONAL":
					minNotional = parseFilterFloat(f.MinNotional)
				}
			}
			// Use exchange info precision as fallback
			if tickSize <= 0 {
				tickSize = math.Pow(10, -float64(sym.QuotePrecision))
			}
			if stepSize <= 0 {
				stepSize = math.Pow(10, -float64(sym.BaseAssetPrecision))
			}
			e.state.SetMarketInfo(tickSize, stepSize, minNotional, maxOrderQty)
			log.Printf("[ENGINE] Market info: tick=%.10f step=%.10f minNotional=%.4f maxQty=%.4f",
				tickSize, stepSize, minNotional, maxOrderQty)
			return nil
		}
	}

	return fmt.Errorf("symbol %s not found in exchange info", e.cfg.Symbol)
}

// initialSync fetches initial state via REST.
func (e *Engine) initialSync(ctx context.Context) error {
	// Fetch depth
	depth, err := e.exch.GetDepth(ctx, e.cfg.Symbol)
	if err != nil {
		return fmt.Errorf("get depth: %w", err)
	}
	bids, asks := parseDepthLevels(depth)
	var bestBid, bestAsk float64
	if len(bids) > 0 {
		bestBid = bids[0].Price
	}
	if len(asks) > 0 {
		bestAsk = asks[0].Price
	}
	e.state.ApplyOrderBookUpdate(&OrderBookUpdateData{
		BestBid: bestBid, BestAsk: bestAsk, Bids: bids, Asks: asks,
	})

	// Fetch last trade price
	if ticker, err := e.exch.GetTicker(ctx, e.cfg.Symbol); err == nil && ticker > 0 {
		e.state.lastTradePrice = ticker
	}

	// Fetch balance
	acct, err := e.exch.GetAccount(ctx)
	if err != nil {
		return fmt.Errorf("get account: %w", err)
	}
	e.state.UpdateFromRESTAccount(acct)

	// Fetch open orders
	orders, err := e.exch.GetOpenOrders(ctx, e.cfg.Symbol)
	if err != nil {
		log.Printf("[ENGINE] Get open orders failed: %v (starting with empty)", err)
	} else {
		e.state.SyncOrdersFromREST(orders)
	}

	log.Printf("[ENGINE] Initial sync: bid=%.8f ask=%.8f orders=%d",
		bestBid, bestAsk, e.state.OrderCount())
	return nil
}

// publishBalancesToRedis publishes cached balances to Redis.
func (e *Engine) publishBalancesToRedis(ctx context.Context) {
	if e.redis == nil || e.cfg.Exchange == "" || e.cfg.BotID == "" {
		return
	}
	cachedBalances := e.state.balanceTracker.GetAllBalances()
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
		if err := e.redis.SetMMBalances(ctx, e.cfg.Exchange, e.cfg.Symbol, e.cfg.BotID, allBalances); err != nil {
			log.Printf("[ENGINE] Redis SetMMBalances failed: %v", err)
		}
	}
}

// syncOrdersToRedis replaces all orders for this bot in Redis Hash.
// Key: order:{exchange}:{symbol}:{botId} — O(1) clear + O(N) write in 1 pipeline.
// Runs async (goroutine) — receives a snapshot copy to avoid races.
func (e *Engine) syncOrdersToRedis(ctx context.Context, liveOrders []core.LiveOrder) {
	if e.redis == nil || e.cfg.Exchange == "" || e.cfg.BotID == "" {
		return
	}

	orders := make([]*store.OrderInfo, 0, len(liveOrders))
	for _, o := range liveOrders {
		orders = append(orders, &store.OrderInfo{
			OrderID:       o.OrderID,
			ClientOrderID: o.ClientOrderID,
			Exchange:      e.cfg.Exchange,
			Symbol:        e.cfg.Symbol,
			Side:          o.Side,
			Price:         o.Price,
			Quantity:      o.Qty,
			CreatedAt:     o.PlacedAt.UnixMilli(),
			Status:        "NEW",
			BotID:         e.cfg.BotID,
		})
	}

	if err := e.redis.ReplaceOrdersByBot(ctx, e.cfg.Exchange, e.cfg.Symbol, e.cfg.BotID, orders); err != nil {
		log.Printf("[ENGINE] Redis sync orders failed: %v", err)
	}
}

// emitMMOrderEvent publishes an order event to mm:stream:{exchange}:{symbol}.
func (e *Engine) emitMMOrderEvent(eventType core.OrderEventType, orderID, side string, price, qty float64, level int, reason string) {
	// Fire callback (wired in main.go → PublishMMOrderEvent)
	if e.onOrderEvent != nil {
		e.onOrderEvent(core.BotOrderEvent{
			Type:      eventType,
			Symbol:    e.cfg.Symbol,
			OrderID:   orderID,
			Side:      side,
			Price:     price,
			Qty:       qty,
			Level:     level,
			Reason:    reason,
			Timestamp: time.Now().UnixMilli(),
		})
	}
}

// reconcileFromREST syncs balance + open orders from REST API.
// Runs every ReconcileIntervalMs (default 60s) even when WS is healthy.
// Catches: WS drift, missed fills, phantom orders, stale balances.
func (e *Engine) reconcileFromREST(ctx context.Context) {
	start := time.Now()
	log.Printf("[RECONCILE] DEBUG: Starting REST reconcile (ws=%v)", e.state.wsConnected)

	// 1. Reconcile balance
	acct, err := e.exch.GetAccount(ctx)
	if err != nil {
		log.Printf("[RECONCILE] GetAccount failed: %v", err)
	} else {
		e.state.UpdateFromRESTAccount(acct)
		go e.publishBalancesToRedis(ctx)
	}

	// 2. Reconcile open orders
	orders, err := e.exch.GetOpenOrders(ctx, e.cfg.Symbol)
	drifted := false
	if err != nil {
		log.Printf("[RECONCILE] GetOpenOrders failed: %v", err)
	} else {
		wsBefore := e.state.orderTracker.Count()
		e.state.SyncOrdersFromREST(orders)
		restCount := e.state.orderTracker.Count()
		if wsBefore != restCount {
			log.Printf("[RECONCILE] Order count drift: WS=%d REST=%d — corrected (likely missed fill)", wsBefore, restCount)
			drifted = true
		}
	}

	// 3. Reconcile last trade price + orderbook
	e.refreshMarketData(ctx)

	log.Printf("[RECONCILE] Synced balance + %d orders from REST (took %v)", e.state.OrderCount(), time.Since(start))

	// 4. If drift detected, trigger immediate recalc to replace missing orders
	if drifted {
		log.Printf("[RECONCILE] Drift detected — triggering recalc")
		e.executeRecalc(ctx)
	}
}

// checkConfigUpdate checks MongoDB for config changes.
func (e *Engine) checkConfigUpdate(ctx context.Context) {
	if e.mongo == nil {
		return
	}
	// Delegate to mongo store's config check
	result, err := e.mongo.CheckConfigUpdate(ctx, e.cfg.BotID)
	if err != nil {
		log.Printf("[ENGINE] Config check error: %v", err)
		return
	}
	if result != nil && result.IsUpdated && result.SimpleConfig != nil {
		log.Printf("[ENGINE] Config changed — reloading")
		e.bus.Emit(Event{
			Type:      EventConfigReload,
			Timestamp: time.Now(),
			Data:      &ConfigReloadData{NewCfg: result.SimpleConfig},
		})
	}
}

// parseFilterFloat parses a filter value string to float64.
func parseFilterFloat(s string) float64 {
	if s == "" {
		return 0
	}
	return parseFloat(s)
}
