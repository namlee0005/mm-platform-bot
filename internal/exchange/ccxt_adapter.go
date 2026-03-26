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

// CCXTAdapter wraps CCXT exchange to implement Exchange interface
// Uses ccxt for REST API and ccxtpro for WebSocket
type CCXTAdapter struct {
	rest         ccxt.IExchange    // REST API client
	ws           ccxtpro.IExchange // WebSocket client
	exchangeName string
	symbol       string // CCXT format: BASE/QUOTE
	nativeSymbol string // Exchange format: BASEQUOTE
	handlers     UserStreamHandlers

	// WebSocket control
	ctx       context.Context
	cancel    context.CancelFunc
	wsRunning bool
	wsMu      sync.RWMutex

	// Fill deduplication: track cumulative filled qty per orderID
	// watchOrders uses this as fallback when watchMyTrades doesn't deliver (e.g. Bybit)
	filledQtyMu      sync.Mutex
	filledQtyByOrder map[string]float64

	// Cached market info
	market *ccxt.Market
}

// NewCCXTExchange creates exchange client using CCXT
// symbol should be in native format (e.g., "AMIUSDT") - will be converted to CCXT format
func NewCCXTExchange(exchangeName, apiKey, secret, symbol string, sandbox bool) (Exchange, error) {
	// Convert native symbol to CCXT format (AMIUSDT → AMI/USDT)
	ccxtSymbol := convertToCCXTSymbol(symbol)
	exName := strings.ToLower(exchangeName)

	// Create REST and WS clients
	var rest ccxt.IExchange
	var ws ccxtpro.IExchange

	switch exName {
	case "bybit":
		rest = ccxt.NewBybit(nil)
		ws = ccxtpro.NewBybit(nil)
	case "binance":
		rest = ccxt.NewBinance(nil)
		ws = ccxtpro.NewBinance(nil)
	case "okx":
		rest = ccxt.NewOkx(nil)
		ws = ccxtpro.NewOkx(nil)
	case "gate", "gateio":
		rest = ccxt.NewGate(nil)
		ws = ccxtpro.NewGate(nil)
	case "kucoin":
		rest = ccxt.NewKucoin(nil)
		ws = ccxtpro.NewKucoin(nil)
	case "mexc":
		rest = ccxt.NewMexc(nil)
		ws = ccxtpro.NewMexc(nil)
	case "htx", "huobi":
		rest = ccxt.NewHtx(nil)
		ws = ccxtpro.NewHtx(nil)
	case "bitget":
		rest = ccxt.NewBitget(nil)
		ws = ccxtpro.NewBitget(nil)
	default:
		return nil, fmt.Errorf("unsupported exchange: %s", exchangeName)
	}

	// Set credentials
	rest.SetApiKey(apiKey)
	rest.SetSecret(secret)
	ws.SetApiKey(apiKey)
	ws.SetSecret(secret)

	// Set sandbox mode if needed
	if sandbox {
		rest.SetSandboxMode(true)
		ws.SetSandboxMode(true)
	}

	adapter := &CCXTAdapter{
		rest:         rest,
		ws:           ws,
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

	// Load markets
	c.rest.LoadMarkets()

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
func (c *CCXTAdapter) GetExchangeInfo(ctx context.Context, symbol string) (*ExchangeInfo, error) {
	ccxtSymbol := convertToCCXTSymbol(symbol)

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
		result, err = c.rest.CreateOrder(
			ccxtSymbol,
			"limit",
			side,
			order.Quantity,
			ccxt.WithCreateOrderPrice(order.Price),
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

// CancelOrder cancels a single order
func (c *CCXTAdapter) CancelOrder(ctx context.Context, symbol, orderID string) error {
	ccxtSymbol := convertToCCXTSymbol(symbol)

	_, err := c.rest.CancelOrder(orderID, ccxt.WithCancelOrderSymbol(ccxtSymbol))
	if err != nil {
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

	orders, err := c.rest.FetchOpenOrders(ccxt.WithFetchOpenOrdersSymbol(ccxtSymbol))
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
	c.filledQtyByOrder = make(map[string]float64)
	c.wsMu.Unlock()

	log.Printf("[CCXT:%s] Starting WebSocket streams for %s", c.exchangeName, c.symbol)

	// Load markets for WS client
	c.ws.LoadMarkets()

	// Start WebSocket watchers in separate goroutines
	go c.watchOrders()
	go c.watchBalance()
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
			orders, err := c.ws.WatchOrders(ccxt.WithWatchOrdersSymbol(c.symbol))
			if err != nil {
				log.Printf("[CCXT:%s] WatchOrders error: %v", c.exchangeName, err)
				if c.handlers.OnError != nil {
					c.handlers.OnError(err)
				}
				time.Sleep(time.Second)
				continue
			}

			for _, order := range orders {
				orderID := derefString(order.Id)
				rawStatus := derefString(order.Status)
				filledQty := derefFloat(order.Filled)

				log.Printf("[CCXT:%s] WatchOrders update: order=%s status=%s filled=%.6f price=%.8f",
					c.exchangeName, orderID, rawStatus, filledQty, derefFloat(order.Price))

				// Fallback fill detection from watchOrders.
				// Bybit watchMyTrades (execution topic) is unreliable — fills arrive here
				// as status=closed/partially_filled. Emit OnFill for any new filled qty delta.
				if filledQty > 0 && (rawStatus == "closed" || rawStatus == "partially_filled") {
					c.filledQtyMu.Lock()
					lastFilled := c.filledQtyByOrder[orderID]
					delta := filledQty - lastFilled
					if delta > 0 {
						c.filledQtyByOrder[orderID] = filledQty
					}
					c.filledQtyMu.Unlock()

					if delta > 0 && c.handlers.OnFill != nil {
						c.handlers.OnFill(&types.FillEvent{
							OrderID:   orderID,
							Symbol:    c.nativeSymbol,
							Side:      strings.ToUpper(derefString(order.Side)),
							Price:     derefFloat(order.Price),
							Quantity:  delta,
							TradeID:   fmt.Sprintf("%s_order", orderID),
							Timestamp: time.UnixMilli(derefInt64(order.Timestamp)),
						})
					}
				}

				if c.handlers.OnOrderUpdate != nil {
					c.handlers.OnOrderUpdate(&types.OrderEvent{
						OrderID:            orderID,
						ClientOrderID:      derefString(order.ClientOrderId),
						Symbol:             c.nativeSymbol,
						Side:               strings.ToUpper(derefString(order.Side)),
						Type:               strings.ToUpper(derefString(order.Type)),
						Status:             mapCCXTStatus(order.Status),
						Price:              derefFloat(order.Price),
						Quantity:           derefFloat(order.Amount),
						ExecutedQty:        derefFloat(order.Filled),
						CumulativeQuoteQty: derefFloat(order.Cost),
						Timestamp:          time.UnixMilli(derefInt64(order.Timestamp)),
					})
				}
			}
		}
	}
}

// watchBalance watches for balance updates via WebSocket
func (c *CCXTAdapter) watchBalance() {
	log.Printf("[CCXT:%s] Starting balance watcher", c.exchangeName)

	for {
		select {
		case <-c.ctx.Done():
			log.Printf("[CCXT:%s] Balance watcher stopped", c.exchangeName)
			return
		default:
			balance, err := c.ws.WatchBalance()
			if err != nil {
				log.Printf("[CCXT:%s] WatchBalance error: %v", c.exchangeName, err)
				if c.handlers.OnError != nil {
					c.handlers.OnError(err)
				}
				time.Sleep(time.Second)
				continue
			}

			if c.handlers.OnAccountUpdate != nil {
				var balances []types.Balance
				for asset, freePtr := range balance.Free {
					free := derefFloat(freePtr)
					locked := 0.0
					if usedPtr, ok := balance.Used[asset]; ok {
						locked = derefFloat(usedPtr)
					}
					if free > 0 || locked > 0 {
						balances = append(balances, types.Balance{
							Asset:  asset,
							Free:   free,
							Locked: locked,
						})
					}
				}

				c.handlers.OnAccountUpdate(&types.AccountEvent{
					Balances:  balances,
					Timestamp: time.Now(),
				})
			}
		}
	}
}

// watchMyTrades watches for trade executions via WebSocket
func (c *CCXTAdapter) watchMyTrades() {
	log.Printf("[CCXT:%s] Starting my trades watcher for symbol=%s", c.exchangeName, c.symbol)

	for {
		select {
		case <-c.ctx.Done():
			log.Printf("[CCXT:%s] My trades watcher stopped", c.exchangeName)
			return
		default:
			log.Printf("[CCXT:%s] Calling WatchMyTrades...", c.exchangeName)
			trades, err := c.ws.WatchMyTrades(ccxt.WithWatchMyTradesSymbol(c.symbol))
			if err != nil {
				log.Printf("[CCXT:%s] WatchMyTrades error: %v", c.exchangeName, err)
				if c.handlers.OnError != nil {
					c.handlers.OnError(err)
				}
				time.Sleep(time.Second)
				continue
			}
			log.Printf("[CCXT:%s] WatchMyTrades returned %d trades", c.exchangeName, len(trades))

			for _, trade := range trades {
				tradeOrderID := derefString(trade.Order)
				tradeQty := derefFloat(trade.Amount)

				log.Printf("[CCXT:%s] Received trade: order=%s side=%s price=%.8f qty=%.6f",
					c.exchangeName, tradeOrderID, derefString(trade.Side),
					derefFloat(trade.Price), tradeQty)

				// Update tracker so watchOrders fallback doesn't double-count this fill
				c.filledQtyMu.Lock()
				c.filledQtyByOrder[tradeOrderID] += tradeQty
				c.filledQtyMu.Unlock()

				if c.handlers.OnFill != nil {
					commission := derefFloat(trade.Fee.Cost)

					c.handlers.OnFill(&types.FillEvent{
						OrderID:         tradeOrderID,
						ClientOrderID:   "",
						Symbol:          c.nativeSymbol,
						Side:            strings.ToUpper(derefString(trade.Side)),
						Price:           derefFloat(trade.Price),
						Quantity:        tradeQty,
						Commission:      commission,
						CommissionAsset: "",
						TradeID:         derefString(trade.Id),
						Timestamp:       time.UnixMilli(derefInt64(trade.Timestamp)),
					})
				}
			}
		}
	}
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
