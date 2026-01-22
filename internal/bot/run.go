package bot

import (
	"context"
	"fmt"
	"log"
	"math"
	"math/rand"
	"mm-platform-engine/internal/exchange"
	"mm-platform-engine/internal/types"
	"mm-platform-engine/internal/utils"
	"strconv"
	"strings"
	"sync"
)

// OrderResult represents the result of placing an order
type OrderResult struct {
	OrderID string
	Side    string
	Price   float64
	Qty     float64
	Tag     string
	Error   error
}

func (b *Bot) run(ctx context.Context) error {
	// Ensure state is initialized
	if b.state == nil {
		b.state = &types.EngineState{}
	}

	// Ensure metricsAgg is initialized
	if b.metricsAgg == nil {
		b.metricsAgg = NewMetricsAggregator(5 * 60 * 1000)
	}

	marketData, err := b.getMarketData(ctx)
	if err != nil {
		return err
	}
	if marketData == nil {
		return fmt.Errorf("market data is nil")
	}

	balState, err := b.getBalanceState(ctx)
	if err != nil {
		return err
	}
	if balState == nil {
		return fmt.Errorf("balance state is nil")
	}

	// Calculate current inventory ratio
	mid := (marketData.BestBid + marketData.BestAsk) / 2
	baseValue := (balState.BaseFree + balState.BaseLocked) * mid
	quoteValue := balState.QuoteFree + balState.QuoteLocked
	totalValue := baseValue + quoteValue + 0.0000001 // epsilon
	currentInvRatio := baseValue / totalValue

	// Use last inventory ratio as previous (or current if first run)
	prevInvRatio := currentInvRatio
	if b.state.LastInvDev != 0 {
		// LastInvDev = invRatio - targetRatio, so invRatio = LastInvDev + targetRatio
		prevInvRatio = b.state.LastInvDev + b.cfg.TradingConfig.TargetRatio
	}

	// Compute metrics
	metrics := b.metricsAgg.ComputeMetrics(
		marketData.NowUnixMs,
		currentInvRatio,
		prevInvRatio,
	)

	// Evaluate and get plan
	plan := b.evaluateDN(*marketData, *balState, metrics)

	// Debug: Show plan details
	debugMid := (marketData.BestBid + marketData.BestAsk) / 2
	debugBaseValue := (balState.BaseFree + balState.BaseLocked) * debugMid
	debugQuoteValue := balState.QuoteFree + balState.QuoteLocked
	debugTotalValue := debugBaseValue + debugQuoteValue
	debugInvRatio := debugBaseValue / debugTotalValue
	debugInvDev := debugInvRatio - b.cfg.TradingConfig.TargetRatio

	fmt.Printf("\n========== PLAN DEBUG ==========\n")
	fmt.Printf("k=%.2f, max_skew_bps=%d, min_offset_bps=%d, d_skew_max_bps_per_tick=%d\n", b.cfg.TradingConfig.K, b.cfg.TradingConfig.MaxSkewBps, b.cfg.TradingConfig.MinOffsetBps, b.cfg.TradingConfig.DSkewMaxBpsPerTick)
	fmt.Printf("Market: BestBid=%.6f, BestAsk=%.6f, Mid=%.6f\n", marketData.BestBid, marketData.BestAsk, debugMid)
	fmt.Printf("Balance: BaseFree=%.2f, QuoteFree=%.2f\n", balState.BaseFree, balState.QuoteFree)
	fmt.Printf("Inventory: BaseValue=%.2f, QuoteValue=%.2f, Total=%.2f\n", debugBaseValue, debugQuoteValue, debugTotalValue)
	fmt.Printf("InvRatio=%.4f (target=%.2f), InvDev=%.4f\n", debugInvRatio, b.cfg.TradingConfig.TargetRatio, debugInvDev)
	fmt.Printf("Skew: LastSkewBps=%.2f\n", b.state.LastSkewBps)
	fmt.Printf("Plan: State=%s, Action=%s, Reason=%s\n", plan.State, plan.Action, plan.Reason)
	fmt.Printf("Orders (%d):\n", len(plan.Orders))
	for i, o := range plan.Orders {
		fmt.Printf("  [%d] %s: Price=%.6f, Qty=%.2f, Notional=%.2f, Tag=%s\n",
			i, o.Side, o.Price, o.Qty, o.Price*o.Qty, o.Tag)
	}
	fmt.Printf("=================================\n\n")

	// Execute the plan
	if err := b.executePlan(ctx, plan, marketData); err != nil {
		return fmt.Errorf("failed to execute plan: %w", err)
	}

	return nil
}

