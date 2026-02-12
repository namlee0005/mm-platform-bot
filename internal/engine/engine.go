package engine

import (
	"context"
	"fmt"
	"log"
	"math"
	"strconv"
	"sync"
	"time"

	"mm-platform-engine/internal/exchange"
	"mm-platform-engine/internal/store"
	"mm-platform-engine/internal/types"
)

// Engine is the top-level market-making engine that orchestrates all modules.
type Engine struct {
	cfg   *Config
	exch  exchange.Exchange
	redis *store.RedisStore
	mongo *store.MongoStore

	// Sub-engines
	priceEngine  *PriceEngine
	spreadEngine *SpreadEngine
	invCtrl      *InventoryController
	riskGuard    *RiskGuard
	depthBuilder *DepthBuilder
	diffEngine   *OrderDiffEngine
	execEngine   *ExecutionEngine
	stateMachine *StateMachine
	reporter     *Reporter

	// State
	mu      sync.RWMutex
	running bool
	ctx     context.Context
	cancel  context.CancelFunc

	// Live order tracking (synced from WS events)
	liveOrdersMu sync.RWMutex
	liveOrders   map[string]*LiveOrder // orderID → LiveOrder

	// Cached balances from WebSocket
	balanceMu     sync.RWMutex
	cachedBalance map[string]*types.Balance

	// Anti-abuse: last fill time for cooldown
	lastFillTime time.Time
}

// NewEngine creates a fully wired engine from config.
func NewEngine(
	cfg *Config,
	exch exchange.Exchange,
	redis *store.RedisStore,
	mongo *store.MongoStore,
) *Engine {
	return &Engine{
		cfg:           cfg,
		exch:          exch,
		redis:         redis,
		mongo:         mongo,
		priceEngine:   NewPriceEngine(&cfg.Shock),
		spreadEngine:  NewSpreadEngine(cfg),
		invCtrl:       NewInventoryController(&cfg.Inventory),
		riskGuard:     NewRiskGuard(&cfg.Risk),
		depthBuilder:  NewDepthBuilder(cfg),
		diffEngine:    NewOrderDiffEngine(&cfg.Execution),
		execEngine:    NewExecutionEngine(&cfg.Execution, &cfg.AntiAbuse, exch, cfg.Symbol),
		stateMachine:  NewStateMachine(),
		reporter:      NewReporter(&cfg.Reporting),
		liveOrders:    make(map[string]*LiveOrder),
		cachedBalance: make(map[string]*types.Balance),
	}
}

// SetOrderEventCallback sets the callback for order events (for WS broadcasting)
func (e *Engine) SetOrderEventCallback(cb OrderEventCallback) {
	e.execEngine.SetOrderEventCallback(cb)
}

// Start initializes and runs the engine
func (e *Engine) Start(ctx context.Context) error {
	e.mu.Lock()
	if e.running {
		e.mu.Unlock()
		return fmt.Errorf("engine already running")
	}
	e.ctx, e.cancel = context.WithCancel(ctx)
	e.running = true
	e.mu.Unlock()

	log.Println("[ENGINE] Starting...")

	// Start exchange
	if err := e.exch.Start(e.ctx); err != nil {
		return fmt.Errorf("exchange start failed: %w", err)
	}

	// Cancel stale orders from previous run
	if err := e.exch.CancelAllOrders(e.ctx, e.cfg.Symbol); err != nil {
		log.Printf("[ENGINE] WARNING: Failed to cancel stale orders: %v", err)
	}

	// Subscribe to user stream
	if err := e.subscribeUserStream(); err != nil {
		return fmt.Errorf("user stream failed: %w", err)
	}

	// Sync current open orders from exchange
	if err := e.syncLiveOrders(); err != nil {
		log.Printf("[ENGINE] WARNING: Failed to sync live orders: %v", err)
	}

	// Start main loop
	go e.mainLoop()

	log.Println("[ENGINE] Started successfully")
	return nil
}

