package exchange

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// ────────────────────────────────────────────────────────────────────────────
// Constants
// ────────────────────────────────────────────────────────────────────────────

const (
	bybitWSTradeURL       = "wss://stream.bybit.com/v5/trade"
	bybitWSPrivateURL     = "wss://stream.bybit.com/v5/private"
	bybitTestWSTradeURL   = "wss://stream-testnet.bybit.com/v5/trade"
	bybitTestWSPrivateURL = "wss://stream-testnet.bybit.com/v5/private"
	bybitRestURL          = "https://api.bybit.com"
	bybitTestRestURL      = "https://api-testnet.bybit.com"
	bybitRecvWindow       = "5000"
	bybitPingInterval     = 20 * time.Second
	bybitWSTimeout        = 5 * time.Second
	bybitCategory         = "spot"
)

// Compile-time interface check
var _ WSOrderExchange = (*BybitNativeAdapter)(nil)

// ────────────────────────────────────────────────────────────────────────────
// Private WS push-message types
// (Defined here so bybit_native_ws_private.go can reference them without
//  redeclaring. All files share the same package.)
// ────────────────────────────────────────────────────────────────────────────

// bybitWSPrivateMsg routes topic-based messages from the private WS.
type bybitWSPrivateMsg struct {
	Topic string `json:"topic"`
	Op    string `json:"op,omitempty"`
}

// bybitWSOrderUpdate from the private stream (order topic).
type bybitWSOrderUpdate struct {
	Topic string `json:"topic"`
	Data  []struct {
		Symbol       string `json:"symbol"`
		OrderID      string `json:"orderId"`
		OrderLinkID  string `json:"orderLinkId"`
		Side         string `json:"side"`
		OrderType    string `json:"orderType"`
		Price        string `json:"price"`
		Qty          string `json:"qty"`
		OrderStatus  string `json:"orderStatus"`
		CumExecQty   string `json:"cumExecQty"`
		CumExecValue string `json:"cumExecValue"`
		UpdatedTime  string `json:"updatedTime"`
		Category     string `json:"category"`
	} `json:"data"`
}

// bybitWSExecutionUpdate from the private stream (execution topic).
type bybitWSExecutionUpdate struct {
	Topic string `json:"topic"`
	Data  []struct {
		Category    string `json:"category"`
		Symbol      string `json:"symbol"`
		ExecID      string `json:"execId"`
		OrderID     string `json:"orderId"`
		OrderLinkID string `json:"orderLinkId"`
		Side        string `json:"side"`
		ExecPrice   string `json:"execPrice"`
		ExecQty     string `json:"execQty"`
		ExecFee     string `json:"execFee"`
		ExecTime    string `json:"execTime"`
		FeeCurrency string `json:"feeCurrency"`
	} `json:"data"`
}

// bybitWSWalletUpdate from the private stream (wallet topic).
type bybitWSWalletUpdate struct {
	Topic string `json:"topic"`
	Data  []struct {
		AccountType string `json:"accountType"`
		Coin        []struct {
			Coin          string `json:"coin"`
			Free          string `json:"free"`
			WalletBalance string `json:"walletBalance"`
			Locked        string `json:"locked"`
		} `json:"coin"`
	} `json:"data"`
}

// ────────────────────────────────────────────────────────────────────────────
// REST response types
// ────────────────────────────────────────────────────────────────────────────

type bybitTradeHistoryResponse struct {
	RetCode int    `json:"retCode"`
	RetMsg  string `json:"retMsg"`
	Result  struct {
		List []struct {
			Symbol      string `json:"symbol"`
			OrderID     string `json:"orderId"`
			OrderLinkID string `json:"orderLinkId"`
			Side        string `json:"side"`
			OrderPrice  string `json:"orderPrice"`
			OrderQty    string `json:"orderQty"`
			ExecPrice   string `json:"execPrice"`
			ExecQty     string `json:"execQty"`
			ExecValue   string `json:"execValue"`
			ExecTime    string `json:"execTime"`
			ExecID      string `json:"execId"`
			ExecFee     string `json:"execFee"`
			FeeCurrency string `json:"feeCurrency"`
		} `json:"list"`
		NextPageCursor string `json:"nextPageCursor"`
	} `json:"result"`
}

