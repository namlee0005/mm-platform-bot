package exchange

import (
	"context"
	"fmt"
	"log"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	"mm-platform-engine/internal/types"

	ccxt "github.com/ccxt/ccxt/go/v4"
	ccxtpro "github.com/ccxt/ccxt/go/v4/pro"
)

// cachedOrderState tracks the last known state of an order for dedup.
// Bybit WatchOrders returns full snapshots — without this, every unchanged
// order in the snapshot would re-emit OnOrderUpdate downstream.
type cachedOrderState struct {
	status    string
	filledQty float64
	seenAt    time.Time
	terminal  bool // true when closed/canceled — eligible for GC
}

// CCXTAdapter wraps CCXT exchange to implement Exchange interface
// Uses ccxt for REST API and ccxtpro for WebSocket
type CCXTAdapter struct {
	rest         ccxt.IExchange    // REST API client
	ws           ccxtpro.IExchange // WS client for creating/canceling orders
	wsOrders     ccxtpro.IExchange // Dedicated WS client for WatchOrders
	wsTrades     ccxtpro.IExchange // Dedicated WS client for WatchMyTrades
	exchangeName string
	symbol       string // CCXT format: BASE/QUOTE
	nativeSymbol string // Exchange format: BASEQUOTE
	handlers     UserStreamHandlers

	// WebSocket control
	ctx       context.Context
	cancel    context.CancelFunc
	wsRunning bool
	wsMu      sync.RWMutex

	// Global CCXT lock to prevent internal map race conditions in go-ccxt
	ccxtLock sync.Mutex

	// Order state dedup: track status + filledQty per orderID.
	// Only emit OnOrderUpdate/OnFill when state actually changes.
	orderStateMu    sync.Mutex
	orderStateCache map[string]*cachedOrderState
	lastGCTime      time.Time

	// Track filled order IDs permanently to prevent duplicate fill emissions
	// after GC clears orderStateCache. Bybit resends filled orders in snapshots.
	emittedFills map[string]bool

	// WatchMyTrades dedup: track seen trade IDs to prevent duplicate fill emissions.
	// WatchMyTrades returns snapshots — same trade can appear multiple times.
	seenTradeIDs map[string]bool

	// Cached market info
	market *ccxt.Market
}

// NewCCXTExchange creates exchange client using CCXT
// symbol should be in native format (e.g., "AMIUSDT") - will be converted to CCXT format
func NewCCXTExchange(exchangeName, apiKey, secret, symbol string, sandbox bool) (Exchange, error) {
	// Convert native symbol to CCXT format (AMIUSDT → AMI/USDT)
	ccxtSymbol := convertToCCXTSymbol(symbol)
	exName := strings.ToLower(exchangeName)

	// REST + single WS client for both WatchOrders and trading.
	var rest ccxt.IExchange
	var ws ccxtpro.IExchange

	switch exName {
	case "bybit":
		rest = ccxt.NewBybit(nil)
		ws = ccxtpro.NewBybit(nil)
		wsOrders = ccxtpro.NewBybit(nil)
		wsTrades = ccxtpro.NewBybit(nil)
	case "binance":
		rest = ccxt.NewBinance(nil)
		ws = ccxtpro.NewBinance(nil)
		wsOrders = ccxtpro.NewBinance(nil)
		wsTrades = ccxtpro.NewBinance(nil)
	case "okx":
		rest = ccxt.NewOkx(nil)
		ws = ccxtpro.NewOkx(nil)
		wsOrders = ccxtpro.NewOkx(nil)
		wsTrades = ccxtpro.NewOkx(nil)
	case "gate", "gateio":
		rest = ccxt.NewGate(nil)
		ws = ccxtpro.NewGate(nil)
		wsOrders = ccxtpro.NewGate(nil)
		wsTrades = ccxtpro.NewGate(nil)
	case "kucoin":
		rest = ccxt.NewKucoin(nil)
		ws = ccxtpro.NewKucoin(nil)
		wsOrders = ccxtpro.NewKucoin(nil)
		wsTrades = ccxtpro.NewKucoin(nil)
	case "mexc":
		rest = ccxt.NewMexc(nil)
		ws = ccxtpro.NewMexc(nil)
		wsOrders = ccxtpro.NewMexc(nil)
		wsTrades = ccxtpro.NewMexc(nil)
	case "htx", "huobi":
		rest = ccxt.NewHtx(nil)
		ws = ccxtpro.NewHtx(nil)
		wsOrders = ccxtpro.NewHtx(nil)
		wsTrades = ccxtpro.NewHtx(nil)
	case "bitget":
		rest = ccxt.NewBitget(nil)
		ws = ccxtpro.NewBitget(nil)
		wsOrders = ccxtpro.NewBitget(nil)
		wsTrades = ccxtpro.NewBitget(nil)
	default:
		return nil, fmt.Errorf("unsupported exchange: %s", exchangeName)
	}

	// Set credentials
	for _, client := range []ccxt.IExchange{rest, ws, wsOrders, wsTrades} {
		client.SetApiKey(apiKey)
		client.SetSecret(secret)
		if sandbox {
			client.SetSandboxMode(true)
		}
	}

	adapter := &CCXTAdapter{
		rest:         rest,
		ws:           ws,
		wsOrders:     wsOrders,
		wsTrades:     wsTrades,
		exchangeName: exName,
		symbol:       ccxtSymbol,
		nativeSymbol: symbol,
	}

	return adapter, nil
}