// Stop gracefully shuts down the engine
func (e *Engine) Stop(ctx context.Context) error {
	e.mu.Lock()
	if !e.running {
		e.mu.Unlock()
		return nil
	}
	e.mu.Unlock()

	log.Println("[ENGINE] Stopping...")

	// Use a shorter timeout for shutdown operations
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Stop main loop first
	if e.cancel != nil {
		log.Println("[ENGINE] Cancelling main loop...")
		e.cancel()
	}

	// Cancel all orders on exchange
	log.Println("[ENGINE] Cancelling all orders on exchange...")
	if err := e.exch.CancelAllOrders(shutdownCtx, e.cfg.Symbol); err != nil {
		log.Printf("[ENGINE] WARNING: Cancel failed on shutdown: %v", err)
	} else {
		log.Println("[ENGINE] Successfully cancelled all orders")
	}

	// Clear all orders from Redis
	if e.redis != nil {
		log.Println("[ENGINE] Clearing orders from Redis...")
		if err := e.redis.ClearAllOrders(shutdownCtx, e.cfg.Symbol); err != nil {
			log.Printf("[ENGINE] WARNING: Failed to clear orders from Redis: %v", err)
		} else {
			log.Println("[ENGINE] Successfully cleared orders from Redis")
		}
	}

	// Stop exchange (close WebSocket, etc.)
	log.Println("[ENGINE] Stopping exchange client...")
	if err := e.exch.Stop(shutdownCtx); err != nil {
		log.Printf("[ENGINE] WARNING: Exchange stop failed: %v", err)
	}

	e.mu.Lock()
	e.running = false
	e.mu.Unlock()

	log.Println("[ENGINE] Stopped")
	return nil
}

// mainLoop is the core tick loop
func (e *Engine) mainLoop() {
	interval := time.Duration(e.cfg.Execution.TickIntervalMs) * time.Millisecond
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// First tick immediately
	if err := e.tick(); err != nil {
		log.Printf("[ENGINE] Initial tick error: %v", err)
	}

	for {
		select {
		case <-e.ctx.Done():
			log.Println("[ENGINE] Main loop stopped")
			return
		case <-ticker.C:
			if err := e.tick(); err != nil {
				log.Printf("[ENGINE] Tick error: %v", err)
			}
		}
	}
}

// tick is the core quoting cycle, called every tick_interval
func (e *Engine) tick() error {
	now := time.Now()

	// ── Step 1: Market data snapshot ──
	snap, err := e.getSnapshot()
	if err != nil {
		return fmt.Errorf("snapshot failed: %w", err)
	}
	if snap.Mid <= 0 || snap.BestBid >= snap.BestAsk {
		// Invalid market → pause
		e.stateMachine.ForceMode(ModePaused, "invalid market data")
		return e.execEngine.CancelAll(e.ctx)
	}

	// ── Step 2: Price engine update + shock detection ──
	e.priceEngine.Update(snap.Mid, now)
	shockInfo := e.priceEngine.DetectShock()
	if !shockInfo.Detected {
		shockInfo = e.priceEngine.DetectSweep(snap)
	}

	// ── Step 3: Balances + inventory ──
	baseFree, quoteFree, baseLocked, quoteLocked := e.getBalances()
	baseValue := (baseFree + baseLocked) * snap.Mid
	quoteValue := quoteFree + quoteLocked
	totalValue := baseValue + quoteValue
	if totalValue < 1e-8 {
		return fmt.Errorf("zero NAV")
	}
	invRatio := baseValue / totalValue
	invDev := invRatio - e.cfg.Inventory.TargetRatio

	// ── Step 4: NAV tracking for drawdown ──
	e.riskGuard.RecordNAV(totalValue, now)

	// ── Step 5: State machine transition ──
	mode, transitioned := e.stateMachine.Evaluate(
		shockInfo, invDev, e.riskGuard, e.invCtrl, e.priceEngine,
	)
	_ = transitioned

	// ── Step 6: PAUSED → cancel all and return ──
	if mode == ModePaused {
		e.emitMetrics(snap, mode, invRatio, invDev, 0, 0, totalValue)
		return e.execEngine.CancelAll(e.ctx)
	}

	// ── Step 7: Fill cooldown check ──
	if e.riskGuard.IsInFillCooldown() {
		return nil // skip this tick, let orders rest
	}

	// ── Step 8: Compute skew + tilt ──
	skewBps := e.invCtrl.ComputeSkew(invDev)
	sizeTilt := e.invCtrl.ComputeSizeTilt(invDev, mode)

	// ── Step 9: Compute spread ──
	volMult := e.priceEngine.VolMultiplier(e.cfg.Spread.VolMultiplierCap)
	spreadBps := e.spreadEngine.ComputeSpread(volMult, invDev, mode)

	// Apply drawdown-based size reduction if configured as "reduce"
	drawdownMult := 1.0
	if e.cfg.Risk.DrawdownAction == "reduce" {
		drawdownMult = e.riskGuard.DrawdownReduceMultiplier()
	}

	// ── Step 10: Build desired ladder ──
	desired := e.depthBuilder.BuildLadder(
		snap, spreadBps, skewBps, sizeTilt, mode, invDev, now.UnixMilli(),
	)

	// Apply drawdown multiplier to sizes
	if drawdownMult < 1.0 {
		for i := range desired {
			desired[i].Qty *= drawdownMult
		}
	}

	// ── Step 11: Enforce depth KPI ──
	desired = e.depthBuilder.EnforceDepthKPI(desired, snap)

	// ── Step 12: Budget constraints ──
	// Use total balance (free + locked) because locked balance is in our existing orders
	// which can be cancelled/replaced if needed
	baseTotal := baseFree + baseLocked
	quoteTotal := quoteFree + quoteLocked
	desired = e.depthBuilder.ApplyBudgetConstraints(desired, baseTotal, quoteTotal)

	// ── Step 13: Compute diff vs live orders ──
	e.liveOrdersMu.RLock()
	live := make([]LiveOrder, 0, len(e.liveOrders))
	for _, lo := range e.liveOrders {
		live = append(live, *lo)
	}
	e.liveOrdersMu.RUnlock()

	log.Printf("[DEBUG] desired=%d live=%d", len(desired), len(live))

	diffs := e.diffEngine.Diff(desired, live)
	places, amends, cancels := CountByAction(diffs)

	log.Printf("[ENGINE] mode=%s mid=%.6f spread=%.1fbps skew=%.1fbps tilt=%.2f inv=%.2f%% dd=%.2f%% | diff: +%d ~%d -%d",
		mode, snap.Mid, spreadBps, skewBps, sizeTilt,
		invRatio*100, e.riskGuard.Drawdown24h()*100,
		places, amends, cancels)

	// ── Step 14: Execute diff ──
	if err := e.execEngine.Execute(e.ctx, diffs); err != nil {
		return fmt.Errorf("execution failed: %w", err)
	}

	// ── Step 15: Emit metrics ──
	e.emitMetrics(snap, mode, invRatio, invDev, skewBps, sizeTilt, totalValue)

	return nil
}