type bybitRecentTradesResponse struct {
	RetCode int    `json:"retCode"`
	RetMsg  string `json:"retMsg"`
	Result  struct {
		Category string `json:"category"`
		List     []struct {
			ExecID string `json:"execId"`
			Symbol string `json:"symbol"`
			Price  string `json:"price"`
			Size   string `json:"size"`
			Side   string `json:"side"`
			Time   string `json:"time"`
		} `json:"list"`
	} `json:"result"`
}

// ────────────────────────────────────────────────────────────────────────────
// Order state dedup type
// (Used by bybit_native_ws_private.go — defined here so it is in scope for
//  the whole package without redeclaration.)
// ────────────────────────────────────────────────────────────────────────────

// bybitNativeOrderState tracks last-seen order state for duplicate-event filtering.
type bybitNativeOrderState struct {
	status    string
	filledQty float64
	seenAt    time.Time
	terminal  bool
}

// ────────────────────────────────────────────────────────────────────────────
// BybitNativeAdapter
// ────────────────────────────────────────────────────────────────────────────

// BybitNativeAdapter implements WSOrderExchange for Bybit V5.
//
// Architecture (composed, not monolithic):
//   - tradeWS  (*BybitWSTrade)   — WS order operations (place/amend/cancel)
//   - privateWS (*BybitWSPrivate) — push user stream (order/execution/wallet)
//   - REST                        — market data, batch orders, account queries
//
// Each WS component manages its own connection, auth, keepalive, and reconnect.
// The adapter's job is wiring them together and implementing the Exchange interface
// via REST for the non-WS methods.
type BybitNativeAdapter struct {
	apiKey    string
	apiSecret string
	restURL   string
	symbol    string // native format: BTCUSDT

	tradeWS   *BybitWSTrade   // WS order operations
	privateWS *BybitWSPrivate // push user stream

	ctx     context.Context
	cancel  context.CancelFunc
	started atomic.Bool
}

// NewBybitNativeExchange creates a BybitNativeAdapter with composed WS clients.
// symbol should be in native Bybit format (e.g. "BTCUSDT").
func NewBybitNativeExchange(apiKey, apiSecret, symbol string, sandbox bool) (*BybitNativeAdapter, error) {
	restURL := bybitRestURL
	if sandbox {
		restURL = bybitTestRestURL
	}

	return &BybitNativeAdapter{
		apiKey:    apiKey,
		apiSecret: apiSecret,
		restURL:   restURL,
		symbol:    symbol,
		tradeWS:   NewBybitWSTrade(apiKey, apiSecret, sandbox),
		privateWS: NewBybitWSPrivate(apiKey, apiSecret, sandbox),
	}, nil
}

// ════════════════════════════════════════════════════════════════════════════
// Lifecycle
// ════════════════════════════════════════════════════════════════════════════

func (b *BybitNativeAdapter) Start(ctx context.Context) error {
	b.ctx, b.cancel = context.WithCancel(ctx)

	if err := b.tradeWS.Connect(b.ctx); err != nil {
		return fmt.Errorf("[BYBIT-NATIVE] trade WS connect: %w", err)
	}

	b.started.Store(true)
	log.Printf("[BYBIT-NATIVE] Started (symbol=%s, rest=%s)", b.symbol, b.restURL)
	return nil
}

func (b *BybitNativeAdapter) Stop(ctx context.Context) error {
	log.Printf("[BYBIT-NATIVE] Stopping...")
	b.started.Store(false)

	if b.cancel != nil {
		b.cancel()
	}

	b.tradeWS.Close()
	b.privateWS.Close()

	log.Printf("[BYBIT-NATIVE] Stopped")
	return nil
}

// ════════════════════════════════════════════════════════════════════════════
// WSOrderExchange — bridge to BybitWSTrade
// ════════════════════════════════════════════════════════════════════════════

func (b *BybitNativeAdapter) PlaceOrderWs(ctx context.Context, order *OrderRequest) (*Order, error) {
	return b.tradeWS.PlaceOrderWs(ctx, order)
}

