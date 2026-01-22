package gate

// APIError represents a Gate.io API error response
type APIError struct {
	Label   string `json:"label"`
	Message string `json:"message"`
}

// SpotAccount represents a spot account balance
type SpotAccount struct {
	Currency  string `json:"currency"`
	Available string `json:"available"`
	Locked    string `json:"locked"`
}

// CurrencyPair represents a trading pair info
type CurrencyPair struct {
	ID              string `json:"id"`               // Trading pair e.g. "BTC_USDT"
	Base            string `json:"base"`             // Base currency
	BaseName        string `json:"base_name"`        // Base currency name
	Quote           string `json:"quote"`            // Quote currency
	QuoteName       string `json:"quote_name"`       // Quote currency name
	Fee             string `json:"fee"`              // Trading fee rate (%)
	MinBaseAmount   string `json:"min_base_amount"`  // Minimum base amount
	MinQuoteAmount  string `json:"min_quote_amount"` // Minimum quote amount (notional)
	MaxQuoteAmount  string `json:"max_quote_amount"` // Maximum quote amount
	AmountPrecision int    `json:"amount_precision"` // Base currency precision (decimal places)
	Precision       int    `json:"precision"`        // Quote currency precision (decimal places)
	TradeStatus     string `json:"trade_status"`     // Trading status: tradable, etc.
	SellStart       int64  `json:"sell_start"`       // Sell start timestamp
	BuyStart        int64  `json:"buy_start"`        // Buy start timestamp
	UpRate          string `json:"up_rate"`          // Price up limit rate
	DownRate        string `json:"down_rate"`        // Price down limit rate
}

// OrderBook represents the order book depth
type OrderBook struct {
	ID      int64      `json:"id"`      // Order book ID
	Current int64      `json:"current"` // Current timestamp in ms
	Update  int64      `json:"update"`  // Last update timestamp in ms
	Asks    [][]string `json:"asks"`    // Asks [price, amount]
	Bids    [][]string `json:"bids"`    // Bids [price, amount]
}

// SpotOrder represents an order response from Gate.io
type SpotOrder struct {
	ID                 string `json:"id"`
	Text               string `json:"text"`                 // User-defined order ID (client order id)
	CreateTime         string `json:"create_time"`          // Creation time
	UpdateTime         string `json:"update_time"`          // Last update time
	CreateTimeMs       int64  `json:"create_time_ms"`       // Creation time in ms
	UpdateTimeMs       int64  `json:"update_time_ms"`       // Update time in ms
	Status             string `json:"status"`               // Order status: open, closed, cancelled
	CurrencyPair       string `json:"currency_pair"`        // Trading pair
	Type               string `json:"type"`                 // Order type: limit, market
	Account            string `json:"account"`              // Account type: spot, margin
	Side               string `json:"side"`                 // Order side: buy, sell
	Amount             string `json:"amount"`               // Trade amount
	Price              string `json:"price"`                // Order price
	TimeInForce        string `json:"time_in_force"`        // Time in force: gtc, ioc, poc, fok
	Iceberg            string `json:"iceberg"`              // Iceberg amount
	Left               string `json:"left"`                 // Remaining amount
	FillPrice          string `json:"fill_price"`           // Fill price
	FilledTotal        string `json:"filled_total"`         // Filled total (quote)
	AvgDealPrice       string `json:"avg_deal_price"`       // Average deal price
	Fee                string `json:"fee"`                  // Fee
	FeeCurrency        string `json:"fee_currency"`         // Fee currency
	PointFee           string `json:"point_fee"`            // Point fee
	GtFee              string `json:"gt_fee"`               // GT fee
	GtMakerFee         string `json:"gt_maker_fee"`         // GT maker fee
	GtTakerFee         string `json:"gt_taker_fee"`         // GT taker fee
	GtDiscount         bool   `json:"gt_discount"`          // Whether GT fee discount is used
	RebatedFee         string `json:"rebated_fee"`          // Rebated fee
	RebatedFeeCurrency string `json:"rebated_fee_currency"` // Rebated fee currency
	StpId              int    `json:"stp_id"`               // STP group ID
	StpAct             string `json:"stp_act"`              // STP action
	FinishAs           string `json:"finish_as"`            // How the order was finished
}