// getSnapshot fetches the current market snapshot
func (e *Engine) getSnapshot() (*Snapshot, error) {
	// Get depth
	depth, err := e.exch.GetDepth(e.ctx, e.cfg.Symbol)
	if err != nil {
		return nil, fmt.Errorf("get depth: %w", err)
	}
	if len(depth.Bids) == 0 || len(depth.Asks) == 0 {
		return nil, fmt.Errorf("empty order book")
	}

	bestBid, _ := strconv.ParseFloat(depth.Bids[0][0], 64)
	bestAsk, _ := strconv.ParseFloat(depth.Asks[0][0], 64)

	// Get exchange info for tick/step sizes
	info, err := e.exch.GetExchangeInfo(e.ctx, e.cfg.Symbol)
	if err != nil {
		return nil, fmt.Errorf("get exchange info: %w", err)
	}

	var tickSize, stepSize, minNotional float64
	for _, sym := range info.Symbols {
		if sym.Symbol == e.cfg.Symbol {
			tickSize = math.Pow10(-sym.QuoteAssetPrecision)
			stepSize = math.Pow10(-sym.BaseAssetPrecision)
			for _, f := range sym.Filters {
				if f.FilterType == "MIN_NOTIONAL" || f.FilterType == "NOTIONAL" {
					if f.MinNotional != "" {
						minNotional, _ = strconv.ParseFloat(f.MinNotional, 64)
					}
				}
			}
			break
		}
	}
	if minNotional <= 0 {
		minNotional = 5.0
	}

	// Parse full book for sweep detection
	bids := make([]PriceLevel, 0, len(depth.Bids))
	for _, b := range depth.Bids {
		p, _ := strconv.ParseFloat(b[0], 64)
		q, _ := strconv.ParseFloat(b[1], 64)
		bids = append(bids, PriceLevel{Price: p, Qty: q})
	}
	asks := make([]PriceLevel, 0, len(depth.Asks))
	for _, a := range depth.Asks {
		p, _ := strconv.ParseFloat(a[0], 64)
		q, _ := strconv.ParseFloat(a[1], 64)
		asks = append(asks, PriceLevel{Price: p, Qty: q})
	}

	return &Snapshot{
		BestBid:     bestBid,
		BestAsk:     bestAsk,
		Mid:         (bestBid + bestAsk) / 2.0,
		TickSize:    tickSize,
		StepSize:    stepSize,
		MinNotional: minNotional,
		Timestamp:   time.Now(),
		Bids:        bids,
		Asks:        asks,
	}, nil
}

