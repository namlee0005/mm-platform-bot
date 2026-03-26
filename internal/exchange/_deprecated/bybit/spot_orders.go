package bybit

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"mm-platform-engine/internal/exchange"
)

// formatFloat formats a float with proper precision, avoiding floating point artifacts
func formatFloat(f float64, precision int) string {
	if precision < 0 {
		precision = 8
	}
	s := strconv.FormatFloat(f, 'f', precision, 64)
	// Trim trailing zeros after decimal point
	if strings.Contains(s, ".") {
		s = strings.TrimRight(s, "0")
		s = strings.TrimRight(s, ".")
	}
	return s
}

// PlaceOrder places a new order on the exchange
func (c *Client) PlaceOrder(ctx context.Context, order *exchange.OrderRequest) (*exchange.Order, error) {
	params := map[string]string{
		"category":    "spot",
		"symbol":      order.Symbol,
		"side":        toBybitSide(order.Side),
		"orderType":   toBybitOrderType(order.Type),
		"qty":         formatFloat(order.Quantity, 8),
		"timeInForce": "GTC",
	}

	if order.Type == "LIMIT" {
		params["price"] = formatFloat(order.Price, 8)
	}

	if order.ClientOrderID != "" {
		params["orderLinkId"] = order.ClientOrderID
	}

	// Debug: log order params
	log.Printf("[Bybit] PlaceOrder: symbol=%s side=%s type=%s qty=%s price=%s clientId=%s",
		params["symbol"], params["side"], params["orderType"], params["qty"],
		params["price"], params["orderLinkId"])

	body, err := c.doRequest(ctx, "POST", "/v5/order/create", params)
	if err != nil {
		return nil, fmt.Errorf("failed to place order: %w", err)
	}

	var resp PlaceOrderResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("failed to unmarshal order response: %w", err)
	}

	return &exchange.Order{
		OrderID:       resp.Result.OrderID,
		ClientOrderID: resp.Result.OrderLinkID,
		Symbol:        order.Symbol,
		Side:          order.Side,
		Type:          order.Type,
		Price:         order.Price,
		Quantity:      order.Quantity,
		Status:        "NEW",
	}, nil
}

