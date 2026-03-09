package types

import "time"

// FillEvent represents a trade execution event
type FillEvent struct {
	OrderID         string    `json:"orderId" bson:"orderId"`
	ClientOrderID   string    `json:"clientOrderId" bson:"clientOrderId"`
	Symbol          string    `json:"symbol" bson:"symbol"`
	Side            string    `json:"side" bson:"side"` // BUY or SELL
	Price           float64   `json:"price" bson:"price"`
	Quantity        float64   `json:"quantity" bson:"quantity"`
	Commission      float64   `json:"commission" bson:"commission"`
	CommissionAsset string    `json:"commissionAsset" bson:"commissionAsset"`
	TradeID         string    `json:"tradeId" bson:"tradeId"`
	Timestamp       time.Time `json:"timestamp" bson:"timestamp"`
	BotID           string    `json:"botId" bson:"botId"`
	Exchange        string    `json:"exchange" bson:"exchange"`
}

// AccountEvent represents an account balance update event
type AccountEvent struct {
	Balances  []Balance `json:"balances"`
	Timestamp time.Time `json:"timestamp"`
}

// Balance represents an asset balance
type Balance struct {
	Asset  string  `json:"asset"`
	Free   float64 `json:"free"`
	Locked float64 `json:"locked"`
}

// OrderEvent represents an order state change event
type OrderEvent struct {
	OrderID            string    `json:"orderId"`
	ClientOrderID      string    `json:"clientOrderId"`
	Symbol             string    `json:"symbol"`
	Side               string    `json:"side"`   // BUY or SELL
	Type               string    `json:"type"`   // LIMIT, MARKET, etc
	Status             string    `json:"status"` // NEW, FILLED, CANCELED, etc
	Price              float64   `json:"price"`
	Quantity           float64   `json:"quantity"`
	ExecutedQty        float64   `json:"executedQty"`
	CumulativeQuoteQty float64   `json:"cumulativeQuoteQty"`
	Timestamp          time.Time `json:"timestamp"`
}

// DealEvent represents a completed grid deal (matched buy-sell pair)
type DealEvent struct {
	BuyOrderID    string    `json:"buyOrderId"`
	SellOrderID   string    `json:"sellOrderId"`
	Symbol        string    `json:"symbol"`
	BuyPrice      float64   `json:"buyPrice"`
	SellPrice     float64   `json:"sellPrice"`
	Quantity      float64   `json:"quantity"`
	Profit        float64   `json:"profit"`
	ProfitPercent float64   `json:"profitPercent"`
	Timestamp     time.Time `json:"timestamp"`
}
