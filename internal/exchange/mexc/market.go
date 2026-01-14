package mexc

import (
	"context"
	"encoding/json"
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
	params.Set("limit", "5")

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
