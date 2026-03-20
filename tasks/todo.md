# Bybit Exchange Implementation Plan

## Overview
Implement Bybit V5 API spot trading exchange adapter following the existing MEXC pattern.

## Implementation Tasks

- [x] **1. Create bybit package structure**
  - `internal/exchange/bybit/client.go` - Main client
  - `internal/exchange/bybit/types.go` - Bybit-specific types
  - `internal/exchange/bybit/spot_orders.go` - Order operations
  - `internal/exchange/bybit/market.go` - Market data
  - `internal/exchange/bybit/ws_private.go` - WebSocket handling

- [x] **2. Implement client.go**
  - Client struct with apiKey, apiSecret, baseURL
  - HMAC-SHA256 signing (signature in headers)
  - doRequest for authenticated requests
  - doPublicRequest for market data
  - Start/Stop lifecycle

- [x] **3. Implement types.go**
  - Response wrappers (Bybit uses `{retCode, retMsg, result}`)
  - Order types, Account types, Market types
  - Status code mappings

- [x] **4. Implement spot_orders.go**
  - PlaceOrder (POST /v5/order/create)
  - BatchPlaceOrders (POST /v5/order/create-batch)
  - CancelOrder (POST /v5/order/cancel)
  - CancelAllOrders (POST /v5/order/cancel-all)
  - GetOpenOrders (GET /v5/order/realtime)

- [x] **5. Implement market.go**
  - GetExchangeInfo (GET /v5/market/instruments-info)
  - GetDepth (GET /v5/market/orderbook)
  - GetTicker (GET /v5/market/tickers)
  - GetRecentTrades (stub)

- [x] **6. Implement ws_private.go**
  - WebSocket connection with direct auth
  - Auth message: `{"op":"auth", "args":[apiKey, expires, signature]}`
  - Subscribe to: "order", "execution", "wallet"
  - Handle message types and dispatch to handlers

- [x] **7. Register in main.go**
  - Add "bybit" case to exchange switch

- [x] **8. Test compilation**
  - go build ./... - PASSED
  - go vet ./internal/exchange/bybit/... - PASSED

## Review

### Files Created
1. `internal/exchange/bybit/types.go` - API response types, WS message types, status mappers
2. `internal/exchange/bybit/client.go` - Main client, auth, HTTP requests
3. `internal/exchange/bybit/spot_orders.go` - Order CRUD operations
4. `internal/exchange/bybit/market.go` - Market data endpoints
5. `internal/exchange/bybit/ws_private.go` - WebSocket user stream

### Files Modified
1. `cmd/main/main.go` - Added bybit import and case handler

### Bybit API Key Differences from MEXC
- Auth headers: `X-BAPI-API-KEY`, `X-BAPI-SIGN`, `X-BAPI-TIMESTAMP`, `X-BAPI-RECV-WINDOW`
- Signature: `HMAC-SHA256(timestamp + api_key + recv_window + payload)`
- Response: `{retCode: 0, retMsg: "OK", result: {...}}`
- WebSocket: Direct auth handshake (no listen key)
- Order statuses: "New", "Filled", "PartiallyFilled", "Cancelled"
- Category param: "spot" for all spot endpoints

### Usage
```bash
# Set exchange name in MongoDB config or env
EXCHANGE_NAME=bybit
EXCHANGE_BASE_URL=https://api.bybit.com

# Or for testnet
EXCHANGE_BASE_URL=https://api-testnet.bybit.com
```

### Verification
- Build: `go build ./...` - PASSED
- Vet: `go vet ./internal/exchange/bybit/...` - PASSED