// getBalances returns cached or fetched balances
func (e *Engine) getBalances() (baseFree, quoteFree, baseLocked, quoteLocked float64) {
	e.balanceMu.RLock()
	baseB := e.cachedBalance[e.cfg.BaseAsset]
	quoteB := e.cachedBalance[e.cfg.QuoteAsset]
	e.balanceMu.RUnlock()

	if baseB != nil {
		baseFree = baseB.Free
		baseLocked = baseB.Locked
	}
	if quoteB != nil {
		quoteFree = quoteB.Free
		quoteLocked = quoteB.Locked
	}

	// If no cached data, fetch from REST
	if baseB == nil && quoteB == nil {
		acct, err := e.exch.GetAccount(e.ctx)
		if err != nil {
			log.Printf("[ENGINE] Balance fetch error: %v", err)
			return
		}
		for _, b := range acct.Balances {
			if b.Asset == e.cfg.BaseAsset {
				baseFree = b.Free
				baseLocked = b.Locked
			}
			if b.Asset == e.cfg.QuoteAsset {
				quoteFree = b.Free
				quoteLocked = b.Locked
			}
		}
	}

	return
}

// syncLiveOrders fetches current open orders from exchange into the live order map
func (e *Engine) syncLiveOrders() error {
	orders, err := e.exch.GetOpenOrders(e.ctx, e.cfg.Symbol)
	if err != nil {
		return err
	}

	e.liveOrdersMu.Lock()
	defer e.liveOrdersMu.Unlock()

	e.liveOrders = make(map[string]*LiveOrder, len(orders))
	for _, o := range orders {
		// Parse level from client order ID
		level := parseLevelFromTag(o.ClientOrderID)
		e.liveOrders[o.OrderID] = &LiveOrder{
			OrderID:       o.OrderID,
			ClientOrderID: o.ClientOrderID,
			Side:          o.Side,
			Price:         o.Price,
			Qty:           o.Quantity,
			RemainingQty:  o.Quantity - o.ExecutedQty,
			LevelIndex:    level,
			PlacedAt:      time.Now(), // approximate
		}
		log.Printf("[SYNC] Order %s side=%s level=%d clientID=%s price=%.8f",
			o.OrderID, o.Side, level, o.ClientOrderID, o.Price)
	}

	log.Printf("[ENGINE] Synced %d live orders from exchange", len(e.liveOrders))
	return nil
}

// subscribeUserStream sets up WebSocket event handlers
func (e *Engine) subscribeUserStream() error {
	handlers := exchange.UserStreamHandlers{
		OnAccountUpdate: e.handleAccountUpdate,
		OnOrderUpdate:   e.handleOrderUpdate,
		OnFill:          e.handleFill,
		OnError: func(err error) {
			log.Printf("[ENGINE] WebSocket error: %v", err)
			// TODO: implement reconnection (see existing bot.reconnectUserStream)
		},
	}

	return e.exch.SubscribeUserStream(e.ctx, handlers)
}

func (e *Engine) handleAccountUpdate(event *types.AccountEvent) {
	e.balanceMu.Lock()
	for i := range event.Balances {
		b := &event.Balances[i]
		e.cachedBalance[b.Asset] = b
	}
	e.balanceMu.Unlock()

	// Publish to Redis for dashboard
	if e.redis != nil {
		_ = e.redis.PublishAccountUpdate(e.ctx, event)
	}
}

func (e *Engine) handleOrderUpdate(event *types.OrderEvent) {
	e.liveOrdersMu.Lock()
	defer e.liveOrdersMu.Unlock()

	switch event.Status {
	case "NEW":
		// Parse level from client order ID if possible
		level := parseLevelFromTag(event.ClientOrderID)
		log.Printf("[WS] Order NEW: id=%s side=%s level=%d clientID=%s price=%.8f",
			event.OrderID, event.Side, level, event.ClientOrderID, event.Price)
		e.liveOrders[event.OrderID] = &LiveOrder{
			OrderID:       event.OrderID,
			ClientOrderID: event.ClientOrderID,
			Side:          event.Side,
			Price:         event.Price,
			Qty:           event.Quantity,
			RemainingQty:  event.Quantity,
			LevelIndex:    level,
			PlacedAt:      event.Timestamp,
		}
	case "PARTIALLY_FILLED":
		if lo, ok := e.liveOrders[event.OrderID]; ok {
			lo.RemainingQty = event.Quantity - event.ExecutedQty
		}
		log.Printf("[WS] Order PARTIALLY_FILLED: id=%s", event.OrderID)
	case "FILLED", "CANCELED", "EXPIRED", "REJECTED":
		log.Printf("[WS] Order %s: id=%s", event.Status, event.OrderID)
		delete(e.liveOrders, event.OrderID)
	}

	// Publish to Redis
	if e.redis != nil {
		_ = e.redis.PublishOrderUpdate(e.ctx, event)
	}
}

