package mexc

import (
	"context"
	"encoding/json"
	"net/url"
)

// GetBalances retrieves account balances (REST fallback)
func (c *Client) GetBalances(ctx context.Context) ([]Balance, error) {
	params := url.Values{}
	params.Set("recvWindow", "5000")

	body, err := c.doRequest(ctx, "GET", "/api/v3/account", params, true)
	if err != nil {
		return nil, err
	}

	var account Account
	if err := json.Unmarshal(body, &account); err != nil {
		return nil, err
	}

	return account.Balances, nil
}

// GetBalance retrieves balance for a specific asset
func (c *Client) GetBalance(ctx context.Context, asset string) (*Balance, error) {
	balances, err := c.GetBalances(ctx)
	if err != nil {
		return nil, err
	}

	for _, balance := range balances {
		if balance.Asset == asset {
			return &balance, nil
		}
	}

	// Return zero balance if asset not found
	return &Balance{
		Asset:  asset,
		Free:   0,
		Locked: 0,
	}, nil
}