// convertToCCXTSymbol converts native symbol format to CCXT format
// AMIUSDT → AMI/USDT, BTCUSDT → BTC/USDT
func convertToCCXTSymbol(symbol string) string {
	// If already in CCXT format
	if strings.Contains(symbol, "/") {
		return symbol
	}

	// Common quote currencies (longest first to avoid partial matches)
	quotes := []string{"USDT", "USDC", "BUSD", "USD", "EUR", "BTC", "ETH", "BNB"}

	for _, quote := range quotes {
		if strings.HasSuffix(symbol, quote) {
			base := strings.TrimSuffix(symbol, quote)
			return base + "/" + quote
		}
	}

	// If no match, return as-is
	return symbol
}

// Start initializes the exchange connection
func (c *CCXTAdapter) Start(ctx context.Context) error {
	log.Printf("[CCXT:%s] Starting exchange adapter for %s", c.exchangeName, c.symbol)

	// Load markets for REST with retry (CCXT may panic internally)
	var lastErr error
	for attempt := 1; attempt <= 3; attempt++ {
		lastErr = func() (err error) {
			defer func() {
				if r := recover(); r != nil {
					err = fmt.Errorf("LoadMarkets panic: %v", r)
				}
			}()
			if _, e := c.rest.LoadMarkets(); e != nil {
				return fmt.Errorf("LoadMarkets error: %w", e)
			}
			// Verify markets actually loaded by calling Market()
			c.rest.Market(c.symbol)
			return nil
		}()
		if lastErr == nil {
			break
		}
		log.Printf("[CCXT:%s] LoadMarkets attempt %d failed: %v", c.exchangeName, attempt, lastErr)
		time.Sleep(2 * time.Second)
	}
	if lastErr != nil {
		return fmt.Errorf("failed to load markets after 3 attempts: %w", lastErr)
	}

	log.Printf("[CCXT:%s] Markets loaded successfully", c.exchangeName)
	return nil
}

// Stop closes the exchange connection
func (c *CCXTAdapter) Stop(ctx context.Context) error {
	log.Printf("[CCXT:%s] Stopping exchange adapter", c.exchangeName)

	c.wsMu.Lock()
	if c.cancel != nil {
		c.cancel()
	}
	c.wsRunning = false
	c.wsMu.Unlock()

	return nil
}

// GetExchangeInfo retrieves market information
func (c *CCXTAdapter) GetExchangeInfo(ctx context.Context, symbol string) (info *ExchangeInfo, retErr error) {
	defer func() {
		if r := recover(); r != nil {
			retErr = fmt.Errorf("GetExchangeInfo panic (markets may not be loaded): %v", r)
		}
	}()

	ccxtSymbol := convertToCCXTSymbol(symbol)

	// Ensure markets are loaded before calling Market()
	if _, err := c.rest.LoadMarkets(); err != nil {
		return nil, fmt.Errorf("failed to load markets: %w", err)
	}

	marketRaw := c.rest.Market(ccxtSymbol)
	if marketRaw == nil {
		return nil, fmt.Errorf("symbol %s not found", ccxtSymbol)
	}

	// Convert to typed Market struct
	market := ccxt.NewMarket(marketRaw)
	if market.Symbol == nil || *market.Symbol == "" {
		return nil, fmt.Errorf("symbol %s not found", ccxtSymbol)
	}

	// Get precision - CCXT uses TICK_SIZE mode for most exchanges
	// This means Precision.Price/Amount are floats representing the actual tick/step size
	// NOT decimal places (integers)
	pricePrecision := 8
	amountPrecision := 8

	// DEBUG: Log raw precision values from CCXT
	if market.Precision != nil {
		log.Printf("[CCXT:%s] DEBUG Precision raw - Price: %v, Amount: %v",
			c.exchangeName, market.Precision.Price, market.Precision.Amount)
		if market.Precision.Price != nil {
			rawPrecision := *market.Precision.Price
			log.Printf("[CCXT:%s] DEBUG Price precision raw value: %v (type: %T, as int: %d)",
				c.exchangeName, rawPrecision, rawPrecision, int(rawPrecision))
			// Check if precision is TICK_SIZE format (< 1) or DECIMAL_PLACES format (>= 1)
			if rawPrecision >= 1 {
				// DECIMAL_PLACES mode: value is number of decimal places
				pricePrecision = int(rawPrecision)
			} else if rawPrecision > 0 {
				// TICK_SIZE mode: value is the actual tick size (e.g., 0.00000001)
				// Convert to decimal places: -log10(tickSize)
				decimalPlaces := -math.Log10(rawPrecision)
				pricePrecision = int(math.Round(decimalPlaces))
				log.Printf("[CCXT:%s] TICK_SIZE mode detected: tickSize=%v -> decimalPlaces=%d",
					c.exchangeName, rawPrecision, pricePrecision)
			}
		}
		if market.Precision.Amount != nil {
			rawPrecision := *market.Precision.Amount
			log.Printf("[CCXT:%s] DEBUG Amount precision raw value: %v (type: %T, as int: %d)",
				c.exchangeName, rawPrecision, rawPrecision, int(rawPrecision))
			// Check if precision is TICK_SIZE format (< 1) or DECIMAL_PLACES format (>= 1)
			if rawPrecision >= 1 {
				// DECIMAL_PLACES mode: value is number of decimal places
				amountPrecision = int(rawPrecision)
			} else if rawPrecision > 0 {
				// TICK_SIZE mode: value is the actual step size
				// Convert to decimal places: -log10(stepSize)
				decimalPlaces := -math.Log10(rawPrecision)
				amountPrecision = int(math.Round(decimalPlaces))
				log.Printf("[CCXT:%s] TICK_SIZE mode detected: stepSize=%v -> decimalPlaces=%d",
					c.exchangeName, rawPrecision, amountPrecision)
			}
		}
	}
	log.Printf("[CCXT:%s] Final precision - Price: %d decimals, Amount: %d decimals",
		c.exchangeName, pricePrecision, amountPrecision)

	// Build filters
	var filters []Filter

	// Min notional from limits
	if market.Limits != nil && market.Limits.Cost.Min != nil {
		filters = append(filters, Filter{
			FilterType:  "PRICE_FILTER",
			MinNotional: fmt.Sprintf("%f", *market.Limits.Cost.Min),
		})
	}

	// Max quantity from limits
	if market.Limits != nil && market.Limits.Amount.Max != nil {
		filters = append(filters, Filter{
			FilterType: "LOT_SIZE",
			MaxQty:     fmt.Sprintf("%f", *market.Limits.Amount.Max),
		})
	}

	return &ExchangeInfo{
		Symbols: []Symbol{{
			Symbol:              symbol,
			BaseAsset:           derefString(market.BaseCurrency),
			QuoteAsset:          derefString(market.QuoteCurrency),
			BaseAssetPrecision:  amountPrecision,
			QuotePrecision:      pricePrecision,
			QuoteAssetPrecision: pricePrecision,
			Filters:             filters,
		}},
	}, nil
}