func (b *BybitNativeAdapter) EditOrderWs(ctx context.Context, orderID, symbol string, newPrice, newQty float64) (*Order, error) {
	return b.tradeWS.EditOrderWs(ctx, orderID, symbol, newPrice, newQty)
}

func (b *BybitNativeAdapter) CancelOrderWs(ctx context.Context, symbol, orderID string) error {
	return b.tradeWS.CancelOrderWs(ctx, symbol, orderID)
}

func (b *BybitNativeAdapter) CancelAllOrdersWs(ctx context.Context, symbol string) error {
	return b.tradeWS.CancelAllOrdersWs(ctx, symbol)
}

func (b *BybitNativeAdapter) HasWsOrderSupport() bool { return true }
func (b *BybitNativeAdapter) HasWsEditSupport() bool  { return true }

// ════════════════════════════════════════════════════════════════════════════
// SubscribeUserStream — bridge to BybitWSPrivate
// ════════════════════════════════════════════════════════════════════════════

func (b *BybitNativeAdapter) SubscribeUserStream(ctx context.Context, handlers UserStreamHandlers) error {
	b.privateWS.SetHandlers(handlers)
	return b.privateWS.Connect(ctx)
}

// ════════════════════════════════════════════════════════════════════════════
// Exchange interface — REST methods
// ════════════════════════════════════════════════════════════════════════════

