package mexc

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"

	"mm-platform-engine/internal/exchange"
)

// PlaceOrder places a new order on the exchange
func (c *Client) PlaceOrder(ctx context.Context, order *exchange.OrderRequest) (*exchange.Order, error) {
	params := url.Values{}
	params.Set("symbol", order.Symbol)
	params.Set("side", order.Side)
	params.Set("type", order.Type)
	params.Set("quantity", strconv.FormatFloat(order.Quantity, 'f', -1, 64))

	if order.Type == "LIMIT" {
		params.Set("price", strconv.FormatFloat(order.Price, 'f', -1, 64))
		params.Set("timeInForce", "GTC") // Good Till Cancel
	}

	if order.ClientOrderID != "" {
		params.Set("newClientOrderId", order.ClientOrderID)
	}

	params.Set("recvWindow", "5000")

	body, err := c.doRequest(ctx, "POST", "/api/v3/order", params, true)
	if err != nil {
		return nil, fmt.Errorf("failed to place order: %w", err)
	}

	var resp OrderResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("failed to unmarshal order response: %w", err)
	}

	return &exchange.Order{
		OrderID:       resp.OrderID,
		ClientOrderID: resp.ClientOrderID,
		Symbol:        resp.Symbol,
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

	// Build batch orders JSON
	type batchOrderItem struct {
		Symbol           string `json:"symbol"`
		Side             string `json:"side"`
		Type             string `json:"type"`
		Quantity         string `json:"quantity"`
		Price            string `json:"price,omitempty"`
		TimeInForce      string `json:"timeInForce,omitempty"`
		NewClientOrderID string `json:"newClientOrderId,omitempty"`
	}

	batchOrders := make([]batchOrderItem, len(orders))
	for i, order := range orders {
		item := batchOrderItem{
			Symbol:   order.Symbol,
			Side:     order.Side,
			Type:     order.Type,
			Quantity: strconv.FormatFloat(order.Quantity, 'f', -1, 64),
		}

		if order.Type == "LIMIT" {
			item.Price = strconv.FormatFloat(order.Price, 'f', -1, 64)
			item.TimeInForce = "GTC"
		}

		if order.ClientOrderID != "" {
			item.NewClientOrderID = order.ClientOrderID
		}

		batchOrders[i] = item
	}

	batchOrdersJSON, err := json.Marshal(batchOrders)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal batch orders: %w", err)
	}

	params := url.Values{}
	params.Set("batchOrders", string(batchOrdersJSON))
	params.Set("recvWindow", "5000")

	body, err := c.doRequest(ctx, "POST", "/api/v3/batchOrders", params, true)
	if err != nil {
		return nil, fmt.Errorf("failed to place batch orders: %w", err)
	}

	var batchResp []BatchOrderItemResponse
	if err := json.Unmarshal(body, &batchResp); err != nil {
		return nil, fmt.Errorf("failed to unmarshal batch response: %w", err)
	}

	// Convert to BatchOrderResponse
	result := &exchange.BatchOrderResponse{
		Orders: make([]*exchange.Order, 0, len(batchResp)),
		Errors: make([]string, 0),
	}

	for i, resp := range batchResp {
		if resp.Code != 0 {
			// Order failed
			errMsg := fmt.Sprintf("Order %d (%s) failed: %s", i, orders[i].ClientOrderID, resp.Msg)
			result.Errors = append(result.Errors, errMsg)
			continue
		}

		result.Orders = append(result.Orders, &exchange.Order{
			OrderID:       resp.OrderID,
			ClientOrderID: resp.ClientOrderID,
			Symbol:        resp.Symbol,
			Side:          orders[i].Side,
			Type:          orders[i].Type,
			Price:         orders[i].Price,
			Quantity:      orders[i].Quantity,
			Status:        "NEW",
		})
	}

	return result, nil
}

// CancelOrder cancels an existing order
func (c *Client) CancelOrder(ctx context.Context, symbol, orderID string) error {
	params := url.Values{}
	params.Set("symbol", symbol)
	params.Set("orderId", orderID)
	params.Set("recvWindow", "5000")

	_, err := c.doRequest(ctx, "DELETE", "/api/v3/order", params, true)
	if err != nil {
		return fmt.Errorf("failed to cancel order: %w", err)
	}

	return nil
}

// CancelAllOrders cancels all open orders for a symbol
func (c *Client) CancelAllOrders(ctx context.Context, symbol string) error {
	params := url.Values{}
	params.Set("symbol", symbol)
	params.Set("recvWindow", "5000")

	_, err := c.doRequest(ctx, "DELETE", "/api/v3/openOrders", params, true)
	if err != nil {
		return fmt.Errorf("failed to cancel all orders: %w", err)
	}

	return nil
}

// GetOpenOrders retrieves all open orders for a symbol
func (c *Client) GetOpenOrders(ctx context.Context, symbol string) ([]*exchange.Order, error) {
	params := url.Values{}
	params.Set("symbol", symbol)
	params.Set("recvWindow", "5000")

	body, err := c.doRequest(ctx, "GET", "/api/v3/openOrders", params, true)
	if err != nil {
		return nil, fmt.Errorf("failed to get open orders: %w", err)
	}

	var openOrders []OpenOrder
	if err := json.Unmarshal(body, &openOrders); err != nil {
		return nil, fmt.Errorf("failed to unmarshal open orders: %w", err)
	}

	// Convert to exchange.Order
	orders := make([]*exchange.Order, len(openOrders))
	for i, o := range openOrders {
		orders[i] = &exchange.Order{
			OrderID:       o.OrderID,
			ClientOrderID: o.ClientOrderID,
			Symbol:        o.Symbol,
			Side:          o.Side,
			Type:          o.Type,
			Status:        o.Status,
			Price:         o.Price,
			Quantity:      o.Quantity,
			ExecutedQty:   o.ExecutedQty,
		}
	}

	return orders, nil
}