// GetDepth retrieves order book depth
func (c *CCXTAdapter) GetDepth(ctx context.Context, symbol string) (*Depth, error) {
	ccxtSymbol := convertToCCXTSymbol(symbol)

	orderbook, err := c.rest.FetchOrderBook(ccxtSymbol, ccxt.WithFetchOrderBookLimit(20))
	if err != nil {
		return nil, fmt.Errorf("failed to fetch order book: %w", err)
	}

	bids := make([][]string, len(orderbook.Bids))
	asks := make([][]string, len(orderbook.Asks))

	for i, bid := range orderbook.Bids {
		bids[i] = []string{
			strconv.FormatFloat(bid[0], 'f', -1, 64),
			strconv.FormatFloat(bid[1], 'f', -1, 64),
		}
	}

	for i, ask := range orderbook.Asks {
		asks[i] = []string{
			strconv.FormatFloat(ask[0], 'f', -1, 64),
			strconv.FormatFloat(ask[1], 'f', -1, 64),
		}
	}

	return &Depth{Bids: bids, Asks: asks}, nil
}

// GetAccount retrieves account balances
func (c *CCXTAdapter) GetAccount(ctx context.Context) (*Account, error) {
	balance, err := c.rest.FetchBalance()
	if err != nil {
		return nil, fmt.Errorf("failed to fetch balance: %w", err)
	}

	var balances []Balance
	for asset, freePtr := range balance.Free {
		free := derefFloat(freePtr)
		locked := 0.0
		if usedPtr, ok := balance.Used[asset]; ok {
			locked = derefFloat(usedPtr)
		}

		// Only include non-zero balances
		if free > 0 || locked > 0 {
			balances = append(balances, Balance{
				Asset:  asset,
				Free:   free,
				Locked: locked,
			})
		}
	}

	return &Account{Balances: balances}, nil
}

// PlaceOrder places a single order
func (c *CCXTAdapter) PlaceOrder(ctx context.Context, order *OrderRequest) (*Order, error) {
	ccxtSymbol := convertToCCXTSymbol(order.Symbol)

	orderType := strings.ToLower(order.Type)
	side := strings.ToLower(order.Side)

	var result ccxt.Order
	var err error

	if orderType == "limit" {
		opts := []ccxt.CreateOrderOptions{
			ccxt.WithCreateOrderPrice(order.Price),
		}
		if order.TimeInForce != "" {
			opts = append(opts, ccxt.WithCreateOrderParams(map[string]interface{}{
				"timeInForce": order.TimeInForce,
			}))
		}
		result, err = c.rest.CreateOrder(
			ccxtSymbol,
			"limit",
			side,
			order.Quantity,
			opts...,
		)
	} else {
		result, err = c.rest.CreateOrder(
			ccxtSymbol,
			"market",
			side,
			order.Quantity,
		)
	}

	if err != nil {
		return nil, fmt.Errorf("failed to place order: %w", err)
	}

	// Use request values for Price/Qty/ClientOrderID as CCXT may not return them for some exchanges (Bybit)
	// This ensures we always have the values we requested
	clientOrderID := derefString(result.ClientOrderId)
	if clientOrderID == "" {
		clientOrderID = order.ClientOrderID
	}
	price := derefFloat(result.Price)
	if price == 0 {
		price = order.Price
	}
	qty := derefFloat(result.Amount)
	if qty == 0 {
		qty = order.Quantity
	}

	return &Order{
		OrderID:       derefString(result.Id),
		ClientOrderID: clientOrderID,
		Symbol:        order.Symbol,
		Side:          strings.ToUpper(side),
		Type:          strings.ToUpper(orderType),
		Price:         price,
		Quantity:      qty,
		ExecutedQty:   derefFloat(result.Filled),
		Status:        mapCCXTStatus(result.Status),
	}, nil
}

