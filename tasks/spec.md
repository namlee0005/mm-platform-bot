# MM Platform Bot - Technical Specification

## Overview

Market Making Bot Platform supporting multiple cryptocurrency exchanges via unified interface.

**Current Version:** CCXT-based multi-exchange support
**Language:** Go 1.21+
**Dependencies:** CCXT Go v4, Redis, MongoDB

---

## Architecture

```
┌─────────────────────────────────────────────────────────────────┐
│                           main.go                                │
│                    (Bot Initialization)                          │
└─────────────────────────────────────────────────────────────────┘
                              │
                              ▼
┌─────────────────────────────────────────────────────────────────┐
│                          BaseBot                                 │
│  - Lifecycle management (Start/Stop)                            │
│  - Tick loop (configurable interval)                            │
│  - WebSocket subscription & event handling                      │
│  - Order execution (Place/Cancel/Amend)                         │
└─────────────────────────────────────────────────────────────────┘
                              │
              ┌───────────────┼───────────────┐
              ▼               ▼               ▼
┌──────────────────┐ ┌──────────────────┐ ┌──────────────────┐
│   Strategy       │ │    Exchange      │ │     Store        │
│ (SimpleLadder/   │ │  (CCXT Adapter)  │ │ (Redis/MongoDB)  │
│  DepthFiller)    │ │                  │ │                  │
└──────────────────┘ └──────────────────┘ └──────────────────┘
```

---

## Core Components

### 1. Exchange Layer (`internal/exchange/`)

#### Exchange Interface
```go
type Exchange interface {
    GetExchangeInfo(ctx, symbol) (*ExchangeInfo, error)
    GetDepth(ctx, symbol) (*Depth, error)
    GetAccount(ctx) (*Account, error)
    PlaceOrder(ctx, *OrderRequest) (*Order, error)
    BatchPlaceOrders(ctx, []*OrderRequest) (*BatchOrderResponse, error)
    CancelOrder(ctx, symbol, orderID) error
    CancelAllOrders(ctx, symbol) error
    GetOpenOrders(ctx, symbol) ([]*Order, error)
    SubscribeUserStream(ctx, handlers) error
    GetTicker(ctx, symbol) (float64, error)
    GetRecentTrades(ctx, symbol, limit) ([]Trade, error)
    Start(ctx) error
    Stop(ctx) error
}
```

#### CCXT Adapter (`ccxt_adapter.go`)
- **Unified multi-exchange support** via `github.com/ccxt/ccxt/go/v4`
- **WebSocket support** via `github.com/ccxt/ccxt/go/v4/pro`
- **Supported exchanges:** bybit, binance, okx, gate, kucoin, mexc, htx, bitget
- **Symbol conversion:** Auto-converts native format (BTCUSDT) to CCXT format (BTC/USDT)

#### Legacy Exchange Clients (deprecated)
- `bybit/` - Direct Bybit API implementation
- `gate/` - Direct Gate.io API implementation
- `mexc/` - Direct MEXC API implementation

---

### 2. Core Layer (`internal/core/`)

#### BaseBot (`base_bot.go`)
Main bot engine providing:
- **Lifecycle:** Start, Stop, mainLoop
- **Tick Loop:** Periodic strategy execution
- **Event Handling:** WebSocket callbacks (OnFill, OnOrderUpdate, OnAccountUpdate)
- **Order Execution:** replaceOrders, amendOrders, cancelAllOrders
- **State Tracking:** OrderTracker, BalanceTracker, MarketDataCache

#### Strategy Interface (`interfaces.go`)
```go
type Strategy interface {
    Name() string
    Init(ctx, *Snapshot, *BalanceState) error
    Tick(ctx, *TickInput) (*TickOutput, error)
    OnFill(*FillEvent)
    OnOrderUpdate(*OrderEvent)
    UpdateConfig(newCfg interface{}) error
}
```

#### TickAction Types
- `KEEP` - No changes needed
- `REPLACE` - Cancel all + place new orders
- `AMEND` - Cancel specific + add specific orders
- `CANCEL_ALL` - Cancel all orders (pause mode)

---

### 3. Strategy Layer (`internal/strategy/`)

#### SimpleLadder Strategy (`simple_ladder.go`)
Two-sided market maker with ladder orders.

**Features:**
- VWAP-based fair price calculation
- Inventory-aware spread adjustment
- Risk management (drawdown limits, recovery mode)
- Fill cooldown (prevents immediate reorder at fill price)
- **Shift Ladder after Fill** - adds order at ladder end instead of full regeneration

**Config Parameters:**
| Parameter | Default | Description |
|-----------|---------|-------------|
| `SpreadMinBps` | 40 | Minimum spread from mid (bps) |
| `SpreadMaxBps` | 100 | Maximum spread from mid (bps) |
| `NumLevels` | 5 | Number of price levels per side |
| `TargetDepthNotional` | 10000 | Target notional per side ($) |
| `DepthBps` | 200 | Max depth from mid (bps) |
| `LadderRegenBps` | 50 | Regenerate when mid moves (bps) |
| `FillCooldownMs` | 5000 | Cooldown after fill (ms) |
| `DrawdownLimitPct` | 0.05 | Max drawdown before pause |

