package exchange

import (
	"context"
	"time"

	"mm-platform-engine/internal/types"
)

// Exchange defines the interface for interacting with a cryptocurrency exchange
type Exchange interface {
	// GetExchangeInfo Get exchange info
	GetExchangeInfo(ctx context.Context, symbol string) (*ExchangeInfo, error)
	GetDepth(ctx context.Context, symbol string) (*Depth, error)

	// GetAccount Account operations
	GetAccount(ctx context.Context) (*Account, error)

	// PlaceOrder Order operations
	PlaceOrder(ctx context.Context, order *OrderRequest) (*Order, error)
	BatchPlaceOrders(ctx context.Context, orders []*OrderRequest) (*BatchOrderResponse, error)
	CancelOrder(ctx context.Context, symbol, orderID string) error
	CancelAllOrders(ctx context.Context, symbol string) error
	GetOpenOrders(ctx context.Context, symbol string) ([]*Order, error)

	// FetchClosedOrders retrieves recently closed (filled/canceled) orders.
	// since: only return orders after this timestamp. limit: max orders to return.
	FetchClosedOrders(ctx context.Context, symbol string, since time.Time, limit int) ([]*Order, error)

	// SubscribeUserStream WebSocket operations
	SubscribeUserStream(ctx context.Context, handlers UserStreamHandlers) error

	// GetTicker returns the last trade price for a symbol
	GetTicker(ctx context.Context, symbol string) (float64, error)

	// GetRecentTrades returns recent market trades (for VWAP calculation)
	// limit: maximum number of trades to return (e.g., 100)
	// Returns trades in chronological order (oldest first)
	GetRecentTrades(ctx context.Context, symbol string, limit int) ([]Trade, error)

	// Start Lifecycle
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
}

// Account represents account information
type Account struct {
	Balances []Balance `json:"balances"`
}

// Balance represents an asset balance
type Balance struct {
	Asset  string  `json:"asset"`
	Free   float64 `json:"free"`
	Locked float64 `json:"locked"`
}

// Order represents an exchange order
type Order struct {
	OrderID            string    `json:"orderId"`
	ClientOrderID      string    `json:"clientOrderId"`
	Symbol             string    `json:"symbol"`
	Side               string    `json:"side"`   // BUY or SELL
	Type               string    `json:"type"`   // LIMIT, MARKET
	Status             string    `json:"status"` // NEW, FILLED, CANCELED, etc
	Price              float64   `json:"price"`
	Quantity           float64   `json:"quantity"`
	ExecutedQty        float64   `json:"executedQty"`
	CumulativeQuoteQty float64   `json:"cumulativeQuoteQty"`
	Timestamp          time.Time `json:"timestamp"`
}

// OrderRequest represents a request to place an order
type OrderRequest struct {
	Symbol        string  `json:"symbol"`
	Side          string  `json:"side"`                  // BUY or SELL
	Type          string  `json:"type"`                  // LIMIT, MARKET
	TimeInForce   string  `json:"timeInForce,omitempty"` // GTC, GTX (PostOnly), IOC, FOK
	Price         float64 `json:"price,omitempty"`
	Quantity      float64 `json:"quantity"`
	ClientOrderID string  `json:"clientOrderId,omitempty"`
}

// BatchOrderResponse represents the response from a batch order request
type BatchOrderResponse struct {
	Orders []*Order `json:"orders"`
	Errors []string `json:"errors,omitempty"`
}

// UserStreamHandlers defines callbacks for user stream events
type UserStreamHandlers struct {
	OnAccountUpdate func(*types.AccountEvent)
	OnOrderUpdate   func(*types.OrderEvent)
	OnFill          func(*types.FillEvent)
	OnError         func(error)
}

type ExchangeInfo struct {
	ServerTime int64    `json:"serverTime"`
	Symbols    []Symbol `json:"symbols"`
}

type Symbol struct {
	Symbol              string   `json:"symbol"`
	BaseAsset           string   `json:"baseAsset"`
	BaseAssetPrecision  int      `json:"baseAssetPrecision"`
	QuoteAsset          string   `json:"quoteAsset"`
	QuotePrecision      int      `json:"quotePrecision"`
	QuoteAssetPrecision int      `json:"quoteAssetPrecision"`
	Filters             []Filter `json:"filters"`
}

type Filter struct {
	FilterType        string `json:"filterType"`
	BidMultiplierUp   string `json:"bidMultiplierUp,omitempty"`
	AskMultiplierDown string `json:"askMultiplierDown,omitempty"`
	MinNotional       string `json:"minNotional,omitempty"`
	MaxQty            string `json:"maxQty,omitempty"` // Maximum order quantity (LOT_SIZE filter)
}

type Depth struct {
	Bids [][]string `json:"bids"`
	Asks [][]string `json:"asks"`
}

// Trade represents a market trade (filled order)
type Trade struct {
	Price        float64   `json:"price"`
	Quantity     float64   `json:"quantity"`
	Timestamp    time.Time `json:"timestamp"`
	IsBuyerMaker bool      `json:"isBuyerMaker"` // true if buyer placed order (sell was aggressive)
}

// WSOrderExchange extends Exchange with WebSocket-based order management.
// Exchanges that support WS orders (Bybit, Binance, Gate, KuCoin) implement this.
// The engine checks for this interface via type assertion and falls back to REST if unavailable.
type WSOrderExchange interface {
	Exchange

	// PlaceOrderWs places an order via WebSocket (lower latency than REST).
	PlaceOrderWs(ctx context.Context, order *OrderRequest) (*Order, error)

	// EditOrderWs amends an existing order's price/qty via WebSocket.
	// This avoids cancel+place roundtrip.
	EditOrderWs(ctx context.Context, orderID, symbol string, newPrice, newQty float64) (*Order, error)

	// CancelOrderWs cancels an order via WebSocket.
	CancelOrderWs(ctx context.Context, symbol, orderID string) error

	// CancelAllOrdersWs cancels all orders for a symbol via WebSocket.
	CancelAllOrdersWs(ctx context.Context, symbol string) error

	// HasWsOrderSupport returns true if the exchange supports WS order placement.
	HasWsOrderSupport() bool

	// HasWsEditSupport returns true if the exchange supports WS order editing.
	HasWsEditSupport() bool
}
