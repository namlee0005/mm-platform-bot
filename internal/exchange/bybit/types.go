package bybit

import "time"

// APIResponse is the base response wrapper for Bybit V5 API
type APIResponse struct {
	RetCode int    `json:"retCode"`
	RetMsg  string `json:"retMsg"`
	Time    int64  `json:"time"`
}

// OrderResponse represents a single order response
type OrderResponse struct {
	OrderID        string `json:"orderId"`
	OrderLinkID    string `json:"orderLinkId"`
	Symbol         string `json:"symbol"`
	Side           string `json:"side"`
	OrderType      string `json:"orderType"`
	Price          string `json:"price"`
	Qty            string `json:"qty"`
	Status         string `json:"orderStatus"`
	ExecQty        string `json:"cumExecQty"`
	ExecValue      string `json:"cumExecValue"`
	AvgPrice       string `json:"avgPrice"`
	TimeInForce    string `json:"timeInForce"`
	CreateTime     string `json:"createdTime"`
	UpdateTime     string `json:"updatedTime"`
	LeavesQty      string `json:"leavesQty"`
	LeavesValue    string `json:"leavesValue"`
	CumExecFee     string `json:"cumExecFee"`
	TriggerPrice   string `json:"triggerPrice"`
	TakeProfit     string `json:"takeProfit"`
	StopLoss       string `json:"stopLoss"`
	ReduceOnly     bool   `json:"reduceOnly"`
	CloseOnTrigger bool   `json:"closeOnTrigger"`
}

// PlaceOrderResponse represents the response from placing an order
type PlaceOrderResponse struct {
	APIResponse
	Result struct {
		OrderID     string `json:"orderId"`
		OrderLinkID string `json:"orderLinkId"`
	} `json:"result"`
}

// BatchPlaceOrderResponse represents the response from batch placing orders
type BatchPlaceOrderResponse struct {
	APIResponse
	Result struct {
		List []struct {
			Category    string `json:"category"`
			Symbol      string `json:"symbol"`
			OrderID     string `json:"orderId"`
			OrderLinkID string `json:"orderLinkId"`
			CreateAt    string `json:"createAt"`
		} `json:"list"`
	} `json:"result"`
	RetExtInfo struct {
		List []struct {
			Code int    `json:"code"`
			Msg  string `json:"msg"`
		} `json:"list"`
	} `json:"retExtInfo"`
}

// CancelOrderResponse represents the response from cancelling an order
type CancelOrderResponse struct {
	APIResponse
	Result struct {
		OrderID     string `json:"orderId"`
		OrderLinkID string `json:"orderLinkId"`
	} `json:"result"`
}

// CancelAllOrdersResponse represents the response from cancelling all orders
type CancelAllOrdersResponse struct {
	APIResponse
	Result struct {
		List []struct {
			OrderID     string `json:"orderId"`
			OrderLinkID string `json:"orderLinkId"`
		} `json:"list"`
		Success string `json:"success"` // "1" for success
	} `json:"result"`
}

// GetOpenOrdersResponse represents the response from getting open orders
type GetOpenOrdersResponse struct {
	APIResponse
	Result struct {
		List           []OrderResponse `json:"list"`
		NextPageCursor string          `json:"nextPageCursor"`
		Category       string          `json:"category"`
	} `json:"result"`
}

// WalletBalanceResponse represents the response from getting wallet balance
type WalletBalanceResponse struct {
	APIResponse
	Result struct {
		List []struct {
			AccountType string `json:"accountType"`
			Coin        []struct {
				Coin          string `json:"coin"`
				WalletBalance string `json:"walletBalance"`
				Locked        string `json:"locked"`
			} `json:"coin"`
		} `json:"list"`
	} `json:"result"`
}

// InstrumentsInfoResponse represents the response from getting instruments info
type InstrumentsInfoResponse struct {
	APIResponse
	Result struct {
		Category string `json:"category"`
		List     []struct {
			Symbol        string `json:"symbol"`
			BaseCoin      string `json:"baseCoin"`
			QuoteCoin     string `json:"quoteCoin"`
			Status        string `json:"status"`
			LotSizeFilter struct {
				BasePrecision  string `json:"basePrecision"`
				QuotePrecision string `json:"quotePrecision"`
				MinOrderQty    string `json:"minOrderQty"`
				MaxOrderQty    string `json:"maxOrderQty"`
				MinOrderAmt    string `json:"minOrderAmt"`
				MaxOrderAmt    string `json:"maxOrderAmt"`
			} `json:"lotSizeFilter"`
			PriceFilter struct {
				TickSize string `json:"tickSize"`
			} `json:"priceFilter"`
		} `json:"list"`
		NextPageCursor string `json:"nextPageCursor"`
	} `json:"result"`
}

// OrderbookResponse represents the response from getting orderbook
type OrderbookResponse struct {
	APIResponse
	Result struct {
		Symbol    string     `json:"s"`
		Bids      [][]string `json:"b"` // [price, size]
		Asks      [][]string `json:"a"` // [price, size]
		Timestamp int64      `json:"ts"`
		UpdateID  int64      `json:"u"`
	} `json:"result"`
}

