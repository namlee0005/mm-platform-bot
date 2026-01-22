package gate

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"mm-platform-engine/internal/exchange"
)

// GetExchangeInfo retrieves exchange information for a trading pair
func (c *Client) GetExchangeInfo(ctx context.Context, symbol string) (*exchange.ExchangeInfo, error) {
	gateSymbol := convertSymbol(symbol)
	endpoint := fmt.Sprintf("/spot/currency_pairs/%s", gateSymbol)

	body, err := c.doPublicRequest(ctx, "GET", endpoint, "")
	if err != nil {
		return nil, fmt.Errorf("failed to get currency pair info: %w", err)
	}

	var pair CurrencyPair
	if err := json.Unmarshal(body, &pair); err != nil {
		return nil, fmt.Errorf("failed to unmarshal currency pair: %w", err)
	}

	// Convert to exchange.ExchangeInfo format
	// Gate.io uses precision as number of decimal places
	// tickSize = 10^(-precision), stepSize = 10^(-amount_precision)

	// Calculate bid/ask multipliers from up_rate and down_rate
	// up_rate = 0.35 means price can go up 35%, so bidMultiplierUp = 1 + 0.35 = 1.35
	// down_rate = 0.35 means price can go down 35%, so askMultiplierDown = 1 - 0.35 = 0.65
	bidMultiplierUp := "1.35"
	askMultiplierDown := "0.65"
	if pair.UpRate != "" {
		bidMultiplierUp = fmt.Sprintf("%.2f", 1+parseFloatSafe(pair.UpRate))
	}
	if pair.DownRate != "" {
		askMultiplierDown = fmt.Sprintf("%.2f", 1-parseFloatSafe(pair.DownRate))
	}

	return &exchange.ExchangeInfo{
		ServerTime: time.Now().UnixMilli(),
		Symbols: []exchange.Symbol{
			{
				Symbol:              convertSymbolBack(pair.ID),
				BaseAsset:           pair.Base,
				BaseAssetPrecision:  pair.AmountPrecision,
				QuoteAsset:          pair.Quote,
				QuotePrecision:      pair.Precision,
				QuoteAssetPrecision: pair.Precision,
				Filters: []exchange.Filter{
					{
						FilterType:        "PERCENT_PRICE_BY_SIDE",
						BidMultiplierUp:   bidMultiplierUp,
						AskMultiplierDown: askMultiplierDown,
					},
					{
						FilterType:  "MIN_NOTIONAL",
						MinNotional: pair.MinQuoteAmount,
					},
				},
			},
		},
	}, nil
}

// parseFloatSafe parses a string to float64, returns 0 on error
func parseFloatSafe(s string) float64 {
	var f float64
	fmt.Sscanf(s, "%f", &f)
	return f
}

// GetDepth retrieves the order book for a trading pair
func (c *Client) GetDepth(ctx context.Context, symbol string) (*exchange.Depth, error) {
	gateSymbol := convertSymbol(symbol)
	query := fmt.Sprintf("currency_pair=%s&limit=20", gateSymbol)

	body, err := c.doPublicRequest(ctx, "GET", "/spot/order_book", query)
	if err != nil {
		return nil, fmt.Errorf("failed to get order book: %w", err)
	}

	var orderBook OrderBook
	if err := json.Unmarshal(body, &orderBook); err != nil {
		return nil, fmt.Errorf("failed to unmarshal order book: %w", err)
	}

	return &exchange.Depth{
		Bids: orderBook.Bids,
		Asks: orderBook.Asks,
	}, nil
}
