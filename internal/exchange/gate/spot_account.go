package gate

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	"mm-platform-engine/internal/exchange"
)

// GetAccount retrieves account balances from Gate.io
func (c *Client) GetAccount(ctx context.Context) (*exchange.Account, error) {
	body, err := c.doRequest(ctx, "GET", "/spot/accounts", "", "")
	if err != nil {
		return nil, fmt.Errorf("failed to get account: %w", err)
	}

	var accounts []SpotAccount
	if err := json.Unmarshal(body, &accounts); err != nil {
		return nil, fmt.Errorf("failed to unmarshal accounts: %w", err)
	}

	// Convert to exchange.Account
	balances := make([]exchange.Balance, 0, len(accounts))
	for _, acc := range accounts {
		available, _ := strconv.ParseFloat(acc.Available, 64)
		locked, _ := strconv.ParseFloat(acc.Locked, 64)

		// Only include non-zero balances
		if available > 0 || locked > 0 {
			balances = append(balances, exchange.Balance{
				Asset:  acc.Currency,
				Free:   available,
				Locked: locked,
			})
		}
	}

	return &exchange.Account{Balances: balances}, nil
}