func (b *Bot) getBalanceState(ctx context.Context) (*types.BalanceState, error) {
	balanceState := &types.BalanceState{}

	// Try to use cached balance from WebSocket first
	b.balanceMu.RLock()
	baseBalance := b.cachedBalance[b.cfg.TradingConfig.BaseAsset]
	quoteBalance := b.cachedBalance[b.cfg.TradingConfig.QuoteAsset]
	hasCachedData := baseBalance != nil || quoteBalance != nil
	b.balanceMu.RUnlock()

	if hasCachedData {
		// Use cached balance from WebSocket (real-time)
		if baseBalance != nil {
			balanceState.BaseFree = baseBalance.Free
			balanceState.BaseLocked = baseBalance.Locked
		}
		if quoteBalance != nil {
			balanceState.QuoteFree = quoteBalance.Free
			balanceState.QuoteLocked = quoteBalance.Locked
		}
		return balanceState, nil
	}

	// Fallback: fetch from REST API if no cached data yet
	account, err := b.exchange.GetAccount(ctx)
	if err != nil {
		return balanceState, err
	}

	balances := account.Balances
	if len(balances) == 0 {
		return balanceState, nil
	}

	for _, balance := range balances {
		if strings.ToLower(balance.Asset) == strings.ToLower(b.cfg.TradingConfig.BaseAsset) {
			balanceState.BaseFree = balance.Free
			balanceState.BaseLocked = balance.Locked
		}
		if strings.ToLower(balance.Asset) == strings.ToLower(b.cfg.TradingConfig.QuoteAsset) {
			balanceState.QuoteFree = balance.Free
			balanceState.QuoteLocked = balance.Locked
		}
	}

	return balanceState, nil
}

func (b *Bot) getMarketData(ctx context.Context) (*types.MarketData, error) {
	// get current market data
	exchangeInfo, err := b.exchange.GetExchangeInfo(ctx, b.cfg.TradingConfig.Symbol)
	if err != nil {
		return nil, err
	}

	// find symbol
	var sym *exchange.Symbol
	for i := range exchangeInfo.Symbols {
		if exchangeInfo.Symbols[i].Symbol == b.cfg.TradingConfig.Symbol {
			sym = &exchangeInfo.Symbols[i]
			break
		}
	}
	if sym == nil {
		return nil, fmt.Errorf("symbol not found: %s", b.cfg.TradingConfig.Symbol)
	}

	var bidMulUp, askMulDown float64
	for _, f := range sym.Filters {
		if f.FilterType == "PERCENT_PRICE_BY_SIDE" {
			bidMulUp, _ = strconv.ParseFloat(f.BidMultiplierUp, 64)
			askMulDown, _ = strconv.ParseFloat(f.AskMultiplierDown, 64)
			break
		}
	}

	// 4. Derive constraints
	tickSize := math.Pow10(-sym.QuoteAssetPrecision)
	stepSize := math.Pow10(-sym.BaseAssetPrecision)

	depth, err := b.exchange.GetDepth(ctx, b.cfg.TradingConfig.Symbol)
	if err != nil {
		return nil, err
	}

	if len(depth.Bids) == 0 || len(depth.Asks) == 0 {
		return nil, fmt.Errorf("empty order book")
	}

	bestBid, _ := strconv.ParseFloat(depth.Bids[0][0], 64)
	bestAsk, _ := strconv.ParseFloat(depth.Asks[0][0], 64)

	return &types.MarketData{
		BestBid:           bestBid,
		BestAsk:           bestAsk,
		TickSize:          tickSize,
		StepSize:          stepSize,
		BidMultiplierUp:   bidMulUp,
		AskMultiplierDown: askMulDown,
		MinNotional:       5,
		NowUnixMs:         exchangeInfo.ServerTime,
	}, nil
}

// isMarketValid checks if market snapshot is valid
func isMarketValid(snap types.MarketData) bool {
	return snap.BestBid > 0 &&
		snap.BestAsk > 0 &&
		snap.BestBid < snap.BestAsk &&
		snap.TickSize > 0 &&
		snap.StepSize > 0
}

