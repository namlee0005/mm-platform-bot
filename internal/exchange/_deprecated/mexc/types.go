package mexc

import "time"

// OpenOrder represents an open order on MEXC
type OpenOrder struct {
	OrderID       string  `json:"orderId"`
	ClientOrderID string  `json:"clientOrderId"`
	Symbol        string  `json:"symbol"`
	Side          string  `json:"side"`
	Type          string  `json:"type"`
	Price         float64 `json:"price,string"`
	Quantity      float64 `json:"origQty,string"`
	ExecutedQty   float64 `json:"executedQty,string"`
	Status        string  `json:"status"`
	TimeInForce   string  `json:"timeInForce"`
}

// Fill represents a trade execution
type Fill struct {
	TradeID         string    `json:"id"`
	OrderID         string    `json:"orderId"`
	Symbol          string    `json:"symbol"`
	Side            string    `json:"side"`
	Price           float64   `json:"price,string"`
	Quantity        float64   `json:"qty,string"`
	Commission      float64   `json:"commission,string"`
	CommissionAsset string    `json:"commissionAsset"`
	Time            time.Time `json:"time"`
}

// Account represents MEXC account information
type Account struct {
	MakerCommission  int       `json:"makerCommission"`
	TakerCommission  int       `json:"takerCommission"`
	BuyerCommission  int       `json:"buyerCommission"`
	SellerCommission int       `json:"sellerCommission"`
	CanTrade         bool      `json:"canTrade"`
	CanWithdraw      bool      `json:"canWithdraw"`
	CanDeposit       bool      `json:"canDeposit"`
	UpdateTime       int64     `json:"updateTime"`
	Balances         []Balance `json:"balances"`
}

// Balance represents an asset balance
type Balance struct {
	Asset  string  `json:"asset"`
	Free   float64 `json:"free,string"`
	Locked float64 `json:"locked,string"`
}

// OrderResponse represents the response when placing an order
type OrderResponse struct {
	Symbol        string `json:"symbol"`
	OrderID       string `json:"orderId"`
	ClientOrderID string `json:"clientOrderId"`
	TransactTime  int64  `json:"transactTime"`
}

// BatchOrderItemResponse represents a single order response in a batch order request
type BatchOrderItemResponse struct {
	Code          int    `json:"code"`
	Msg           string `json:"msg"`
	Symbol        string `json:"symbol"`
	OrderID       string `json:"orderId"`
	ClientOrderID string `json:"clientOrderId"`
	TransactTime  int64  `json:"transactTime"`
}

// ListenKeyResponse represents the response when creating a listen key
type ListenKeyResponse struct {
	ListenKey string `json:"listenKey"`
}