func (b *BybitNativeAdapter) GetExchangeInfo(ctx context.Context, symbol string) (*ExchangeInfo, error) {
	params := map[string]string{"category": bybitCategory, "symbol": symbol}
	body, err := b.doPublicRequest(ctx, "GET", "/v5/market/instruments-info", params)
	if err != nil {
		return nil, err
	}

	var resp struct {
		RetCode int `json:"retCode"`
		Result  struct {
			List []struct {
				Symbol        string `json:"symbol"`
				BaseCoin      string `json:"baseCoin"`
				QuoteCoin     string `json:"quoteCoin"`
				LotSizeFilter struct {
					BasePrecision string `json:"basePrecision"`
					MaxOrderQty   string `json:"maxOrderQty"`
					MinOrderAmt   string `json:"minOrderAmt"`
				} `json:"lotSizeFilter"`
				PriceFilter struct {
					TickSize string `json:"tickSize"`
				} `json:"priceFilter"`
			} `json:"list"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, err
	}

	symbols := make([]Symbol, len(resp.Result.List))
	for i, inst := range resp.Result.List {
		basePrecision := bybitNativePrecisionFromStr(inst.LotSizeFilter.BasePrecision)
		pricePrecision := bybitNativePrecisionFromStr(inst.PriceFilter.TickSize)

		symbols[i] = Symbol{
			Symbol:              inst.Symbol,
			BaseAsset:           inst.BaseCoin,
			BaseAssetPrecision:  basePrecision,
			QuoteAsset:          inst.QuoteCoin,
			QuotePrecision:      pricePrecision,
			QuoteAssetPrecision: pricePrecision,
			Filters: []Filter{
				{FilterType: "PRICE_FILTER", MinNotional: inst.LotSizeFilter.MinOrderAmt},
				{FilterType: "LOT_SIZE", MaxQty: inst.LotSizeFilter.MaxOrderQty},
			},
		}
	}
	return &ExchangeInfo{Symbols: symbols}, nil
}

func (b *BybitNativeAdapter) GetDepth(ctx context.Context, symbol string) (*Depth, error) {
	params := map[string]string{"category": bybitCategory, "symbol": symbol, "limit": "20"}
	body, err := b.doPublicRequest(ctx, "GET", "/v5/market/orderbook", params)
	if err != nil {
		return nil, err
	}

	var resp struct {
		Result struct {
			B [][]string `json:"b"`
			A [][]string `json:"a"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, err
	}
	return &Depth{Bids: resp.Result.B, Asks: resp.Result.A}, nil
}

func (b *BybitNativeAdapter) GetAccount(ctx context.Context) (*Account, error) {
	for _, accountType := range []string{"UNIFIED", "SPOT"} {
		params := map[string]string{"accountType": accountType}
		body, err := b.doRequest(ctx, "GET", "/v5/account/wallet-balance", params)
		if err != nil {
			log.Printf("[BYBIT-NATIVE] GetAccount %s error: %v", accountType, err)
			continue
		}

		var resp struct {
			Result struct {
				List []struct {
					Coin []struct {
						Coin          string `json:"coin"`
						WalletBalance string `json:"walletBalance"`
						Locked        string `json:"locked"`
					} `json:"coin"`
				} `json:"list"`
			} `json:"result"`
		}
		if err := json.Unmarshal(body, &resp); err != nil {
			continue
		}

		var balances []Balance
		for _, acct := range resp.Result.List {
			for _, coin := range acct.Coin {
				walletBal := bybitNativeParseFloat(coin.WalletBalance)
				locked := bybitNativeParseFloat(coin.Locked)
				free := walletBal - locked
				if walletBal > 0 {
					balances = append(balances, Balance{Asset: coin.Coin, Free: free, Locked: locked})
				}
			}
		}
		if len(balances) > 0 {
			return &Account{Balances: balances}, nil
		}
	}
	return &Account{Balances: []Balance{}}, nil
}

func (b *BybitNativeAdapter) PlaceOrder(ctx context.Context, order *OrderRequest) (*Order, error) {
	params := map[string]string{
		"category":    bybitCategory,
		"symbol":      order.Symbol,
		"side":        bybitNativeToSide(order.Side),
		"orderType":   bybitNativeToOrderType(order.Type),
		"qty":         bybitNativeFormatFloat(order.Quantity),
		"timeInForce": bybitNativeToTIF(order.TimeInForce),
	}
	if order.Type == "LIMIT" && order.Price > 0 {
		params["price"] = bybitNativeFormatFloat(order.Price)
	}
	if order.ClientOrderID != "" {
		params["orderLinkId"] = order.ClientOrderID
	}

	body, err := b.doRequest(ctx, "POST", "/v5/order/create", params)
	if err != nil {
		return nil, fmt.Errorf("place order: %w", err)
	}

	var resp struct {
		Result struct {
			OrderID     string `json:"orderId"`
			OrderLinkID string `json:"orderLinkId"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, err
	}

	return &Order{
		OrderID:       resp.Result.OrderID,
		ClientOrderID: resp.Result.OrderLinkID,
		Symbol:        order.Symbol,
		Side:          order.Side,
		Type:          order.Type,
		Price:         order.Price,
		Quantity:      order.Quantity,
		Status:        "NEW",
		Timestamp:     time.Now(),
	}, nil
}

func (b *BybitNativeAdapter) BatchPlaceOrders(ctx context.Context, orders []*OrderRequest) (*BatchOrderResponse, error) {
	if len(orders) == 0 {
		return &BatchOrderResponse{Orders: []*Order{}, Errors: []string{}}, nil
	}

	type batchItem struct {
		Symbol      string `json:"symbol"`
		Side        string `json:"side"`
		OrderType   string `json:"orderType"`
		Qty         string `json:"qty"`
		Price       string `json:"price,omitempty"`
		TimeInForce string `json:"timeInForce"`
		OrderLinkID string `json:"orderLinkId,omitempty"`
	}

	items := make([]batchItem, 0, len(orders))
	for _, o := range orders {
		if o.Quantity <= 0 || o.Quantity != o.Quantity || o.Price != o.Price {
			continue
		}
		item := batchItem{
			Symbol:      o.Symbol,
			Side:        bybitNativeToSide(o.Side),
			OrderType:   bybitNativeToOrderType(o.Type),
			Qty:         bybitNativeFormatFloat(o.Quantity),
			TimeInForce: bybitNativeToTIF(o.TimeInForce),
		}
		if o.Type == "LIMIT" && o.Price > 0 {
			item.Price = bybitNativeFormatFloat(o.Price)
		}
		if o.ClientOrderID != "" {
			item.OrderLinkID = o.ClientOrderID
		}
		items = append(items, item)
	}

	jsonBody, err := json.Marshal(map[string]interface{}{
		"category": bybitCategory,
		"request":  items,
	})
	if err != nil {
		return nil, err
	}

	body, err := b.doRequestRaw(ctx, "POST", "/v5/order/create-batch", string(jsonBody))
	if err != nil {
		return b.batchPlaceSequential(ctx, orders)
	}

	var resp struct {
		Result struct {
			List []struct {
				OrderID     string `json:"orderId"`
				OrderLinkID string `json:"orderLinkId"`
				Symbol      string `json:"symbol"`
			} `json:"list"`
		} `json:"result"`
		RetExtInfo struct {
			List []struct {
				Code int    `json:"code"`
				Msg  string `json:"msg"`
			} `json:"list"`
		} `json:"retExtInfo"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return b.batchPlaceSequential(ctx, orders)
	}

	result := &BatchOrderResponse{
		Orders: make([]*Order, 0, len(resp.Result.List)),
		Errors: make([]string, 0),
	}
	for i, ext := range resp.RetExtInfo.List {
		if ext.Code != 0 {
			cid := ""
			if i < len(orders) {
				cid = orders[i].ClientOrderID
			}
			result.Errors = append(result.Errors, fmt.Sprintf("Order %d (%s): %s", i, cid, ext.Msg))
		}
	}
	for i, o := range resp.Result.List {
		if o.OrderID != "" && i < len(orders) {
			result.Orders = append(result.Orders, &Order{
				OrderID:       o.OrderID,
				ClientOrderID: o.OrderLinkID,
				Symbol:        o.Symbol,
				Side:          orders[i].Side,
				Type:          orders[i].Type,
				Price:         orders[i].Price,
				Quantity:      orders[i].Quantity,
				Status:        "NEW",
			})
		}
	}
	return result, nil
}

func (b *BybitNativeAdapter) batchPlaceSequential(ctx context.Context, orders []*OrderRequest) (*BatchOrderResponse, error) {
	result := &BatchOrderResponse{
		Orders: make([]*Order, 0, len(orders)),
		Errors: make([]string, 0),
	}
	for i, o := range orders {
		placed, err := b.PlaceOrder(ctx, o)
		if err != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("Order %d (%s): %v", i, o.ClientOrderID, err))
			continue
		}
		result.Orders = append(result.Orders, placed)
	}
	return result, nil
}

