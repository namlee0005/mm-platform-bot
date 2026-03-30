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

Stateless convergence-based two-sided market maker. Mỗi tick tính desired orders từ đầu, diff với live orders, chỉ cancel/create phần khác biệt.

---

##### 3.1 Tick Flow

```
Tick(input) → TickOutput:
  1. Tính fair price = VWAP (10 min window) + inventory adjustment
  2. Tính NAV = (BaseFree + BaseLocked) * mid + (QuoteFree + QuoteLocked)
  3. Check risk: drawdown % → mode (NORMAL/WARNING/RECOVERY/PAUSED)
     - PAUSED → return CANCEL_ALL
  4. Tính sizeMult dựa trên mode/drawdown
  5. Gọi maintainLadder(mid, bestBid, bestAsk, balance, liveOrders, sizeMult)
  6. Return TickOutput {Action, OrdersToCancel, OrdersToAdd}
```

##### 3.2 maintainLadder (Stateless Convergence)

```
maintainLadder():
  1. desired = computeDesiredOrders(mid, balance, sizeMult)
  2. toCancel, toCreate = converge(desired, liveOrders)
  3. Nếu không có thay đổi → return nil (KEEP)
  4. Return AMEND {toCancel, toCreate}
```

Không có state giữa các tick. Desired được tính fresh mỗi lần.

##### 3.3 computeDesiredOrders — Tính Order Book Mong Muốn

**Bước 1: Inventory → Spread**
```
skew = (baseValue - quoteValue) / totalValue
  skew > 0: nhiều base → BID spread rộng hơn (khuyến khích bán)
  skew < 0: ít base → ASK spread rộng hơn (khuyến khích mua)

bidBps, askBps = split spread dựa trên skew
  splitRatio ∈ [0.3, 0.7]
```

**Bước 2: Price Generation — Log Spacing + Cached Jitter**
```
BUY:
  inner = mid × (1 - bidBps/10000)
  outer = mid × (1 - (depthBps - 15bps)/10000)   // 15bps safety buffer
  prices = logSpacedPrices(inner, outer, numLevels)
    → Dense gần inner, sparse gần outer
    → Jitter ±2-6 ticks cho middle levels (L0 và outermost giữ nguyên)
    → Jitter CACHED, chỉ refresh khi mid shift >1%

SELL: tương tự nhưng prices tăng dần
```

**Bước 3: Qty — Noisy Pyramid + Cached Noise**
```
weight[i] = (1 + (pyramidFactor-1) × i/(N-1)) × cachedQtyNoise[i]
  cachedQtyNoise[i] ∈ [0.8, 1.2] (±20% noise, cached cùng lúc với jitter)
  Normalize: sum(weights) = 1.0

qty[i] = roundToStep(targetDepth × weight[i] × sizeMult / price[i])
```

**Bước 4: Budget Constraint**
```
availableQuote = QuoteFree + QuoteLocked  // Dùng TOTAL vì converge có thể cancel order cũ
availableBase  = BaseFree + BaseLocked

Nếu bidTotal > availableQuote → scale down tỷ lệ
Nếu askTotal > availableBase*mid → scale down tỷ lệ
Orders dưới minNotional bị drop (outer first)
```

##### 3.4 Noise Caching — Tại Sao Quan Trọng

```
refreshNoiseIfNeeded(mid, numLevels):
  Refresh khi:
    - Chưa có cache
    - numLevels thay đổi
    - mid shift >1% so với lần cache trước

  Generate:
    - priceJitter[i]: ±(2-6) ticks (signed float)
    - qtyNoise[i]: [0.8, 1.2] multiplier

  Lưu cachedNoiseMid = mid hiện tại
```

**Tại sao cache?** Nếu random mỗi tick → desired set khác nhau hoàn toàn → converge luôn thấy cần thay đổi → cancel/create liên tục (oscillation). Cache đảm bảo desired ổn định giữa các tick.

##### 3.5 converge — Diff Desired vs Live