func (b *Bot) evaluateDN(
	marketData types.MarketData,
	bal types.BalanceState,
	metrics types.RollingMetrics,
) types.ReplacePlan {
	// Step 1: Safety checks - invalid market data
	if !isMarketValid(marketData) {
		return types.ReplacePlan{
			State:  types.ModePaused,
			Action: types.ActionCancelAll,
			Reason: "invalid or stale market data",
			Orders: nil,
		}
	}

	// Step 2: Compute mid price
	mid := (marketData.BestBid + marketData.BestAsk) / 2

	// Step 3: Compute inventory ratio by value
	baseValue := (bal.BaseFree + bal.BaseLocked) * mid
	quoteValue := bal.QuoteFree + bal.QuoteLocked
	totalValue := baseValue + quoteValue + utils.Epsilon
	invRatio := baseValue / totalValue
	invDev := invRatio - b.cfg.TradingConfig.TargetRatio

	// Step 4: Determine mode (NORMAL or RISK)
	mode := b.determineMode(metrics)

	// Step 5: Compute skew with deadzone and rate limiting
	skewBps := b.computeSkew(invDev)

	// Step 6: Apply mode modifiers
	effectiveOffsets := make([]int, len(b.cfg.TradingConfig.OffsetsBps))
	effectiveSizeMult := make([]float64, len(b.cfg.TradingConfig.SizeMult))

	spreadMult := 1.0
	sizeMult := 1.0
	if mode == types.ModeRisk {
		spreadMult = b.cfg.TradingConfig.RiskActions.RiskSpreadMult
		sizeMult = b.cfg.TradingConfig.RiskActions.RiskSizeMult
	}

	for i := range b.cfg.TradingConfig.OffsetsBps {
		effectiveOffsets[i] = int(float64(b.cfg.TradingConfig.OffsetsBps[i]) * spreadMult)
		effectiveSizeMult[i] = b.cfg.TradingConfig.SizeMult[i] * sizeMult
	}

	// Step 7: Build ladder orders
	orders := b.buildLadder(mid, skewBps, effectiveOffsets, effectiveSizeMult, marketData, marketData.NowUnixMs)

	// Step 8: Budget check and scale down if needed
	orders = b.applyBudgetConstraints(orders, bal)

	// Step 9: Determine replace action
	action, reason := b.shouldReplace(mid, invDev, mode, marketData.NowUnixMs)

	// Update engine state
	b.state.LastMid = mid
	b.state.LastInvDev = invDev
	b.state.LastSkewBps = skewBps
	b.state.LastMode = mode

	if action == types.ActionReplace {
		b.state.LastRefreshMs = marketData.NowUnixMs
		b.state.OldestOrderAgeMs = 0
	} else if b.state.OldestOrderAgeMs == 0 {
		b.state.OldestOrderAgeMs = marketData.NowUnixMs
	}

	return types.ReplacePlan{
		State:  mode,
		Action: action,
		Reason: reason,
		Orders: orders,
	}
}

// determineMode determines the operating mode based on metrics
func (b *Bot) determineMode(m types.RollingMetrics) types.Mode {
	rt := b.cfg.TradingConfig.RiskThresholds

	// Check for fast fills + spike
	if m.TtfP50Sec < rt.TtfFastSec && m.FillsPerMin > rt.FillSpikePerMin {
		return types.ModeRisk
	}

	// Check for extreme imbalance
	if m.HitImbalance > rt.ImbHigh || m.HitImbalance < rt.ImbLow {
		return types.ModeRisk
	}

	// Check for fast inventory drift
	if utils.Abs(m.InvDriftPerHour) > rt.DriftFastPerHour {
		return types.ModeRisk
	}

	return types.ModeNormal
}

// computeSkew computes the price skew based on inventory deviation
func (b *Bot) computeSkew(invDev float64) float64 {
	// Apply deadzone
	if utils.Abs(invDev) <= b.cfg.TradingConfig.Deadzone {
		return 0
	}

	// Compute raw skew
	rawSkewBps := b.cfg.TradingConfig.K * invDev * 10000.0

	// Clamp to max skew
	skewBps := utils.Clamp(rawSkewBps, float64(-b.cfg.TradingConfig.MaxSkewBps), float64(b.cfg.TradingConfig.MaxSkewBps))

	// Note: Rate limiting disabled to allow immediate skew adjustment
	// This ensures the bot reacts quickly to inventory imbalance
	// If you want smoother transitions, uncomment the rate limiting below:
	//
	maxChange := float64(b.cfg.TradingConfig.DSkewMaxBpsPerTick)
	if utils.Abs(skewBps-b.state.LastSkewBps) > maxChange {
		if skewBps > b.state.LastSkewBps {
			skewBps = b.state.LastSkewBps + maxChange
		} else {
			skewBps = b.state.LastSkewBps - maxChange
		}
	}

	return skewBps
}