func (b *BybitNativeAdapter) CancelOrder(ctx context.Context, symbol, orderID string) error {
	params := map[string]string{
		"category": bybitCategory,
		"symbol":   symbol,
		"orderId":  orderID,
	}
	_, err := b.doRequest(ctx, "POST", "/v5/order/cancel", params)
	if err != nil {
		if strings.Contains(err.Error(), "110001") {
			return nil // Already gone
		}
		return err
	}
	return nil
}

func (b *BybitNativeAdapter) CancelAllOrders(ctx context.Context, symbol string) error {
	params := map[string]string{
		"category": bybitCategory,
		"symbol":   symbol,
	}
	_, err := b.doRequest(ctx, "POST", "/v5/order/cancel-all", params)
	return err
}

func (b *BybitNativeAdapter) GetOpenOrders(ctx context.Context, symbol string) ([]*Order, error) {
	params := map[string]string{
		"category": bybitCategory,
		"symbol":   symbol,
		"openOnly": "0",
		"limit":    "50",
	}
	body, err := b.doRequest(ctx, "GET", "/v5/order/realtime", params)
	if err != nil {
		return nil, err
	}

	var resp struct {
		Result struct {
			List []struct {
				OrderID      string `json:"orderId"`
				OrderLinkID  string `json:"orderLinkId"`
				Symbol       string `json:"symbol"`
				Side         string `json:"side"`
				OrderType    string `json:"orderType"`
				Price        string `json:"price"`
				Qty          string `json:"qty"`
				OrderStatus  string `json:"orderStatus"`
				CumExecQty   string `json:"cumExecQty"`
				CumExecValue string `json:"cumExecValue"`
				CreatedTime  string `json:"createdTime"`
			} `json:"list"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, err
	}

	orders := make([]*Order, len(resp.Result.List))
	for i, o := range resp.Result.List {
		orders[i] = &Order{
			OrderID:            o.OrderID,
			ClientOrderID:      o.OrderLinkID,
			Symbol:             o.Symbol,
			Side:               bybitNativeMapSide(o.Side),
			Type:               bybitNativeMapOrderType(o.OrderType),
			Status:             bybitNativeMapOrderStatus(o.OrderStatus),
			Price:              bybitNativeParseFloat(o.Price),
			Quantity:           bybitNativeParseFloat(o.Qty),
			ExecutedQty:        bybitNativeParseFloat(o.CumExecQty),
			CumulativeQuoteQty: bybitNativeParseFloat(o.CumExecValue),
			Timestamp:          bybitNativeParseTimestamp(o.CreatedTime),
		}
	}
	return orders, nil
}

func (b *BybitNativeAdapter) FetchClosedOrders(ctx context.Context, symbol string, since time.Time, limit int) ([]*Order, error) {
	now := time.Now()
	const windowDays = 7
	const pageSize = 50

	var result []*Order
	windowStart := since

	for windowStart.Before(now) {
		windowEnd := windowStart.Add(windowDays * 24 * time.Hour)
		if windowEnd.After(now) {
			windowEnd = now
		}

		startMs := strconv.FormatInt(windowStart.UnixMilli(), 10)
		endMs := strconv.FormatInt(windowEnd.UnixMilli(), 10)

		params := map[string]string{
			"category":  bybitCategory,
			"symbol":    symbol,
			"startTime": startMs,
			"endTime":   endMs,
			"limit":     strconv.Itoa(pageSize),
		}

		body, err := b.doRequest(ctx, "GET", "/v5/execution/list", params)
		if err != nil {
			return nil, fmt.Errorf("fetch trades (window %s-%s): %w",
				windowStart.Format("01-02"), windowEnd.Format("01-02"), err)
		}

		var resp bybitTradeHistoryResponse
		if err := json.Unmarshal(body, &resp); err != nil {
			return nil, err
		}

		for _, t := range resp.Result.List {
			result = append(result, &Order{
				OrderID:            t.OrderID,
				ClientOrderID:      t.ExecID,
				Symbol:             symbol,
				Side:               bybitNativeMapSide(t.Side),
				Type:               "LIMIT",
				Price:              bybitNativeParseFloat(t.ExecPrice),
				Quantity:           bybitNativeParseFloat(t.ExecQty),
				ExecutedQty:        bybitNativeParseFloat(t.ExecQty),
				CumulativeQuoteQty: bybitNativeParseFloat(t.ExecValue),
				Status:             "FILLED",
				Timestamp:          bybitNativeParseTimestamp(t.ExecTime),
			})
		}

		if len(resp.Result.List) < pageSize {
			windowStart = windowEnd
			continue
		}

		if last := resp.Result.List[len(resp.Result.List)-1]; last.ExecTime != "" {
			ms, _ := strconv.ParseInt(last.ExecTime, 10, 64)
			windowStart = time.UnixMilli(ms + 1)
		} else {
			windowStart = windowEnd
		}

		time.Sleep(100 * time.Millisecond) // rate limit
	}

	log.Printf("[BYBIT-NATIVE] FetchClosedOrders %s → %s: %d trades",
		since.Format("2006-01-02"), now.Format("2006-01-02"), len(result))
	return result, nil
}

func (b *BybitNativeAdapter) GetTicker(ctx context.Context, symbol string) (float64, error) {
	params := map[string]string{"category": bybitCategory, "symbol": symbol}
	body, err := b.doPublicRequest(ctx, "GET", "/v5/market/tickers", params)
	if err != nil {
		return 0, err
	}

	var resp struct {
		Result struct {
			List []struct {
				LastPrice string `json:"lastPrice"`
			} `json:"list"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return 0, err
	}
	if len(resp.Result.List) == 0 {
		return 0, nil
	}
	return bybitNativeParseFloat(resp.Result.List[0].LastPrice), nil
}

func (b *BybitNativeAdapter) GetRecentTrades(ctx context.Context, symbol string, limit int) ([]Trade, error) {
	if limit <= 0 {
		limit = 100
	}
	params := map[string]string{
		"category": bybitCategory,
		"symbol":   symbol,
		"limit":    strconv.Itoa(limit),
	}
	body, err := b.doPublicRequest(ctx, "GET", "/v5/market/recent-trade", params)
	if err != nil {
		return nil, err
	}

	var resp bybitRecentTradesResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, err
	}

	trades := make([]Trade, len(resp.Result.List))
	for i, t := range resp.Result.List {
		trades[i] = Trade{
			Price:        bybitNativeParseFloat(t.Price),
			Quantity:     bybitNativeParseFloat(t.Size),
			Timestamp:    bybitNativeParseTimestamp(t.Time),
			IsBuyerMaker: t.Side == "Sell",
		}
	}
	return trades, nil
}

