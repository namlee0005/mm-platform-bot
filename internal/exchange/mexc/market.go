package mexc

import (
	"context"
	"encoding/json"
	"fmt"
	"mm-platform-engine/internal/exchange"
	"net/url"
)

// GetExchangeInfo retrieves market information
func (c *Client) GetExchangeInfo(ctx context.Context, symbol string) (*exchange.ExchangeInfo, error) {
	params := url.Values{}
	params.Set("symbol", symbol)

	body, err := c.doRequest(ctx, "GET", "/api/v3/exchangeInfo", params, false)
	if err != nil {
		return nil, err
	}

	var exchangeInfo *exchange.ExchangeInfo
	if err := json.Unmarshal(body, &exchangeInfo); err != nil {
		return nil, err
	}

	return exchangeInfo, nil
}

func (c *Client) GetDepth(ctx context.Context, symbol string) (*exchange.Depth, error) {
	params := url.Values{}
	params.Set("symbol", symbol)
	params.Set("limit", "20") // More levels for sweep detection

	body, err := c.doRequest(ctx, "GET", "/api/v3/depth", params, false)
	if err != nil {
		return nil, err
	}

	var depth *exchange.Depth
	if err := json.Unmarshal(body, &depth); err != nil {
		return nil, err
	}
	return depth, nil
}

// GetTicker returns the last trade price for a symbol
func (c *Client) GetTicker(ctx context.Context, symbol string) (float64, error) {
	params := url.Values{}
	params.Set("symbol", symbol)

	body, err := c.doRequest(ctx, "GET", "/api/v3/ticker/price", params, false)
	if err != nil {
		return 0, err
	}

	var ticker struct {
		Price string `json:"price"`
	}
	if err := json.Unmarshal(body, &ticker); err != nil {
		return 0, err
	}

	var price float64
	_, _ = fmt.Sscanf(ticker.Price, "%f", &price)
	return price, nil
}