// buildLadder constructs the ladder of orders
func (b *Bot) buildLadder(
	mid float64,
	skewBps float64,
	offsets []int,
	sizeMult []float64,
	snap types.MarketData,
	timestamp int64,
) []types.OrderIntent {
	orders := make([]types.OrderIntent, 0, len(offsets)*2)

	// Generate unique batch ID using timestamp + random suffix
	batchID := fmt.Sprintf("%d_%04d", timestamp, rand.Intn(10000))

	// Size skew disabled - using price skew only for inventory rebalancing
	// The idea is to place BID orders far from mid (unlikely to fill) and ASK orders close to mid (easy to fill)
	// This achieves rebalancing without reducing order sizes
	fmt.Printf("Price Skew: skewBps=%.2f (no size skew applied)\n", skewBps)

	for i, baseOffset := range offsets {
		// Compute offsets with skew (ADDITIVE formula)
		// When skewBps > 0 (too much base/AMI):
		//   - BID offset = baseOffset + skewBps (pushed far from mid)
		//   - ASK offset = baseOffset - skewBps (pulled close to mid, clamped to min)
		// When skewBps < 0 (too much quote/USDT):
		//   - BID offset = baseOffset + skewBps (pulled close to mid, skewBps is negative)
		//   - ASK offset = baseOffset - skewBps (pushed far from mid, skewBps is negative so this adds)

		bidOffsetBps := float64(baseOffset) + skewBps
		askOffsetBps := float64(baseOffset) - skewBps

		// Ensure minimum offset
		bidOffsetBps = utils.Max(bidOffsetBps, float64(b.cfg.TradingConfig.MinOffsetBps))
		askOffsetBps = utils.Max(askOffsetBps, float64(b.cfg.TradingConfig.MinOffsetBps))

		// Compute prices
		bidPrice := utils.RoundDown(mid*(1.0-bidOffsetBps/10000.0), snap.TickSize)
		askPrice := utils.RoundUp(mid*(1.0+askOffsetBps/10000.0), snap.TickSize)

		// Ensure bid < ask (uncross if needed)
		if bidPrice >= askPrice {
			bidPrice = askPrice - snap.TickSize
			if bidPrice <= 0 {
				continue // Skip this level
			}
		}

		// Compute quantities (no size skew - using price skew only)
		quoteAmount := b.cfg.TradingConfig.QuotePerOrder * sizeMult[i]
		bidQty := utils.FloorToStep(quoteAmount/bidPrice, snap.StepSize)
		askQty := utils.FloorToStep(quoteAmount/askPrice, snap.StepSize)

		// Check minimum notional and non-zero quantity
		bidNotional := bidPrice * bidQty
		askNotional := askPrice * askQty

		if bidQty > 0 && bidNotional >= snap.MinNotional {
			orders = append(orders, types.OrderIntent{
				Side:       types.OrderSideBuy,
				Price:      bidPrice,
				Qty:        bidQty,
				LevelIndex: i,
				Tag:        fmt.Sprintf("DN_%s_L%d_BID", batchID, i),
			})
		}

		if askQty > 0 && askNotional >= snap.MinNotional {
			orders = append(orders, types.OrderIntent{
				Side:       types.OrderSideSell,
				Price:      askPrice,
				Qty:        askQty,
				LevelIndex: i,
				Tag:        fmt.Sprintf("DN_%s_L%d_ASK", batchID, i),
			})
		}
	}

	return orders
}

// applyBudgetConstraints ensures orders fit within available balance
func (b *Bot) applyBudgetConstraints(orders []types.OrderIntent, bal types.BalanceState) []types.OrderIntent {
	// Calculate required balances
	var requiredBase, requiredQuote float64
	for _, order := range orders {
		if order.Side == types.OrderSideBuy {
			requiredQuote += order.Price * order.Qty
		} else {
			requiredBase += order.Qty
		}
	}

	// Check if we have enough balance
	if requiredBase <= bal.BaseFree && requiredQuote <= bal.QuoteFree {
		return orders // All good
	}

	// Need to scale down - first try proportional scaling
	baseScale := 1.0
	quoteScale := 1.0

	if requiredBase > bal.BaseFree && requiredBase > utils.Epsilon {
		baseScale = bal.BaseFree / requiredBase
	}
	if requiredQuote > bal.QuoteFree && requiredQuote > utils.Epsilon {
		quoteScale = bal.QuoteFree / requiredQuote
	}

	scaleFactor := utils.Min(baseScale, quoteScale)

	// If scale factor is reasonable (>0.5), just scale all orders
	if scaleFactor > 0.5 {
		return b.scaleOrders(orders, scaleFactor)
	}

	// Otherwise, drop far levels first
	return b.dropFarLevels(orders, bal)
}