// BatchPlaceOrders places multiple orders in a single request
func (c *Client) BatchPlaceOrders(ctx context.Context, orders []*exchange.OrderRequest) (*exchange.BatchOrderResponse, error) {
	if len(orders) == 0 {
		return &exchange.BatchOrderResponse{
			Orders: []*exchange.Order{},
			Errors: []string{},
		}, nil
	}

	// Build batch orders
	type batchOrderItem struct {
		Symbol      string `json:"symbol"`
		Side        string `json:"side"`
		OrderType   string `json:"orderType"`
		Qty         string `json:"qty"`
		Price       string `json:"price,omitempty"`
		TimeInForce string `json:"timeInForce"`
		OrderLinkID string `json:"orderLinkId,omitempty"`
	}

	batchOrders := make([]batchOrderItem, 0, len(orders))
	for _, order := range orders {
		// Skip invalid orders (Inf, NaN, zero)
		if order.Quantity <= 0 || order.Quantity != order.Quantity || order.Price < 0 || order.Price != order.Price {
			log.Printf("[Bybit] Skipping invalid order: qty=%.8f price=%.8f clientId=%s",
				order.Quantity, order.Price, order.ClientOrderID)
			continue
		}

		item := batchOrderItem{
			Symbol:      order.Symbol,
			Side:        toBybitSide(order.Side),
			OrderType:   toBybitOrderType(order.Type),
			Qty:         formatFloat(order.Quantity, 8),
			TimeInForce: "GTC",
		}

		if order.Type == "LIMIT" {
			if order.Price <= 0 {
				log.Printf("[Bybit] Skipping order with zero price: clientId=%s", order.ClientOrderID)
				continue
			}
			item.Price = formatFloat(order.Price, 8)
		}

		if order.ClientOrderID != "" {
			item.OrderLinkID = order.ClientOrderID
		}

		batchOrders = append(batchOrders, item)
	}

	// Bybit batch order endpoint expects JSON body with "category" and "request" array
	requestJSON, err := json.Marshal(map[string]interface{}{
		"category": "spot",
		"request":  batchOrders,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to marshal batch orders: %w", err)
	}

	// Debug: log batch order request
	log.Printf("[Bybit] BatchPlaceOrders: %d orders, JSON=%s", len(batchOrders), string(requestJSON))

	// For batch orders, we need to send raw JSON
	params := map[string]string{}
	body, err := c.doRequestRaw(ctx, "POST", "/v5/order/create-batch", string(requestJSON))
	if err != nil {
		return nil, fmt.Errorf("failed to place batch orders: %w", err)
	}

	var batchResp BatchPlaceOrderResponse
	if err := json.Unmarshal(body, &batchResp); err != nil {
		// If batch failed, try placing orders individually
		return c.placeBatchOrdersSequentially(ctx, orders, params)
	}

	// Convert to BatchOrderResponse
	result := &exchange.BatchOrderResponse{
		Orders: make([]*exchange.Order, 0, len(batchResp.Result.List)),
		Errors: make([]string, 0),
	}

	// Check for errors in retExtInfo
	for i, extInfo := range batchResp.RetExtInfo.List {
		if extInfo.Code != 0 {
			clientID := ""
			if i < len(orders) {
				clientID = orders[i].ClientOrderID
			}
			errMsg := fmt.Sprintf("Order %d (%s) failed: %s", i, clientID, extInfo.Msg)
			result.Errors = append(result.Errors, errMsg)
		}
	}

	for i, orderResult := range batchResp.Result.List {
		if orderResult.OrderID != "" {
			result.Orders = append(result.Orders, &exchange.Order{
				OrderID:       orderResult.OrderID,
				ClientOrderID: orderResult.OrderLinkID,
				Symbol:        orderResult.Symbol,
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

// placeBatchOrdersSequentially places orders one by one as fallback
func (c *Client) placeBatchOrdersSequentially(ctx context.Context, orders []*exchange.OrderRequest, _ map[string]string) (*exchange.BatchOrderResponse, error) {
	result := &exchange.BatchOrderResponse{
		Orders: make([]*exchange.Order, 0, len(orders)),
		Errors: make([]string, 0),
	}

	for i, order := range orders {
		placedOrder, err := c.PlaceOrder(ctx, order)
		if err != nil {
			errMsg := fmt.Sprintf("Order %d (%s) failed: %v", i, order.ClientOrderID, err)
			result.Errors = append(result.Errors, errMsg)
			continue
		}
		result.Orders = append(result.Orders, placedOrder)
	}

	return result, nil
}

// doRequestRaw performs an authenticated HTTP request with raw JSON body
func (c *Client) doRequestRaw(ctx context.Context, method, endpoint, jsonBody string) ([]byte, error) {
	timestamp := strconv.FormatInt(time.Now().UnixMilli(), 10)
	signature := c.sign(timestamp, jsonBody)

	reqURL := c.baseURL + endpoint
	req, err := http.NewRequestWithContext(ctx, method, reqURL, strings.NewReader(jsonBody))
	if err != nil {
		return nil, err
	}

	req.Header.Set("X-BAPI-API-KEY", c.apiKey)
	req.Header.Set("X-BAPI-SIGN", signature)
	req.Header.Set("X-BAPI-TIMESTAMP", timestamp)
	req.Header.Set("X-BAPI-RECV-WINDOW", recvWindow)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
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

	var baseResp APIResponse
	if err := json.Unmarshal(body, &baseResp); err != nil {
		return nil, fmt.Errorf("failed to unmarshal response: %w", err)
	}

	if baseResp.RetCode != 0 {
		return nil, fmt.Errorf("Bybit API error: code=%d msg=%s", baseResp.RetCode, baseResp.RetMsg)
	}

	return body, nil
}

// CancelOrder cancels an existing order
func (c *Client) CancelOrder(ctx context.Context, symbol, orderID string) error {
	params := map[string]string{
		"category": "spot",
		"symbol":   symbol,
		"orderId":  orderID,
	}

	_, err := c.doRequest(ctx, "POST", "/v5/order/cancel", params)
	if err != nil {
		return fmt.Errorf("failed to cancel order: %w", err)
	}

	return nil
}

// CancelAllOrders cancels all open orders for a symbol
func (c *Client) CancelAllOrders(ctx context.Context, symbol string) error {
	params := map[string]string{
		"category": "spot",
		"symbol":   symbol,
	}

	_, err := c.doRequest(ctx, "POST", "/v5/order/cancel-all", params)
	if err != nil {
		return fmt.Errorf("failed to cancel all orders: %w", err)
	}

	return nil
}

// GetOpenOrders retrieves all open orders for a symbol
func (c *Client) GetOpenOrders(ctx context.Context, symbol string) ([]*exchange.Order, error) {
	params := map[string]string{
		"category": "spot",
		"symbol":   symbol,
		"openOnly": "0", // 0 = return unfilled/partially filled orders
		"limit":    "50",
	}

	body, err := c.doRequest(ctx, "GET", "/v5/order/realtime", params)
	if err != nil {
		return nil, fmt.Errorf("failed to get open orders: %w", err)
	}

	var resp GetOpenOrdersResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("failed to unmarshal open orders: %w", err)
	}

	orders := make([]*exchange.Order, len(resp.Result.List))
	for i, o := range resp.Result.List {
		orders[i] = &exchange.Order{
			OrderID:       o.OrderID,
			ClientOrderID: o.OrderLinkID,
			Symbol:        o.Symbol,
			Side:          mapSide(o.Side),
			Type:          mapOrderType(o.OrderType),
			Status:        mapOrderStatus(o.Status),
			Price:         parseFloat(o.Price),
			Quantity:      parseFloat(o.Qty),
			ExecutedQty:   parseFloat(o.ExecQty),
		}
	}

	return orders, nil
}

// toBybitSide converts standard side to Bybit side
func toBybitSide(side string) string {
	switch side {
	case "BUY":
		return "Buy"
	case "SELL":
		return "Sell"
	default:
		return side
	}
}

// toBybitOrderType converts standard order type to Bybit order type
func toBybitOrderType(orderType string) string {
	switch orderType {
	case "LIMIT":
		return "Limit"
	case "MARKET":
		return "Market"
	default:
		return orderType
	}
}