func (e *Engine) handleFill(event *types.FillEvent) {
	now := time.Now()

	// Record in risk guard for fill rate tracking
	e.riskGuard.RecordFill(now)

	// Record sanitized trade log
	e.reporter.RecordTrade(TradeLogEntry{
		Timestamp: event.Timestamp,
		Symbol:    event.Symbol,
		Side:      event.Side,
		Price:     event.Price,
		Qty:       event.Quantity,
		Notional:  event.Price * event.Quantity,
		Fee:       event.Commission,
		FeeAsset:  event.CommissionAsset,
		TradeID:   event.TradeID,
	})

	// Alert if fill rate is high
	if e.riskGuard.IsFillRateExceeded() {
		e.reporter.Alert("WARNING", fmt.Sprintf(
			"Fill rate exceeded: %.0f/min (limit: %.0f)",
			e.riskGuard.FillsPerMin(), e.cfg.Risk.MaxFillsPerMin))
	}

	// Publish to Redis + MongoDB
	if e.redis != nil {
		_ = e.redis.PublishFill(e.ctx, event)
	}
	if e.mongo != nil {
		_ = e.mongo.SaveFill(e.ctx, event)
	}

	e.lastFillTime = now
}

// emitMetrics builds and emits the current metrics snapshot
func (e *Engine) emitMetrics(
	snap *Snapshot,
	mode Mode,
	invRatio, invDev, skewBps, sizeTilt, nav float64,
) {
	// Count depth within ±2%
	depthRange := snap.Mid * 0.02
	var depthNotional float64
	var bidCount, askCount int

	e.liveOrdersMu.RLock()
	for _, lo := range e.liveOrders {
		if lo.Side == "BUY" && lo.Price >= snap.Mid-depthRange {
			depthNotional += lo.Price * lo.RemainingQty
			bidCount++
		}
		if lo.Side == "SELL" && lo.Price <= snap.Mid+depthRange {
			depthNotional += lo.Price * lo.RemainingQty
			askCount++
		}
	}
	e.liveOrdersMu.RUnlock()

	// Compute average spread from best bid/ask
	spreadBps := 0.0
	if snap.Mid > 0 {
		spreadBps = (snap.BestAsk - snap.BestBid) / snap.Mid * 10000.0
	}

	baseFree, quoteFree, baseLocked, quoteLocked := e.getBalances()
	baseValue := (baseFree + baseLocked) * snap.Mid
	quoteValue := quoteFree + quoteLocked

	m := EngineMetrics{
		Timestamp:     time.Now(),
		Mode:          mode,
		Mid:           snap.Mid,
		AvgSpreadBps:  spreadBps,
		DepthNotional: depthNotional,
		NumBidOrders:  bidCount,
		NumAskOrders:  askCount,
		InvRatio:      invRatio,
		InvDeviation:  invDev,
		SkewBps:       skewBps,
		SizeTilt:      sizeTilt,
		Drawdown24h:   e.riskGuard.Drawdown24h(),
		FillsPerMin:   e.riskGuard.FillsPerMin(),
		QuoteUptime:   e.stateMachine.QuoteUptime(),
		RealizedVol:   e.priceEngine.RealizedVol(),
		BaseValue:     baseValue,
		QuoteValue:    quoteValue,
		NAV:           nav,
	}

	e.reporter.EmitMetrics(m)
}

// parseLevelFromTag extracts level index from tag like "MM_123_L2_BID"
func parseLevelFromTag(tag string) int {
	// Simple parser: find "L" followed by digit
	for i := 0; i < len(tag)-1; i++ {
		if tag[i] == 'L' && tag[i+1] >= '0' && tag[i+1] <= '9' {
			level := int(tag[i+1] - '0')
			// Check for multi-digit
			if i+2 < len(tag) && tag[i+2] >= '0' && tag[i+2] <= '9' {
				level = level*10 + int(tag[i+2]-'0')
			}
			return level
		}
	}
	return 0
}

// GetMode returns the current engine mode
func (e *Engine) GetMode() Mode {
	return e.stateMachine.CurrentMode()
}

// GetMetrics returns recent metrics for dashboard
func (e *Engine) GetMetrics(n int) []EngineMetrics {
	return e.reporter.GetRecentMetrics(n)
}

// GetTradeLog returns sanitized trade log
func (e *Engine) GetTradeLog(limit int) []TradeLogEntry {
	return e.reporter.GetTradeLog(limit)
}

// ForceMode allows manual mode override (admin use)
func (e *Engine) ForceMode(mode Mode, reason string) {
	e.stateMachine.ForceMode(mode, reason)
}