// ════════════════════════════════════════════════════════════════════════════
// HTTP helpers
// ════════════════════════════════════════════════════════════════════════════

func (b *BybitNativeAdapter) bybitSign(timestamp, payload string) string {
	signStr := timestamp + b.apiKey + bybitRecvWindow + payload
	mac := hmac.New(sha256.New, []byte(b.apiSecret))
	mac.Write([]byte(signStr))
	return hex.EncodeToString(mac.Sum(nil))
}

func (b *BybitNativeAdapter) doRequest(ctx context.Context, method, endpoint string, params map[string]string) ([]byte, error) {
	timestamp := strconv.FormatInt(time.Now().UnixMilli(), 10)

	var reqURL string
	var bodyReader *strings.Reader
	var payload string

	if method == "GET" || method == "DELETE" {
		if len(params) > 0 {
			parts := make([]string, 0, len(params))
			for k, v := range params {
				parts = append(parts, k+"="+v)
			}
			payload = strings.Join(parts, "&")
			reqURL = b.restURL + endpoint + "?" + payload
		} else {
			reqURL = b.restURL + endpoint
		}
	} else {
		reqURL = b.restURL + endpoint
		if len(params) > 0 {
			jsonBody, err := json.Marshal(params)
			if err != nil {
				return nil, err
			}
			payload = string(jsonBody)
			bodyReader = strings.NewReader(payload)
		}
	}

	signature := b.bybitSign(timestamp, payload)

	var req *http.Request
	var err error
	if bodyReader != nil {
		req, err = http.NewRequestWithContext(ctx, method, reqURL, bodyReader)
	} else {
		req, err = http.NewRequestWithContext(ctx, method, reqURL, nil)
	}
	if err != nil {
		return nil, err
	}

	req.Header.Set("X-BAPI-API-KEY", b.apiKey)
	req.Header.Set("X-BAPI-SIGN", signature)
	req.Header.Set("X-BAPI-TIMESTAMP", timestamp)
	req.Header.Set("X-BAPI-RECV-WINDOW", bybitRecvWindow)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("API error: status=%d body=%s", resp.StatusCode, string(body))
	}

	var baseResp struct {
		RetCode int    `json:"retCode"`
		RetMsg  string `json:"retMsg"`
	}
	if err := json.Unmarshal(body, &baseResp); err != nil {
		return nil, err
	}
	if baseResp.RetCode != 0 {
		return nil, fmt.Errorf("Bybit error: code=%d msg=%s", baseResp.RetCode, baseResp.RetMsg)
	}

	return body, nil
}