// scaleOrders scales all order sizes by a factor
func (b *Bot) scaleOrders(orders []types.OrderIntent, scale float64) []types.OrderIntent {
	scaled := make([]types.OrderIntent, 0, len(orders))
	for _, order := range orders {
		order.Qty = utils.FloorToStep(order.Qty*scale, 0.00000001) // Use small step for scaling
		if order.Qty > 0 {
			scaled = append(scaled, order)
		}
	}
	return scaled
}

// dropFarLevels drops levels starting from the farthest until budget is satisfied
func (b *Bot) dropFarLevels(orders []types.OrderIntent, bal types.BalanceState) []types.OrderIntent {
	// Find max level index
	maxLevel := -1
	for _, order := range orders {
		if order.LevelIndex > maxLevel {
			maxLevel = order.LevelIndex
		}
	}

	// Try dropping levels one by one from farthest
	for dropLevel := maxLevel; dropLevel >= 0; dropLevel-- {
		filtered := make([]types.OrderIntent, 0)
		var requiredBase, requiredQuote float64

		for _, order := range orders {
			if order.LevelIndex < dropLevel {
				filtered = append(filtered, order)
				if order.Side == types.OrderSideBuy {
					requiredQuote += order.Price * order.Qty
				} else {
					requiredBase += order.Qty
				}
			}
		}

		// Check if this fits
		if requiredBase <= bal.BaseFree && requiredQuote <= bal.QuoteFree {
			return filtered
		}
	}

	// If nothing fits, return empty
	return []types.OrderIntent{}
}

// shouldReplace determines if orders should be replaced
func (b *Bot) shouldReplace(mid, invDev float64, mode types.Mode, nowMs int64) (types.Action, string) {
	rt := b.cfg.TradingConfig.ReplaceThresholds

	// Check if mode changed
	if mode != b.state.LastMode {
		return types.ActionReplace, fmt.Sprintf("mode changed: %s -> %s", b.state.LastMode, mode)
	}

	// Check mid price movement
	if b.state.LastMid > 0 {
		midChange := utils.Abs(mid-b.state.LastMid) / b.state.LastMid
		midChangeBps := midChange * 10000.0
		if midChangeBps > float64(rt.RepriceThresholdBps) {
			return types.ActionReplace, fmt.Sprintf("mid moved %.2f bps", midChangeBps)
		}
	}

	// Check inventory deviation change
	if utils.Abs(invDev-b.state.LastInvDev) > rt.InvDevThreshold {
		return types.ActionReplace, fmt.Sprintf("inv dev changed by %.4f", utils.Abs(invDev-b.state.LastInvDev))
	}

	// Check order age
	if b.state.OldestOrderAgeMs > 0 {
		ageMs := nowMs - b.state.OldestOrderAgeMs
		ageSec := float64(ageMs) / 1000.0
		if ageSec > float64(rt.MaxOrderAgeSec) {
			return types.ActionReplace, fmt.Sprintf("orders aged %.1f sec", ageSec)
		}
	}

	// Check jittered refresh interval (backstop)
	if b.state.LastRefreshMs > 0 {
		refreshInterval := b.getJitteredRefreshInterval()
		timeSinceRefresh := nowMs - b.state.LastRefreshMs
		if timeSinceRefresh > refreshInterval {
			return types.ActionReplace, fmt.Sprintf("refresh interval %.1f sec", float64(timeSinceRefresh)/1000.0)
		}
	}

	return types.ActionKeep, "no replace conditions met"
}

// getJitteredRefreshInterval returns the refresh interval with jitter applied
func (b *Bot) getJitteredRefreshInterval() int64 {
	baseMs := int64(b.cfg.TradingConfig.RefreshBaseSec) * 1000
	jitterRange := float64(baseMs) * b.cfg.TradingConfig.RefreshJitterPct
	jitter := (rand.Float64()*2.0 - 1.0) * jitterRange // Random in [-jitterRange, +jitterRange]
	return baseMs + int64(jitter)
}