#### DepthFiller Strategy (`depth_filler.go`)
Order book depth filler for thin markets.

**Config Parameters:**
| Parameter | Default | Description |
|-----------|---------|-------------|
| `MinDepthPct` | 5 | Minimum depth from mid (%) |
| `MaxDepthPct` | 50 | Maximum depth from mid (%) |
| `UseFullBalance` | false | Use full balance mode |
| `MinOrderSizePct` | 1 | Min order size (% of balance) |

---

### 4. Store Layer (`internal/store/`)

#### Redis Store (`redis.go`)
- **Order tracking:** `order:{exchange}:{symbol}` hash
- **Balance storage:** `mm:balance:{exchange}:{symbol}:{botID}` hash
- **Event streaming:** Redis Streams for real-time events
- **Status tracking:** Bot status keys

#### MongoDB Store (`mongo.go`)
- **Fill history:** Persistent trade records
- **Config storage:** Bot configuration
- **Config hot-reload:** Runtime config updates

---

### 5. Bot Layer (`internal/bot/`)

#### Bot Factory
- `NewSimpleMaker()` - Creates SimpleLadder bot
- `NewDepthFiller()` - Creates DepthFiller bot

#### UserStream (`userstream.go`)
WebSocket event handling with auto-reconnect.

---

## Data Flow

### Tick Cycle
```
1. Fetch last trade price (GetTicker)
2. Get market snapshot (GetDepth)
3. Get balance state (from cache or GetAccount)
4. Get recent trades (for VWAP)
5. Build TickInput
6. Strategy.Tick() → TickOutput
7. Execute action (KEEP/REPLACE/AMEND/CANCEL_ALL)
```

### Fill Event Flow
```
1. WebSocket receives fill
2. BaseBot.handleFill()
   - Update OrderTracker (remove/update)
   - Forward to Strategy.OnFill()
   - Emit event callback
   - Save to Redis/MongoDB
3. Strategy.OnFill()
   - Set fill cooldown
   - Add pending shift order
4. Next Tick
   - Process pending shifts (add order at ladder end)
```

---

## Key Algorithms

### Fair Price Calculation (VWAP + Inventory)
```
1. Calculate VWAP from recent trades (10 min window)
2. Calculate inventory skew: (baseValue - quoteValue) / totalValue
3. Apply inventory adjustment: fairPrice = vwap - (skew * adjustmentBps)
```

### Spread Allocation
```
1. Random total spread in [SpreadMin, SpreadMax]
2. Split based on inventory:
   - Excess base → BID gets more spread (encourage selling)
   - Excess quote → ASK gets more spread (encourage buying)
```

### Shift Ladder after Fill
```
1. On fill: store pending shift (side, price, qty)
2. On next tick:
   - BID fill → add new BID below lowest existing
   - ASK fill → add new ASK above highest existing
   - Return AMEND with only OrdersToAdd (no cancels)
```

---

## Configuration

### Environment Variables
```bash
BOT_TYPE=simple-maker|depth-filler
MONGO_URI=mongodb://...
REDIS_ADDR=localhost:6379
```

### MongoDB Config Structure
```json
{
  "exchange_name": "bybit",
  "trading_config": {
    "symbol": "BTCUSDT",
    "base_asset": "BTC",
    "quote_asset": "USDT"
  },
  "simple_config": {
    "spread_min_bps": 40,
    "num_levels": 5,
    "target_depth_notional": 10000
  }
}
```

---

## Error Handling

### Rate Limiting
- Batch orders in groups of 5
- 1 second delay between batches
- Fallback to individual orders on batch failure
- 5 second wait on rate limit error

### Reconnection
- Auto-reconnect WebSocket on disconnect
- Exponential backoff (max 30 seconds)
- Max 10 retry attempts

### Drawdown Protection
- **Warning mode:** >3% drawdown → log warning
- **Recovery mode:** >2% drawdown → reduce order size
- **Paused mode:** >5% drawdown → cancel all orders

---

## File Structure
```
mm-platform-bot/
├── cmd/main/main.go          # Entry point
├── internal/
│   ├── bot/                  # Bot factory & helpers
│   ├── config/               # Config loading
│   ├── core/                 # BaseBot, interfaces
│   ├── exchange/             # Exchange adapters
│   │   ├── ccxt_adapter.go   # CCXT unified adapter
│   │   ├── bybit/            # Legacy Bybit client
│   │   ├── gate/             # Legacy Gate client
│   │   └── mexc/             # Legacy MEXC client
│   ├── modules/              # Reusable modules
│   ├── store/                # Redis & MongoDB
│   ├── strategy/             # Trading strategies
│   └── types/                # Shared types
├── tasks/                    # Task tracking
└── CLAUDE.md                 # AI instructions
```