// BatchPlaceOrders places multiple orders
func (c *CCXTAdapter) BatchPlaceOrders(ctx context.Context, orders []*OrderRequest) (*BatchOrderResponse, error) {
	response := &BatchOrderResponse{
		Orders: make([]*Order, 0, len(orders)),
		Errors: make([]string, 0),
	}

	// Place orders sequentially with small delay to respect rate limits
	for i, order := range orders {
		result, err := c.PlaceOrder(ctx, order)
		if err != nil {
			response.Errors = append(response.Errors, fmt.Sprintf("order %d: %v", i, err))
			continue
		}
		response.Orders = append(response.Orders, result)

		// Small delay between orders
		if i < len(orders)-1 {
			time.Sleep(50 * time.Millisecond)
		}
	}

	return response, nil
}

// isOrderGoneError checks if an error string indicates the order no longer exists.
// Covers Bybit ("OrderNotFound", "Order does not exist"), MEXC ("Order cancelled", code -2011),
// and Gate/other exchanges with similar patterns.
func isOrderGoneError(errStr string) bool {
	lower := strings.ToLower(errStr)
	return strings.Contains(lower, "ordernotfound") ||
		strings.Contains(lower, "order does not exist") ||
		strings.Contains(lower, "order cancelled") ||
		strings.Contains(lower, "order canceled") ||
		strings.Contains(errStr, "-2011")
}

// CancelOrder cancels a single order
func (c *CCXTAdapter) CancelOrder(ctx context.Context, symbol, orderID string) (retErr error) {
	ccxtSymbol := convertToCCXTSymbol(symbol)

	defer func() {
		if r := recover(); r != nil {
			errStr := fmt.Sprintf("%v", r)
			if isOrderGoneError(errStr) {
				log.Printf("[CCXT:%s] CancelOrder %s: order already gone (ignored)", c.exchangeName, orderID)
				retErr = nil
				return
			}
			retErr = fmt.Errorf("cancel order panic: %v", r)
		}
	}()

	_, err := c.rest.CancelOrder(orderID, ccxt.WithCancelOrderSymbol(ccxtSymbol))
	if err != nil {
		if isOrderGoneError(err.Error()) {
			log.Printf("[CCXT:%s] CancelOrder %s: order already gone (ignored)", c.exchangeName, orderID)
			return nil
		}
		return fmt.Errorf("failed to cancel order: %w", err)
	}

	return nil
}

// CancelAllOrders cancels all open orders for a symbol
func (c *CCXTAdapter) CancelAllOrders(ctx context.Context, symbol string) error {
	ccxtSymbol := convertToCCXTSymbol(symbol)

	_, err := c.rest.CancelAllOrders(ccxt.WithCancelAllOrdersSymbol(ccxtSymbol))
	if err != nil {
		// Some exchanges don't support cancel all, fall back to individual cancels
		orders, fetchErr := c.GetOpenOrders(ctx, symbol)
		if fetchErr != nil {
			return fmt.Errorf("failed to cancel all orders: %w (fetch error: %v)", err, fetchErr)
		}

		for _, order := range orders {
			if cancelErr := c.CancelOrder(ctx, symbol, order.OrderID); cancelErr != nil {
				log.Printf("[CCXT:%s] Failed to cancel order %s: %v", c.exchangeName, order.OrderID, cancelErr)
			}
		}
	}

	return nil
}

// GetOpenOrders retrieves all open orders for a symbol
func (c *CCXTAdapter) GetOpenOrders(ctx context.Context, symbol string) ([]*Order, error) {
	ccxtSymbol := convertToCCXTSymbol(symbol)

	orders, err := c.rest.FetchOpenOrders(ccxt.WithFetchOpenOrdersSymbol(ccxtSymbol), ccxt.WithFetchOpenOrdersLimit(100))
	if err != nil {
		return nil, fmt.Errorf("failed to fetch open orders: %w", err)
	}

	result := make([]*Order, len(orders))
	for i, o := range orders {
		result[i] = &Order{
			OrderID:       derefString(o.Id),
			ClientOrderID: derefString(o.ClientOrderId),
			Symbol:        symbol,
			Side:          strings.ToUpper(derefString(o.Side)),
			Type:          strings.ToUpper(derefString(o.Type)),
			Price:         derefFloat(o.Price),
			Quantity:      derefFloat(o.Amount),
			ExecutedQty:   derefFloat(o.Filled),
			Status:        mapCCXTStatus(o.Status),
		}
	}

	return result, nil
}