func (b *BybitNativeAdapter) doPublicRequest(ctx context.Context, method, endpoint string, params map[string]string) ([]byte, error) {
	var reqURL string
	if len(params) > 0 {
		parts := make([]string, 0, len(params))
		for k, v := range params {
			parts = append(parts, k+"="+v)
		}
		reqURL = b.restURL + endpoint + "?" + strings.Join(parts, "&")
	} else {
		reqURL = b.restURL + endpoint
	}

	req, err := http.NewRequestWithContext(ctx, method, reqURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("API error: status=%d body=%s", resp.StatusCode, string(body))
	}

	var baseResp struct {
		RetCode int    `json:"retCode"`
		RetMsg  string `json:"retMsg"`
	}
	if err := json.Unmarshal(body, &baseResp); err != nil {
		return nil, err
	}
	if baseResp.RetCode != 0 {
		return nil, fmt.Errorf("Bybit error: code=%d msg=%s", baseResp.RetCode, baseResp.RetMsg)
	}

	return body, nil
}

func (b *BybitNativeAdapter) doRequestRaw(ctx context.Context, method, endpoint, jsonBody string) ([]byte, error) {
	timestamp := strconv.FormatInt(time.Now().UnixMilli(), 10)
	signature := b.bybitSign(timestamp, jsonBody)

	reqURL := b.restURL + endpoint
	req, err := http.NewRequestWithContext(ctx, method, reqURL, strings.NewReader(jsonBody))
	if err != nil {
		return nil, err
	}

	req.Header.Set("X-BAPI-API-KEY", b.apiKey)
	req.Header.Set("X-BAPI-SIGN", signature)
	req.Header.Set("X-BAPI-TIMESTAMP", timestamp)
	req.Header.Set("X-BAPI-RECV-WINDOW", bybitRecvWindow)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("API error: status=%d body=%s", resp.StatusCode, string(body))
	}

	var baseResp struct {
		RetCode int    `json:"retCode"`
		RetMsg  string `json:"retMsg"`
	}
	if err := json.Unmarshal(body, &baseResp); err != nil {
		return nil, err
	}
	if baseResp.RetCode != 0 {
		return nil, fmt.Errorf("Bybit error: code=%d msg=%s", baseResp.RetCode, baseResp.RetMsg)
	}

	return body, nil
}