// executePlan executes the replace plan
func (b *Bot) executePlan(ctx context.Context, plan types.ReplacePlan, marketData *types.MarketData) error {
	symbol := b.cfg.TradingConfig.Symbol

	// Log the plan decision
	log.Printf("[%s] State=%s Action=%s Reason=%s Orders=%d",
		symbol, plan.State, plan.Action, plan.Reason, len(plan.Orders))

	switch plan.Action {
	case types.ActionCancelAll:
		// Cancel all orders and stop
		log.Printf("[%s] Cancelling all orders (paused state)", symbol)
		if err := b.exchange.CancelAllOrders(ctx, symbol); err != nil {
			return fmt.Errorf("failed to cancel all orders: %w", err)
		}
		return nil

	case types.ActionKeep:
		// Keep existing orders, do nothing
		log.Printf("[%s] Keeping existing orders", symbol)
		return nil

	case types.ActionReplace:
		// Cancel all existing orders
		log.Printf("[%s] Replacing orders: cancelling old orders", symbol)
		if err := b.exchange.CancelAllOrders(ctx, symbol); err != nil {
			return fmt.Errorf("failed to cancel orders before replace: %w", err)
		}

		// Clear all orders from Redis
		log.Printf("[%s] Clearing orders from Redis", symbol)
		if err := b.redis.ClearAllOrders(ctx, symbol); err != nil {
			log.Printf("[%s] WARNING: Failed to clear orders from Redis: %v", symbol, err)
			// Continue despite Redis error
		}

		// Convert OrderIntents to OrderRequests
		orderReqs := make([]*exchange.OrderRequest, len(plan.Orders))
		for i, orderIntent := range plan.Orders {
			side := "BUY"
			if orderIntent.Side == types.OrderSideSell {
				side = "SELL"
			}

			orderReqs[i] = &exchange.OrderRequest{
				Symbol:        symbol,
				Side:          side,
				Type:          "LIMIT",
				Price:         orderIntent.Price,
				Quantity:      orderIntent.Qty,
				ClientOrderID: orderIntent.Tag,
			}
		}

		// Place orders using batch API
		log.Printf("[%s] Placing %d new orders via batch API", symbol, len(orderReqs))
		batchResp, err := b.exchange.BatchPlaceOrders(ctx, orderReqs)
		if err != nil {
			return fmt.Errorf("failed to place batch orders: %w", err)
		}

		// Log results
		placedCount := len(batchResp.Orders)
		failedCount := len(batchResp.Errors)

		for _, order := range batchResp.Orders {
			log.Printf("[%s] Placed order %s: %s %.8f @ %.8f",
				symbol, order.OrderID, order.Side, order.Quantity, order.Price)

			// Note: Order will be saved to Redis automatically via WebSocket handler
			// when we receive the order update event from MEXC
		}

		for _, errMsg := range batchResp.Errors {
			log.Printf("[%s] %s", symbol, errMsg)
		}

		log.Printf("[%s] Batch order complete: %d placed, %d failed", symbol, placedCount, failedCount)

		if failedCount > 0 && placedCount == 0 {
			return fmt.Errorf("failed to place any orders: %d failures", failedCount)
		}

		return nil

	default:
		return fmt.Errorf("unknown action: %v", plan.Action)
	}
}

// placeOrdersConcurrently places multiple orders concurrently using goroutines
func (b *Bot) placeOrdersConcurrently(ctx context.Context, symbol string, orders []types.OrderIntent) []OrderResult {
	results := make([]OrderResult, len(orders))
	var wg sync.WaitGroup

	for i, orderIntent := range orders {
		wg.Add(1)
		go func(idx int, intent types.OrderIntent) {
			defer wg.Done()

			// Convert OrderIntent to OrderRequest
			side := "BUY"
			if intent.Side == types.OrderSideSell {
				side = "SELL"
			}

			orderReq := &exchange.OrderRequest{
				Symbol:        symbol,
				Side:          side,
				Type:          "LIMIT",
				Price:         intent.Price,
				Quantity:      intent.Qty,
				ClientOrderID: intent.Tag,
			}

			// Place order
			order, err := b.exchange.PlaceOrder(ctx, orderReq)
			if err != nil {
				results[idx] = OrderResult{
					Tag:   intent.Tag,
					Side:  side,
					Price: intent.Price,
					Qty:   intent.Qty,
					Error: err,
				}
				return
			}

			results[idx] = OrderResult{
				OrderID: order.OrderID,
				Tag:     intent.Tag,
				Side:    side,
				Price:   intent.Price,
				Qty:     intent.Qty,
				Error:   nil,
			}
		}(i, orderIntent)
	}

	// Wait for all goroutines to complete
	wg.Wait()
	return results
}