// FetchClosedOrders retrieves recently closed orders (filled/canceled) via REST API.
func (c *CCXTAdapter) FetchClosedOrders(ctx context.Context, symbol string, since time.Time, limit int) ([]*Order, error) {
	ccxtSymbol := convertToCCXTSymbol(symbol)
	now := time.Now()

	// Bybit /v5/execution/list only supports 7-day windows.
	// Split the range into 7-day windows and paginate within each.
	const windowDays = 7
	const pageSize int64 = 100
	const maxPagesPerWindow = 50

	var result []*Order
	windowStart := since

	for windowStart.Before(now) {
		windowEnd := windowStart.Add(windowDays * 24 * time.Hour)
		if windowEnd.After(now) {
			windowEnd = now
		}

		startMs := windowStart.UnixMilli()
		endMs := windowEnd.UnixMilli()

		for page := 0; page < maxPagesPerWindow; page++ {
			params := map[string]interface{}{
				"endTime": endMs,
			}
			opts := []ccxt.FetchMyTradesOptions{
				ccxt.WithFetchMyTradesSymbol(ccxtSymbol),
				ccxt.WithFetchMyTradesSince(startMs),
				ccxt.WithFetchMyTradesLimit(pageSize),
				ccxt.WithFetchMyTradesParams(params),
			}

			trades, err := c.rest.FetchMyTrades(opts...)
			if err != nil {
				return nil, fmt.Errorf("failed to fetch trades (window %s-%s): %w",
					windowStart.Format("01-02"), windowEnd.Format("01-02"), err)
			}

			if len(trades) == 0 {
				break
			}

			for _, t := range trades {
				tradeTime := time.Now()
				if t.Timestamp != nil {
					tradeTime = time.UnixMilli(*t.Timestamp)
				}
				result = append(result, &Order{
					OrderID:            derefString(t.Order),
					ClientOrderID:      derefString(t.Id), // trade ID for dedup
					Symbol:             symbol,
					Side:               strings.ToUpper(derefString(t.Side)),
					Type:               "LIMIT",
					Price:              derefFloat(t.Price),
					Quantity:           derefFloat(t.Amount),
					ExecutedQty:        derefFloat(t.Amount),
					CumulativeQuoteQty: derefFloat(t.Cost),
					Status:             "FILLED",
					Timestamp:          tradeTime,
				})
			}

			// Move cursor past the last trade's timestamp
			lastTrade := trades[len(trades)-1]
			if lastTrade.Timestamp != nil {
				startMs = *lastTrade.Timestamp + 1
			} else {
				break
			}

			if int64(len(trades)) < pageSize {
				break
			}

			time.Sleep(100 * time.Millisecond) // rate limit
		}

		windowStart = windowEnd
	}

	log.Printf("[CCXT:%s] FetchMyTrades %s → %s returned %d trades",
		c.exchangeName, since.Format("2006-01-02"), now.Format("2006-01-02"), len(result))

	return result, nil
}

// GetTicker returns the last trade price for a symbol
func (c *CCXTAdapter) GetTicker(ctx context.Context, symbol string) (float64, error) {
	ccxtSymbol := convertToCCXTSymbol(symbol)

	ticker, err := c.rest.FetchTicker(ccxtSymbol)
	if err != nil {
		return 0, fmt.Errorf("failed to fetch ticker: %w", err)
	}

	return derefFloat(ticker.Last), nil
}

// GetRecentTrades returns recent market trades
func (c *CCXTAdapter) GetRecentTrades(ctx context.Context, symbol string, limit int) ([]Trade, error) {
	ccxtSymbol := convertToCCXTSymbol(symbol)

	trades, err := c.rest.FetchTrades(ccxtSymbol, ccxt.WithFetchTradesLimit(int64(limit)))
	if err != nil {
		return nil, fmt.Errorf("failed to fetch trades: %w", err)
	}

	result := make([]Trade, len(trades))
	for i, t := range trades {
		result[i] = Trade{
			Price:        derefFloat(t.Price),
			Quantity:     derefFloat(t.Amount),
			Timestamp:    time.UnixMilli(derefInt64(t.Timestamp)),
			IsBuyerMaker: derefString(t.Side) == "sell", // If taker is selling, buyer was maker
		}
	}

	return result, nil
}