// BatchOrderRequest represents a single order in batch request
type BatchOrderRequest struct {
	Text         string `json:"text,omitempty"`          // Client order ID
	CurrencyPair string `json:"currency_pair"`           // Trading pair
	Type         string `json:"type,omitempty"`          // Order type: limit, market
	Account      string `json:"account,omitempty"`       // Account type: spot
	Side         string `json:"side"`                    // Order side: buy, sell
	Amount       string `json:"amount"`                  // Order amount
	Price        string `json:"price,omitempty"`         // Order price (required for limit)
	TimeInForce  string `json:"time_in_force,omitempty"` // gtc, ioc, poc, fok
}

// BatchOrderResponse represents a single order response in batch
type BatchOrderResponse struct {
	OrderID   string `json:"order_id,omitempty"`
	Label     string `json:"label,omitempty"`   // Error label
	Message   string `json:"message,omitempty"` // Error message
	Succeeded bool   `json:"succeeded"`
}

// Ticker represents market ticker data
type Ticker struct {
	CurrencyPair     string `json:"currency_pair"`
	Last             string `json:"last"`
	LowestAsk        string `json:"lowest_ask"`
	HighestBid       string `json:"highest_bid"`
	ChangePercentage string `json:"change_percentage"`
	BaseVolume       string `json:"base_volume"`
	QuoteVolume      string `json:"quote_volume"`
	High24h          string `json:"high_24h"`
	Low24h           string `json:"low_24h"`
}

// WebSocket message types

// WSMessage represents a WebSocket message
type WSMessage struct {
	Time    int64       `json:"time"`
	Channel string      `json:"channel"`
	Event   string      `json:"event"`
	Payload interface{} `json:"payload,omitempty"`
	Result  interface{} `json:"result,omitempty"`
	Error   *WSError    `json:"error,omitempty"`
}

// WSError represents a WebSocket error
type WSError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// WSAuthPayload represents WebSocket authentication payload
type WSAuthPayload struct {
	ApiKey    string `json:"api_key"`
	Signature string `json:"signature"`
	Timestamp string `json:"timestamp"`
}

// WSSubscribeRequest represents a WebSocket subscription request
type WSSubscribeRequest struct {
	Time    int64          `json:"time"`
	Channel string         `json:"channel"`
	Event   string         `json:"event"`
	Payload []string       `json:"payload,omitempty"`
	Auth    *WSAuthPayload `json:"auth,omitempty"`
}

// WSOrderUpdate represents an order update from WebSocket
type WSOrderUpdate struct {
	ID           string `json:"id"`
	Text         string `json:"text"`
	CreateTime   string `json:"create_time"`
	UpdateTime   string `json:"update_time"`
	CurrencyPair string `json:"currency_pair"`
	Type         string `json:"type"`
	Account      string `json:"account"`
	Side         string `json:"side"`
	Amount       string `json:"amount"`
	Price        string `json:"price"`
	TimeInForce  string `json:"time_in_force"`
	Left         string `json:"left"`
	FilledTotal  string `json:"filled_total"`
	AvgDealPrice string `json:"avg_deal_price"`
	Fee          string `json:"fee"`
	FeeCurrency  string `json:"fee_currency"`
	Status       string `json:"status"`
	Event        string `json:"event"` // put, update, finish
	FinishAs     string `json:"finish_as"`
}

// WSUserTrade represents a user trade from WebSocket
type WSUserTrade struct {
	ID           int64  `json:"id"`
	UserID       int64  `json:"user_id"`
	OrderID      string `json:"order_id"`
	CurrencyPair string `json:"currency_pair"`
	CreateTime   int64  `json:"create_time"`
	CreateTimeMs string `json:"create_time_ms"`
	Side         string `json:"side"`
	Amount       string `json:"amount"`
	Role         string `json:"role"` // taker, maker
	Price        string `json:"price"`
	Fee          string `json:"fee"`
	FeeCurrency  string `json:"fee_currency"`
	PointFee     string `json:"point_fee"`
	GtFee        string `json:"gt_fee"`
	Text         string `json:"text"`
}

// WSBalanceUpdate represents a balance update from WebSocket
type WSBalanceUpdate struct {
	Timestamp   string `json:"timestamp"`
	TimestampMs string `json:"timestamp_ms"`
	User        string `json:"user"`
	Currency    string `json:"currency"`
	Change      string `json:"change"`
	Total       string `json:"total"`
	Available   string `json:"available"`
}
