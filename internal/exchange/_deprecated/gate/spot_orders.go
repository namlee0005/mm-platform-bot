package gate

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"mm-platform-engine/internal/exchange"
)

// convertSymbol converts symbol format from "BTCUSDT" to "BTC_USDT"
func convertSymbol(symbol string) string {
	// If already contains underscore, return as is
	if strings.Contains(symbol, "_") {
		return symbol
	}
	// Common quote currencies
	quotes := []string{"USDT", "USDC", "BTC", "ETH", "USD"}
	for _, quote := range quotes {
		if strings.HasSuffix(symbol, quote) {
			base := strings.TrimSuffix(symbol, quote)
			return base + "_" + quote
		}
	}
	return symbol
}

// convertSymbolBack converts symbol format from "BTC_USDT" to "BTCUSDT"
func convertSymbolBack(symbol string) string {
	return strings.ReplaceAll(symbol, "_", "")
}

// PlaceOrder places a new order on Gate.io
func (c *Client) PlaceOrder(ctx context.Context, order *exchange.OrderRequest) (*exchange.Order, error) {
	// Build order request
	gateOrder := map[string]interface{}{
		"currency_pair": convertSymbol(order.Symbol),
		"side":          strings.ToLower(order.Side),
		"amount":        strconv.FormatFloat(order.Quantity, 'f', -1, 64),
		"account":       "spot",
	}

	if order.Type == "LIMIT" {
		gateOrder["type"] = "limit"
		gateOrder["price"] = strconv.FormatFloat(order.Price, 'f', -1, 64)
		gateOrder["time_in_force"] = "gtc"
	} else {
		gateOrder["type"] = "market"
	}

	if order.ClientOrderID != "" {
		gateOrder["text"] = "t-" + order.ClientOrderID
	}

	bodyBytes, err := json.Marshal(gateOrder)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal order: %w", err)
	}

	respBody, err := c.doRequest(ctx, "POST", "/spot/orders", "", string(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("failed to place order: %w", err)
	}

	var resp SpotOrder
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return nil, fmt.Errorf("failed to unmarshal order response: %w", err)
	}

	return &exchange.Order{
		OrderID:       resp.ID,
		ClientOrderID: strings.TrimPrefix(resp.Text, "t-"),
		Symbol:        convertSymbolBack(resp.CurrencyPair),
		Side:          strings.ToUpper(resp.Side),
		Type:          strings.ToUpper(resp.Type),
		Price:         order.Price,
		Quantity:      order.Quantity,
		Status:        convertOrderStatus(resp.Status),
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

	// Build batch order requests
	batchOrders := make([]map[string]interface{}, len(orders))
	for i, order := range orders {
		gateOrder := map[string]interface{}{
			"currency_pair": convertSymbol(order.Symbol),
			"side":          strings.ToLower(order.Side),
			"amount":        strconv.FormatFloat(order.Quantity, 'f', -1, 64),
			"account":       "spot",
		}

		if order.Type == "LIMIT" {
			gateOrder["type"] = "limit"
			gateOrder["price"] = strconv.FormatFloat(order.Price, 'f', -1, 64)
			gateOrder["time_in_force"] = "gtc"
		} else {
			gateOrder["type"] = "market"
		}

		if order.ClientOrderID != "" {
			gateOrder["text"] = "t-" + order.ClientOrderID
		}

		batchOrders[i] = gateOrder
	}

	bodyBytes, err := json.Marshal(batchOrders)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal batch orders: %w", err)
	}

	respBody, err := c.doRequest(ctx, "POST", "/spot/batch_orders", "", string(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("failed to place batch orders: %w", err)
	}

	var batchResp []SpotOrder
	if err := json.Unmarshal(respBody, &batchResp); err != nil {
		return nil, fmt.Errorf("failed to unmarshal batch response: %w", err)
	}

	// Convert to BatchOrderResponse
	result := &exchange.BatchOrderResponse{
		Orders: make([]*exchange.Order, 0, len(batchResp)),
		Errors: make([]string, 0),
	}

	for i, resp := range batchResp {
		// Check if order has ID (success) or not (failure)
		if resp.ID == "" {
			errMsg := fmt.Sprintf("Order %d (%s) failed", i, orders[i].ClientOrderID)
			result.Errors = append(result.Errors, errMsg)
			continue
		}

		result.Orders = append(result.Orders, &exchange.Order{
			OrderID:       resp.ID,
			ClientOrderID: strings.TrimPrefix(resp.Text, "t-"),
			Symbol:        convertSymbolBack(resp.CurrencyPair),
			Side:          strings.ToUpper(resp.Side),
			Type:          strings.ToUpper(resp.Type),
			Price:         orders[i].Price,
			Quantity:      orders[i].Quantity,
			Status:        convertOrderStatus(resp.Status),
		})
	}

	return result, nil
}

// CancelOrder cancels an existing order
func (c *Client) CancelOrder(ctx context.Context, symbol, orderID string) error {
	endpoint := fmt.Sprintf("/spot/orders/%s", orderID)
	query := fmt.Sprintf("currency_pair=%s", convertSymbol(symbol))

	_, err := c.doRequest(ctx, "DELETE", endpoint, query, "")
	if err != nil {
		return fmt.Errorf("failed to cancel order: %w", err)
	}

	return nil
}

// CancelAllOrders cancels all open orders for a symbol
func (c *Client) CancelAllOrders(ctx context.Context, symbol string) error {
	query := fmt.Sprintf("currency_pair=%s&account=spot", convertSymbol(symbol))

	_, err := c.doRequest(ctx, "DELETE", "/spot/orders", query, "")
	if err != nil {
		return fmt.Errorf("failed to cancel all orders: %w", err)
	}

	return nil
}

// GetOpenOrders retrieves all open orders for a symbol
func (c *Client) GetOpenOrders(ctx context.Context, symbol string) ([]*exchange.Order, error) {
	query := fmt.Sprintf("currency_pair=%s&status=open&account=spot", convertSymbol(symbol))

	body, err := c.doRequest(ctx, "GET", "/spot/orders", query, "")
	if err != nil {
		return nil, fmt.Errorf("failed to get open orders: %w", err)
	}

	var openOrders []SpotOrder
	if err := json.Unmarshal(body, &openOrders); err != nil {
		return nil, fmt.Errorf("failed to unmarshal open orders: %w", err)
	}

	// Convert to exchange.Order
	orders := make([]*exchange.Order, len(openOrders))
	for i, o := range openOrders {
		price, _ := strconv.ParseFloat(o.Price, 64)
		amount, _ := strconv.ParseFloat(o.Amount, 64)
		left, _ := strconv.ParseFloat(o.Left, 64)
		executedQty := amount - left
		filledTotal, _ := strconv.ParseFloat(o.FilledTotal, 64)

		orders[i] = &exchange.Order{
			OrderID:            o.ID,
			ClientOrderID:      strings.TrimPrefix(o.Text, "t-"),
			Symbol:             convertSymbolBack(o.CurrencyPair),
			Side:               strings.ToUpper(o.Side),
			Type:               strings.ToUpper(o.Type),
			Status:             convertOrderStatus(o.Status),
			Price:              price,
			Quantity:           amount,
			ExecutedQty:        executedQty,
			CumulativeQuoteQty: filledTotal,
		}
	}

	return orders, nil
}

// convertOrderStatus converts Gate.io order status to standard status
func convertOrderStatus(status string) string {
	switch status {
	case "open":
		return "NEW"
	case "closed":
		return "FILLED"
	case "cancelled":
		return "CANCELED"
	default:
		return strings.ToUpper(status)
	}
}