// SubscribeUserStream subscribes to user data stream via WebSocket
func (c *CCXTAdapter) SubscribeUserStream(ctx context.Context, handlers UserStreamHandlers) error {
	c.wsMu.Lock()
	if c.wsRunning {
		c.wsMu.Unlock()
		return nil // Already running
	}
	c.handlers = handlers
	c.ctx, c.cancel = context.WithCancel(ctx)
	c.wsRunning = true
	c.orderStateCache = make(map[string]*cachedOrderState)
	c.emittedFills = make(map[string]bool)
	c.seenTradeIDs = make(map[string]bool)
	c.wsMu.Unlock()

	log.Printf("[CCXT:%s] Starting WebSocket streams for %s", c.exchangeName, c.symbol)

	// Load markets for WS client
	c.ws.LoadMarkets()
	c.wsOrders.LoadMarkets()
	c.wsTrades.LoadMarkets()

	// WatchOrders via WS for order status updates
	go c.watchOrders()

	// WatchMyTrades via WS for real-time fill events (Bybit "execution" topic)
	go c.watchMyTrades()

	return nil
}

// watchOrders watches for order updates via WebSocket
func (c *CCXTAdapter) watchOrders() {
	log.Printf("[CCXT:%s] Starting order watcher for %s", c.exchangeName, c.symbol)

	for {
		select {
		case <-c.ctx.Done():
			log.Printf("[CCXT:%s] Order watcher stopped", c.exchangeName)
			return
		default:
		}

		func() {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("[CCXT:%s] watchOrders panic (CCXT internal race): %v — restarting in 1s", c.exchangeName, r)
					time.Sleep(time.Second)
				}
			}()
			c.ccxtLock.Lock()
			orders, err := c.wsOrders.WatchOrders(ccxt.WithWatchOrdersSymbol(c.symbol))
			c.ccxtLock.Unlock()
			if err != nil {
				log.Printf("[CCXT:%s] WatchOrders error: %v", c.exchangeName, err)
				if c.handlers.OnError != nil {
					c.handlers.OnError(err)
				}
				time.Sleep(time.Second)
				return
			}

			for _, order := range orders {
				orderID := derefString(order.Id)
				rawStatus := derefString(order.Status)
				filledQty := derefFloat(order.Filled)

				// Dedup: only process orders whose state actually changed.
				// Bybit sends full snapshots — skip unchanged orders.
				changed, _ := c.updateOrderState(orderID, rawStatus, filledQty)
				if !changed {
					continue
				}

				log.Printf("[CCXT:%s] WatchOrders update: order=%s status=%s filled=%.6f price=%.8f",
					c.exchangeName, orderID, rawStatus, filledQty, derefFloat(order.Price))

				// Use average fill price when available, fallback to limit price
				fillPrice := derefFloat(order.Average)
				if fillPrice == 0 {
					fillPrice = derefFloat(order.Price)
				}

				// NOTE: OnFill is NOT emitted here — fills come from watchMyTrades()
				// which subscribes to Bybit "execution" topic for real-time fill events.
				// WatchOrders ("order" topic) can delay fills by minutes on Bybit.

				// Emit OnOrderUpdate only when state changed
				if c.handlers.OnOrderUpdate != nil {
					mappedStatus := mapCCXTStatus(order.Status)
					// Bybit sends status=open for partial fills — remap to PARTIALLY_FILLED
					if mappedStatus == "NEW" && filledQty > 0 && filledQty < derefFloat(order.Amount) {
						mappedStatus = "PARTIALLY_FILLED"
					}
					c.handlers.OnOrderUpdate(&types.OrderEvent{
						OrderID:            orderID,
						ClientOrderID:      derefString(order.ClientOrderId),
						Symbol:             c.nativeSymbol,
						Side:               strings.ToUpper(derefString(order.Side)),
						Type:               strings.ToUpper(derefString(order.Type)),
						Status:             mappedStatus,
						Price:              fillPrice,
						Quantity:           derefFloat(order.Amount),
						ExecutedQty:        derefFloat(order.Filled),
						CumulativeQuoteQty: derefFloat(order.Cost),
						Timestamp:          time.UnixMilli(derefInt64(order.Timestamp)),
					})
				}
			}
		}()
	}
}

// watchMyTrades watches for trade executions via WebSocket ("execution" topic).
// This is the primary source for fill events — much faster than WatchOrders on Bybit.
func (c *CCXTAdapter) watchMyTrades() {
	log.Printf("[CCXT:%s] Starting my trades watcher for %s", c.exchangeName, c.symbol)

	for {
		select {
		case <-c.ctx.Done():
			log.Printf("[CCXT:%s] My trades watcher stopped", c.exchangeName)
			return
		default:
		}

		func() {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("[CCXT:%s] watchMyTrades panic: %v — restarting in 1s", c.exchangeName, r)
					time.Sleep(time.Second)
				}
			}()

			c.ccxtLock.Lock()
			trades, err := c.wsTrades.WatchMyTrades(ccxt.WithWatchMyTradesSymbol(c.symbol))
			c.ccxtLock.Unlock()
			if err != nil {
				log.Printf("[CCXT:%s] WatchMyTrades error: %v", c.exchangeName, err)
				if c.handlers.OnError != nil {
					c.handlers.OnError(err)
				}
				time.Sleep(time.Second)
				return
			}

			for _, trade := range trades {
				tradeID := derefString(trade.Id)
				if tradeID == "" {
					continue
				}

				// Dedup: WatchMyTrades can return snapshots with previously seen trades
				c.orderStateMu.Lock()
				if c.seenTradeIDs[tradeID] {
					c.orderStateMu.Unlock()
					continue
				}
				c.seenTradeIDs[tradeID] = true
				c.orderStateMu.Unlock()

				orderID := derefString(trade.Order)
				side := strings.ToUpper(derefString(trade.Side))
				price := derefFloat(trade.Price)
				qty := derefFloat(trade.Amount)
				var commission float64
				if trade.Fee.Cost != nil {
					commission = *trade.Fee.Cost
				}
				ts := time.Now()
				if trade.Timestamp != nil {
					ts = time.UnixMilli(*trade.Timestamp)
				}

				log.Printf("[CCXT:%s] Trade fill: %s %s @ %.8f x %.6f (order=%s trade=%s)",
					c.exchangeName, side, c.nativeSymbol, price, qty, orderID, tradeID)

				if c.handlers.OnFill != nil {
					c.handlers.OnFill(&types.FillEvent{
						OrderID:    orderID,
						Symbol:     c.nativeSymbol,
						Side:       side,
						Price:      price,
						Quantity:   qty,
						Commission: commission,
						TradeID:    tradeID,
						Timestamp:  ts,
					})
				}
			}
		}()
	}
}