```
converge(desired, live) → (toCancel, toCreate):

  1. Split theo side (BUY/SELL)
  2. Tìm innermost live price mỗi side:
     innermostBid = max(live BUY prices)
     innermostAsk = min(live SELL prices)

  3. Mỗi desired order tìm live order gần nhất theo price:

     a) Match (price diff ≤ priceTolerance):
        - Check qty: |live.qty - desired.qty| / desired.qty
        - Nếu qty diff > 30% → cancel + create (replace)
        - Nếu ≤ 30% → KEEP (giữ nguyên)

     b) No match — Inner Gap (fill gap):
        - desired.Price > innermostLiveBid (cho BUY)
        - → SKIP: không đặt lại, để fill gap tự lấp khi mid shift
        - Lý do: tránh "prop up" giá cho người đang dump

     c) No match — Outer:
        - → CREATE: đặt order mới

  4. Live orders không match desired nào → CANCEL (orphans)
```

**Không có compensate outer orders** khi skip inner gaps. Nếu có, outer orders sẽ nằm ngoài desired range → cycle sau bị cancel → oscillation loop.

##### 3.6 Constants

| Constant | Value | Mô tả |
|----------|-------|-------|
| `defaultRelistToleranceBps` | 25 | Price tolerance để giữ live order (bps) |
| `defaultQtyTolerance` | 0.30 | Qty tolerance (30% diff → replace) |
| `outerSafetyBps` | 15 | Buffer từ depth boundary |
| `inventorySkewThreshold` | 0.05 | Threshold bắt đầu điều chỉnh spread |
| `spreadSplitRatioMin/Max` | 0.3/0.7 | BID nhận 30-70% total spread |
| `vwapWindowMinutes` | 10 | VWAP window |
| `inventoryAdjustmentBps` | 50 | ±50bps per 10% skew |

##### 3.7 Config Parameters

| Parameter | Default | Mô tả |
|-----------|---------|-------|
| `SpreadMinBps` | 40 | Spread tối thiểu (bps) |
| `SpreadMaxBps` | 100 | Spread tối đa (bps) |
| `NumLevels` | 10 | Số levels mỗi side |
| `TargetDepthNotional` | — | Target notional mỗi side ($) |
| `DepthBps` | 200 | Depth tối đa từ mid (bps) |
| `PyramidFactor` | 2.0 | Outer = 2x inner size |
| `LevelGapTicksMax` | 3 | Price tolerance ticks |
| `DrawdownLimitPct` | 5% | Pause khi drawdown vượt |
| `DrawdownWarnPct` | 3% | Warning threshold |
| `DrawdownReducePct` | 2% | Bắt đầu reduce size |
| `RecoveryHours` | 48 | Thời gian recovery |
| `MaxRecoverySizeMult` | 0.3 | Size multiplier khi recovery |

##### 3.8 Risk Management — Drawdown Modes

```
NAV = totalBase × mid + totalQuote
drawdown = (peakNAV - currentNAV) / peakNAV

NORMAL:     drawdown < DrawdownReducePct  → sizeMult = 1.0
WARNING:    drawdown ≥ DrawdownWarnPct    → log warning
RECOVERY:   drawdown ≥ DrawdownReducePct  → sizeMult = MaxRecoverySizeMult (0.3)
PAUSED:     drawdown ≥ DrawdownLimitPct   → CANCEL_ALL orders
```

---

##### 3.9 CancelOrder — Panic Recovery

```
CancelOrder(ctx, symbol, orderID):
  defer recover():
    Nếu panic chứa "OrderNotFound", "Order does not exist",
    "Order cancelled", "Order canceled", "-2011"
    → log "order already gone (ignored)", return nil
    → Khác: return error

  Nếu REST error chứa cùng patterns → return nil

  Tại sao: CCXT Go dùng panic() cho exchange errors.
  Race condition: order có thể đã filled/cancelled trước khi cancel request tới.
  Return nil = idempotent (mục đích là xoá order, order đã xoá = thành công).
```

---

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

### Fair Price = VWAP + Inventory Adjustment
```
fairPrice = VWAP(10min) - skew × 50bps_per_10%_skew
  skew = (baseValue - quoteValue) / totalValue
  Ví dụ: skew=15% → adjust = -0.15 × 50bps = -7.5bps
```

### Stateless Convergence (thay thế Shift Ladder)
```
Mỗi tick:
  1. computeDesiredOrders() → desired set (fresh, từ đầu)
  2. converge(desired, live) → diff (cancel/create)
  3. Execute diff

Không cần track fills hay shift — desired tự điều chỉnh theo mid mới.
Inner gaps (do fill) được skip, tự lấp khi mid shift.
```

### Noise Stability
```
Price jitter + qty noise được CACHE (không random mỗi tick).
Chỉ refresh khi mid shift >1%.
Đảm bảo desired set ổn định → converge không oscillate.
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
