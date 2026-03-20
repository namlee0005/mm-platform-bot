package bybit

import (
	"context"
	"encoding/json"
	"log"
	"strconv"
	"strings"

	"mm-platform-engine/internal/exchange"
)

// GetExchangeInfo retrieves market information
func (c *Client) GetExchangeInfo(ctx context.Context, symbol string) (*exchange.ExchangeInfo, error) {
	params := map[string]string{
		"category": "spot",
		"symbol":   symbol,
	}

	body, err := c.doPublicRequest(ctx, "GET", "/v5/market/instruments-info", params)
	if err != nil {
		return nil, err
	}

	var resp InstrumentsInfoResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, err
	}

	symbols := make([]exchange.Symbol, len(resp.Result.List))
	for i, inst := range resp.Result.List {
		// Calculate precision from tick size (price) and base precision (quantity)
		basePrecision := getPrecisionFromString(inst.LotSizeFilter.BasePrecision)
		// Price precision comes from tickSize in PriceFilter, not QuotePrecision
		pricePrecision := getPrecisionFromString(inst.PriceFilter.TickSize)

		log.Printf("[Bybit] %s: tickSize=%s pricePrecision=%d basePrecision=%d",
			inst.Symbol, inst.PriceFilter.TickSize, pricePrecision, basePrecision)

		symbols[i] = exchange.Symbol{
			Symbol:              inst.Symbol,
			BaseAsset:           inst.BaseCoin,
			BaseAssetPrecision:  basePrecision,
			QuoteAsset:          inst.QuoteCoin,
			QuotePrecision:      pricePrecision,
			QuoteAssetPrecision: pricePrecision,
			Filters: []exchange.Filter{
				{
					FilterType:  "PRICE_FILTER",
					MinNotional: inst.LotSizeFilter.MinOrderAmt,
				},
			},
		}
	}

	return &exchange.ExchangeInfo{
		Symbols: symbols,
	}, nil
}

// GetDepth retrieves order book depth
func (c *Client) GetDepth(ctx context.Context, symbol string) (*exchange.Depth, error) {
	params := map[string]string{
		"category": "spot",
		"symbol":   symbol,
		"limit":    "20",
	}

	body, err := c.doPublicRequest(ctx, "GET", "/v5/market/orderbook", params)
	if err != nil {
		return nil, err
	}

	var resp OrderbookResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, err
	}

	return &exchange.Depth{
		Bids: resp.Result.Bids,
		Asks: resp.Result.Asks,
	}, nil
}

// GetTicker returns the last trade price for a symbol
func (c *Client) GetTicker(ctx context.Context, symbol string) (float64, error) {
	params := map[string]string{
		"category": "spot",
		"symbol":   symbol,
	}

	body, err := c.doPublicRequest(ctx, "GET", "/v5/market/tickers", params)
	if err != nil {
		return 0, err
	}

	var resp TickerResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return 0, err
	}

	if len(resp.Result.List) == 0 {
		return 0, nil
	}

	return parseFloat(resp.Result.List[0].LastPrice), nil
}

// GetRecentTrades returns recent market trades
// TODO: Implement using Bybit /v5/market/recent-trade endpoint
func (c *Client) GetRecentTrades(ctx context.Context, symbol string, limit int) ([]exchange.Trade, error) {
	// Stub implementation - returns empty trades
	// Bot will use fallback logic (cached VWAP or order book mid)
	return []exchange.Trade{}, nil
}

// getPrecisionFromString calculates decimal precision from a string like "0.0001"
func getPrecisionFromString(s string) int {
	if s == "" {
		return 8
	}

	// Parse the number
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 8
	}

	if f == 0 {
		return 8
	}

	// Count decimal places
	str := strings.TrimRight(s, "0")
	idx := strings.Index(str, ".")
	if idx == -1 {
		return 0
	}

	return len(str) - idx - 1
}