// updateOrderState checks if an order's state has changed since last seen.
// Returns (changed bool, fillDelta float64).
// changed=true means status or filledQty changed → should emit OnOrderUpdate.
// fillDelta>0 means new fill qty detected → should emit OnFill.
func (c *CCXTAdapter) updateOrderState(orderID, rawStatus string, filledQty float64) (changed bool, fillDelta float64) {
	isTerminal := rawStatus == "closed" || rawStatus == "canceled" || rawStatus == "cancelled" || rawStatus == "expired" || rawStatus == "rejected"
	now := time.Now()
	c.orderStateMu.Lock()
	defer c.orderStateMu.Unlock()

	// GC terminal entries every 60s — remove entries not seen for 5 minutes.
	// Terminal entries don't refresh seenAt, so they expire naturally even if
	// the exchange keeps resending them (e.g. MEXC keeps canceled orders in WS cache).
	if now.Sub(c.lastGCTime) > 60*time.Second {
		cutoff := now.Add(-5 * time.Minute)
		for id, st := range c.orderStateCache {
			if st.terminal && st.seenAt.Before(cutoff) {
				delete(c.orderStateCache, id)
			}
		}
		c.lastGCTime = now
	}

	prev, exists := c.orderStateCache[orderID]
	if !exists {
		// First time seeing this order.
		// If it arrives already terminal (e.g. MEXC resending old canceled order
		// after GC cleared it), still record it but don't emit — it was already
		// processed once before GC. We detect this by checking if filledQty==0
		// and status is terminal, which means it's a stale replay, not a fresh event.
		// Fresh terminal events (actual fill/cancel) come from orders we already
		// track as "open" in the cache, so they hit the status-change path below.
		c.orderStateCache[orderID] = &cachedOrderState{
			status:    rawStatus,
			filledQty: filledQty,
			seenAt:    now,
			terminal:  isTerminal,
		}
		if isTerminal && filledQty <= 0 {
			// Terminal order with no fill → stale cancel replay. Don't emit.
			return false, 0
		}
		if isTerminal && filledQty > 0 {
			// Terminal with fill — could be real (Bybit race) or GC replay.
			// Check emittedFills to prevent duplicate emissions.
			if c.emittedFills[orderID] {
				return false, 0 // already emitted this fill before GC cleared it
			}
			c.emittedFills[orderID] = true
			return true, filledQty
		}
		return true, filledQty
	}

	// Already known order — only refresh seenAt for non-terminal orders.
	// Terminal orders keep their original seenAt so GC can expire them.
	if !prev.terminal {
		prev.seenAt = now
	}

	// Check fill delta
	if filledQty > prev.filledQty {
		fillDelta = filledQty - prev.filledQty
		prev.filledQty = filledQty
		changed = true
		// Mark as emitted so GC replay won't re-emit
		if isTerminal {
			c.emittedFills[orderID] = true
		}
	}

	// Check status change
	if rawStatus != prev.status {
		prev.status = rawStatus
		prev.terminal = isTerminal
		changed = true
	}

	return changed, fillDelta
}

// mapCCXTStatus maps CCXT order status to standard status
func mapCCXTStatus(status *string) string {
	if status == nil {
		return "UNKNOWN"
	}
	switch strings.ToLower(*status) {
	case "open":
		return "NEW"
	case "closed":
		return "FILLED"
	case "canceled", "cancelled":
		return "CANCELED"
	case "expired":
		return "EXPIRED"
	case "rejected":
		return "REJECTED"
	default:
		return strings.ToUpper(*status)
	}
}