// TickerResponse represents the response from getting ticker
type TickerResponse struct {
	APIResponse
	Result struct {
		Category string `json:"category"`
		List     []struct {
			Symbol      string `json:"symbol"`
			LastPrice   string `json:"lastPrice"`
			Bid1Price   string `json:"bid1Price"`
			Ask1Price   string `json:"ask1Price"`
			Volume24h   string `json:"volume24h"`
			Turnover24h string `json:"turnover24h"`
		} `json:"list"`
	} `json:"result"`
}

// OpenOrder represents an open order
type OpenOrder struct {
	OrderID       string  `json:"orderId"`
	ClientOrderID string  `json:"orderLinkId"`
	Symbol        string  `json:"symbol"`
	Side          string  `json:"side"`
	Type          string  `json:"orderType"`
	Price         float64 `json:"price"`
	Quantity      float64 `json:"qty"`
	ExecutedQty   float64 `json:"cumExecQty"`
	Status        string  `json:"orderStatus"`
	TimeInForce   string  `json:"timeInForce"`
}

// Account represents Bybit account information
type Account struct {
	Balances []Balance `json:"balances"`
}

// Balance represents an asset balance
type Balance struct {
	Asset  string  `json:"asset"`
	Free   float64 `json:"free"`
	Locked float64 `json:"locked"`
}

// Fill represents a trade execution
type Fill struct {
	TradeID         string    `json:"execId"`
	OrderID         string    `json:"orderId"`
	Symbol          string    `json:"symbol"`
	Side            string    `json:"side"`
	Price           float64   `json:"execPrice"`
	Quantity        float64   `json:"execQty"`
	Commission      float64   `json:"execFee"`
	CommissionAsset string    `json:"feeCurrency"`
	Time            time.Time `json:"execTime"`
}

// WSMessage represents a WebSocket message
type WSMessage struct {
	Op    string        `json:"op,omitempty"`
	Args  []interface{} `json:"args,omitempty"`
	Topic string        `json:"topic,omitempty"`
	Data  interface{}   `json:"data,omitempty"`
}

// WSAuthMessage represents a WebSocket auth response
type WSAuthMessage struct {
	Success bool   `json:"success"`
	RetMsg  string `json:"ret_msg"`
	Op      string `json:"op"`
	ConnID  string `json:"conn_id"`
}

// WSOrderUpdate represents a WebSocket order update
type WSOrderUpdate struct {
	Topic        string `json:"topic"`
	ID           string `json:"id"`
	CreationTime int64  `json:"creationTime"`
	Data         []struct {
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
		AvgPrice     string `json:"avgPrice"`
		TimeInForce  string `json:"timeInForce"`
		CreateTime   string `json:"createdTime"`
		UpdateTime   string `json:"updatedTime"`
		LeavesQty    string `json:"leavesQty"`
		LeavesValue  string `json:"leavesValue"`
		Category     string `json:"category"`
		ExecType     string `json:"execType"` // Trade, Funding, etc.
		FeeCurrency  string `json:"feeCurrency"`
		CumExecFee   string `json:"cumExecFee"`
	} `json:"data"`
}

// WSExecutionUpdate represents a WebSocket execution/trade update
type WSExecutionUpdate struct {
	Topic        string `json:"topic"`
	ID           string `json:"id"`
	CreationTime int64  `json:"creationTime"`
	Data         []struct {
		Category    string `json:"category"`
		Symbol      string `json:"symbol"`
		ExecID      string `json:"execId"`
		OrderID     string `json:"orderId"`
		OrderLinkID string `json:"orderLinkId"`
		Side        string `json:"side"`
		OrderType   string `json:"orderType"`
		OrderPrice  string `json:"orderPrice"`
		OrderQty    string `json:"orderQty"`
		ExecPrice   string `json:"execPrice"`
		ExecQty     string `json:"execQty"`
		ExecFee     string `json:"execFee"`
		ExecTime    string `json:"execTime"`
		FeeCurrency string `json:"feeCurrency"`
		IsMaker     bool   `json:"isMaker"`
		ExecType    string `json:"execType"`
		LeavesQty   string `json:"leavesQty"`
		ClosedSize  string `json:"closedSize"`
	} `json:"data"`
}

// WSWalletUpdate represents a WebSocket wallet update
type WSWalletUpdate struct {
	Topic        string `json:"topic"`
	ID           string `json:"id"`
	CreationTime int64  `json:"creationTime"`
	Data         []struct {
		AccountType string `json:"accountType"`
		Coin        []struct {
			Coin                string `json:"coin"`
			Free                string `json:"free"`
			WalletBalance       string `json:"walletBalance"`
			Locked              string `json:"locked"`
			AvailableToWithdraw string `json:"availableToWithdraw"`
		} `json:"coin"`
	} `json:"data"`
}

// mapOrderStatus converts Bybit order status to standard status
func mapOrderStatus(status string) string {
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

// mapSide converts Bybit side to standard side
func mapSide(side string) string {
	switch side {
	case "Buy":
		return "BUY"
	case "Sell":
		return "SELL"
	default:
		return side
	}
}

// mapOrderType converts Bybit order type to standard type
func mapOrderType(orderType string) string {
	switch orderType {
	case "Limit":
		return "LIMIT"
	case "Market":
		return "MARKET"
	default:
		return orderType
	}
}