// ════════════════════════════════════════════════════════════════════════════
// Mapping helpers (prefixed bybitNative* to avoid conflicts with deprecated pkg)
// ════════════════════════════════════════════════════════════════════════════

func bybitNativeToSide(side string) string {
	switch side {
	case "BUY":
		return "Buy"
	case "SELL":
		return "Sell"
	default:
		return side
	}
}

func bybitNativeMapSide(side string) string {
	switch side {
	case "Buy":
		return "BUY"
	case "Sell":
		return "SELL"
	default:
		return side
	}
}

func bybitNativeToOrderType(t string) string {
	switch t {
	case "LIMIT":
		return "Limit"
	case "MARKET":
		return "Market"
	default:
		return t
	}
}

func bybitNativeMapOrderType(t string) string {
	switch t {
	case "Limit":
		return "LIMIT"
	case "Market":
		return "MARKET"
	default:
		return t
	}
}

func bybitNativeMapOrderStatus(status string) string {
	switch status {
	case "New":
		return "NEW"
	case "Filled":
		return "FILLED"
	case "PartiallyFilled":
		return "PARTIALLY_FILLED"
	case "Cancelled", "PartiallyFilledCanceled":
		return "CANCELED"
	case "Rejected":
		return "REJECTED"
	default:
		return status
	}
}

func bybitNativeToTIF(tif string) string {
	switch tif {
	case "GTX":
		return "PostOnly"
	case "GTC", "":
		return "GTC"
	case "IOC":
		return "IOC"
	case "FOK":
		return "FOK"
	default:
		return tif
	}
}

func bybitNativeFormatFloat(f float64) string {
	s := strconv.FormatFloat(f, 'f', 8, 64)
	if strings.Contains(s, ".") {
		s = strings.TrimRight(s, "0")
		s = strings.TrimRight(s, ".")
	}
	return s
}

func bybitNativeParseFloat(s string) float64 {
	f, _ := strconv.ParseFloat(s, 64)
	return f
}

func bybitNativeParseTimestamp(s string) time.Time {
	if s == "" {
		return time.Now()
	}
	ms, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return time.Now()
	}
	return time.UnixMilli(ms)
}

func bybitNativePrecisionFromStr(s string) int {
	if s == "" {
		return 8
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f == 0 {
		return 8
	}
	str := strings.TrimRight(s, "0")
	idx := strings.Index(str, ".")
	if idx == -1 {
		return 0
	}
	return len(str) - idx - 1
}