// Helper functions for pointer dereferencing
func derefString(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func derefFloat(p *float64) float64 {
	if p == nil {
		return 0
	}
	return *p
}

func derefInt(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

func derefInt64(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

// SupportedExchanges returns list of supported exchanges
func SupportedExchanges() []string {
	return []string{
		"bybit",
		"binance",
		"okx",
		"gate",
		"kucoin",
		"mexc",
		"htx",
		"bitget",
	}
}

// --- WebSocket Order Management (WSOrderExchange interface) ---

// HasWsOrderSupport returns true if the exchange supports WS order placement.
func (c *CCXTAdapter) HasWsOrderSupport() bool {
	switch c.exchangeName {
	case "bybit", "binance", "gate", "gateio", "kucoin", "okx":
		return true
	case "mexc":
		return false // MEXC doesn't support CreateOrderWs
	default:
		return false
	}
}

// HasWsEditSupport returns true if the exchange supports WS order editing.
func (c *CCXTAdapter) HasWsEditSupport() bool {
	switch c.exchangeName {
	case "bybit", "binance", "gate", "gateio", "kucoin", "okx":
		return true
	case "mexc":
		return true // MEXC supports EditOrderWs but not CreateOrderWs
	default:
		return false
	}
}

// PlaceOrderWs places an order via WebSocket.
func (c *CCXTAdapter) PlaceOrderWs(ctx context.Context, order *OrderRequest) (*Order, error) {
	ccxtSymbol := convertToCCXTSymbol(order.Symbol)
	orderType := strings.ToLower(order.Type)
	side := strings.ToLower(order.Side)

	var result ccxt.Order
	var err error

	if orderType == "limit" {
		opts := []ccxt.CreateOrderWsOptions{
			ccxt.WithCreateOrderWsPrice(order.Price),
		}
		if order.TimeInForce != "" {
			opts = append(opts, ccxt.WithCreateOrderWsParams(map[string]interface{}{
				"timeInForce": order.TimeInForce,
			}))
		}
		c.ccxtLock.Lock()
		result, err = c.ws.CreateOrderWs(
			ccxtSymbol,
			"limit",
			side,
			order.Quantity,
			opts...,
		)
	} else {
		c.ccxtLock.Lock()
		result, err = c.ws.CreateOrderWs(
			ccxtSymbol,
			"market",
			side,
			order.Quantity,
		)
	}
	if err != nil {
		return nil, fmt.Errorf("WS place order failed: %w", err)
	}

	clientOrderID := derefString(result.ClientOrderId)
	if clientOrderID == "" {
		clientOrderID = order.ClientOrderID
	}
	price := derefFloat(result.Price)
	if price == 0 {
		price = order.Price
	}
	qty := derefFloat(result.Amount)
	if qty == 0 {
		qty = order.Quantity
	}

	return &Order{
		OrderID:       derefString(result.Id),
		ClientOrderID: clientOrderID,
		Symbol:        order.Symbol,
		Side:          strings.ToUpper(side),
		Type:          strings.ToUpper(orderType),
		Price:         price,
		Quantity:      qty,
		ExecutedQty:   derefFloat(result.Filled),
		Status:        mapCCXTStatus(result.Status),
	}, nil
}

// EditOrderWs amends an existing order via WebSocket.
func (c *CCXTAdapter) EditOrderWs(ctx context.Context, orderID, symbol string, newPrice, newQty float64) (*Order, error) {
	ccxtSymbol := convertToCCXTSymbol(symbol)

	result, err := c.ws.EditOrderWs(
		orderID,
		ccxtSymbol,
		"limit",
		"",
		ccxt.WithEditOrderWsAmount(newQty),
		ccxt.WithEditOrderWsPrice(newPrice),
	)
	if err != nil {
		return nil, fmt.Errorf("WS edit order failed: %w", err)
	}

	return &Order{
		OrderID:       derefString(result.Id),
		ClientOrderID: derefString(result.ClientOrderId),
		Symbol:        symbol,
		Side:          strings.ToUpper(derefString(result.Side)),
		Type:          "LIMIT",
		Price:         derefFloat(result.Price),
		Quantity:      derefFloat(result.Amount),
		ExecutedQty:   derefFloat(result.Filled),
		Status:        mapCCXTStatus(result.Status),
	}, nil
}

// CancelOrderWs cancels an order via WebSocket.
func (c *CCXTAdapter) CancelOrderWs(ctx context.Context, symbol, orderID string) error {
	c.ccxtLock.Lock()
	_, err := c.ws.CancelOrderWs(
		orderID,
		ccxt.WithCancelOrderWsSymbol(convertToCCXTSymbol(symbol)),
	)
	if err != nil {
		errStr := err.Error()
		if strings.Contains(errStr, "OrderNotFound") || strings.Contains(errStr, "does not exist") {
			log.Printf("[CCXT:%s] CancelOrderWs %s: order already gone (ignored)", c.exchangeName, orderID)
			return nil
		}
		return fmt.Errorf("WS cancel order failed: %w", err)
	}
	return nil
}

// CancelAllOrdersWs cancels all orders for a symbol via WebSocket.
func (c *CCXTAdapter) CancelAllOrdersWs(ctx context.Context, symbol string) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("WS cancel all orders not supported: %v", r)
		}
	}()
	_, err = c.ws.CancelAllOrdersWs(
		ccxt.WithCancelAllOrdersWsSymbol(convertToCCXTSymbol(symbol)),
	)
	if err != nil {
		return fmt.Errorf("WS cancel all orders failed: %w", err)
	}
	return nil
}
