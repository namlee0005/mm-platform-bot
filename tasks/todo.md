# MM Platform Bot - Task Tracker

## Completed Tasks

### Session 2026-03-21

- [x] **CCXT Multi-Exchange Integration**
  - Created `internal/exchange/ccxt_adapter.go`
  - REST API via `github.com/ccxt/ccxt/go/v4`
  - WebSocket via `github.com/ccxt/ccxt/go/v4/pro`
  - Supported: bybit, binance, okx, gate, kucoin, mexc, htx, bitget
  - Auto symbol conversion (BTCUSDT → BTC/USDT)
  - Updated `cmd/main/main.go` to use CCXT adapter

- [x] **Shift Ladder after Fill**
  - Modified `internal/strategy/simple_ladder.go`
  - Added `pendingShiftOrder` struct
  - OnFill stores pending shift
  - Tick processes shifts → adds orders at ladder end
  - Max shifts per side per tick (3)
  - Inventory-aware size multipliers
  - Eliminates gap time when replenishing after fill

- [x] **Unit Tests for CCXT Adapter**
  - Created `internal/exchange/ccxt_adapter_test.go`
  - Tests for symbol conversion (BTCUSDT → BTC/USDT)
  - Tests for helper functions (derefString, derefFloat64, etc.)
  - Tests for status mapping

- [x] **Prometheus Metrics**
  - Created `internal/metrics/prometheus.go`
  - Metrics: NAV, drawdown, inventory, fills, orders, latency, errors
  - Updated HTTP server `/metrics` endpoint with promhttp.Handler()
  - Integrated metrics recording into bot run loop and fill handler

- [x] **Telegram Notifications**
  - Created `internal/notify/telegram.go`
  - Alert cases: Fill, DrawdownWarning, DrawdownCritical, ModeChange, BalanceLow, ConnectionLost, ConnectionRestored, BotStarted, BotStopped, DailySummary, Error
  - Rate limiting per alert type
  - Integrated into bot lifecycle (start/stop) and WebSocket handlers

- [x] **Clean up Legacy Exchange Clients**
  - Moved `bybit/`, `gate/`, `mexc/` to `internal/exchange/_deprecated/`
  - Updated `internal/bot/bot.go` to use CCXT adapter only
  - Removed legacy imports and conversion functions

- [x] **Redis Key Format Update**
  - Changed from `order:{symbol}` to `order:{exchange}:{symbol}`
  - Updated all Redis functions in `internal/store/redis.go`

### Previous Sessions

- [x] Bybit exchange implementation (V5 API)
- [x] VWAP-based fair price calculation
- [x] Inventory-aware spread adjustment
- [x] Drawdown protection modes (Warning/Recovery/Paused)
- [x] Fill cooldown mechanism
- [x] Order diff algorithm (incremental updates)
- [x] WebSocket auto-reconnect
- [x] Config hot-reload from MongoDB
- [x] Batch order placement with rate limiting

---

## Pending Tasks

### High Priority

- [ ] **Test CCXT adapter on live exchange**
  - Verify WebSocket streams work correctly
  - Test order placement/cancellation
  - Verify symbol conversion for all exchanges
  - Test on testnet first, then mainnet

### Medium Priority

- [ ] **Add Grafana dashboard template**
  - Dashboard JSON for Prometheus metrics
  - Panels: NAV, drawdown, fills, spread, latency

- [ ] **Add drawdown alert with Telegram**
  - Integrate NotifyDrawdownWarning into run loop
  - Trigger when drawdown > 3% (warning) or > 5% (critical)

### Low Priority

- [ ] **Add more exchanges to CCXT adapter**
  - Coinbase, Kraken, Bitstamp
  - Test WebSocket compatibility

- [ ] **Optimize tick interval**
  - Dynamic tick based on market volatility
  - Reduce API calls during low activity

- [ ] **Add backtesting framework**
  - Historical data replay
  - Strategy performance metrics

### Session 2026-03-24

- [x] **Fix CCXT TICK_SIZE Precision Mode**
  - Root cause: Bybit returns precision as TICK_SIZE float (0.00000001), not DECIMAL_PLACES int (8)
  - Fix in `ccxt_adapter.go`: detect mode by checking if value >= 1 (DECIMAL_PLACES) or < 1 (TICK_SIZE)
  - Convert TICK_SIZE to decimal places via `-log10(tickSize)`

- [x] **Fix PlaceOrder Response Missing Values**
  - CCXT Bybit response doesn't include Price/Qty/ClientOrderID
  - Fix: Use request values as fallback in PlaceOrder response

- [x] **Fix CancelAll False Positive "External Cancel"**
  - Root cause: CancelAllOrders() cancels orders not in `pendingCancels` map
  - Fix: Added `cancelAllPending` flag with 5-10 second window

- [x] **Fix Duplicate CANCELED Events**
  - Root cause: WebSocket sends duplicate CANCELED events seconds apart
  - First event removes from `pendingCancels`, second treated as "External cancel"
  - Fix: Track confirmed cancels in `recentlyCancelled` map with 60s TTL
  - Now logs "Duplicate cancel event (ignoring)" instead of "External cancel"

- [x] **Fix Order Tracker Mismatch (6 orders vs 5)**
  - Root cause: Orders added to tracker immediately after REST API success, before WebSocket confirmation
  - If exchange rejects order silently (no WebSocket event), tracker has stale entry
  - Fix: Added `SyncOrdersIntervalMs` config to sync tracker with exchange periodically
  - simple_maker: `0` (sync every tick, ~6 orders)
  - depth_filler: `300000` (sync every 5 min, ~50 orders to avoid API spam)

- [x] **Fix External Cancel Overreaction**
  - Root cause: External cancel cleared `cachedLadder = nil`, forcing full regeneration
  - User cancels 1 order → bot replaces all 6 orders (overkill)
  - Fix: Don't clear cache on external cancel. Let `notional_low` check handle it naturally
  - Result: Cancel 1 order → KEEP or AMEND 1 order (not REPLACE all)

- [x] **Fix notional_low threshold (110% → 100%)**
  - Root cause: Threshold was 110% of target, causing regen even when at 100%
  - Fix: Changed to 100% - only regen when actually below target

- [x] **Reduce ordersTooFew threshold**
  - Root cause: `minOrdersPerSide = NumLevels/2 = 3`, too aggressive
  - Fix: Changed to `minOrdersPerSide = 1` - only regen when side is completely empty

- [x] **Clean up cancel event logs**
  - Removed spam logs for duplicate/external cancel events
  - Only log important events: NEW, FILLED, Internal cancel confirmed

- [x] **Add fill log with details**
  - Added: `💰 FILL BUY @ price x qty ($notional) order=id, cooldown=ms`

---

## 🔴 HIGH PRIORITY - Next Session Tasks

### 1. Bug: Order Fill không show data
- **Vấn đề:** WebSocket `WatchMyTrades` không nhận được fill events từ Bybit
- **Logs không thấy:** `[CCXT:bybit] Received trade` hoặc `[FILL]`
- **Debug đã thêm:** Logs trong `ccxt_adapter.go` line ~602-615
- **Cần check:**
  - CCXT có support `WatchMyTrades` cho Bybit không?
  - Symbol format có đúng không?
  - Thử gọi `WatchMyTrades` không có symbol filter

---

### ✅ COMPLETED (Session 2026-03-24 continued)

- [x] **Refactor Architecture: WebSocket chỉ để notifications**
  - Strategy `OnFill` and `OnOrderUpdate` now only log events
  - BaseBot handles all state tracking, Redis push, MongoDB save
  - Main loop syncs from exchange (source of truth) via `SyncOrdersIntervalMs`
  - Gap detection runs every tick - no dependency on WebSocket events

- [x] **Simplify OnFill và OnOrderUpdate**
  - `OnFill`: Only logs fill event (1 line)
  - `OnOrderUpdate`: Only logs NEW events (3 lines)
  - Removed all state tracking (pendingCancels, recentlyCancelled, recentFills, etc.)

- [x] **Thêm `detectAndFillGaps` function**
  - Added to `internal/strategy/simple_ladder.go`
  - Counts orders per side, detects missing orders
  - Adds orders at ladder edges (below lowest bid / above highest ask)
  - Max 3 orders per side per tick
  - Uses inventory-based size multipliers

- [x] **Update Tick logic**
  - Tick now calls `detectAndFillGaps` before `shouldRegenerateLadder`
  - If gaps detected → AMEND (add orders)
  - If no gaps and ladder ok → KEEP
  - Only regenerate for: initial, mid moved > LadderRegenBps, one side empty

- [x] **Remove unused fields from SimpleLadderStrategy**
  - Removed: `fillCooldowns`, `fillCooldownMs`, `lastFillTime`
  - Removed: `lastCancelTime`, `pendingCancels`, `cancelAllPending`, `cancelAllDeadline`
  - Removed: `recentlyCancelled`, `recentFills`, `pendingShifts`
  - Removed: `pendingShiftOrder` type
  - Removed: `DebugCancelSleep` config field

---

## Known Issues

1. **CCXT Fee struct missing Currency field**
   - CommissionAsset always empty in fill events
   - Low impact - fee amount still tracked

2. **Fill notification always shows "Full Fill"**
   - Need to detect partial vs full fills from event data
   - Minor UX issue only

3. **REPLACE loop khi fill order**
   - Root cause: Fill không được detect → notional giảm → regenerate với random prices → REPLACE
   - Fix: Implement detectAndFillGaps (task #4 above)

---

## Integration Summary

### Files Modified/Created This Session:

**New Files:**
- `internal/exchange/ccxt_adapter.go` - Unified CCXT exchange adapter
- `internal/exchange/ccxt_adapter_test.go` - Unit tests
- `internal/metrics/prometheus.go` - Prometheus metrics
- `internal/notify/telegram.go` - Telegram notifications

**Modified Files:**
- `internal/bot/bot.go` - Added metrics/telegram fields, init in NewBot
- `internal/bot/userstream.go` - Added fill notifications, connection alerts
- `internal/bot/run.go` - Added Prometheus metrics recording
- `internal/http/server.go` - Added `/metrics` Prometheus endpoint
- `internal/config/config.go` - Added Telegram config fields
- `internal/types/types.go` - Added PeakNAV to EngineState
- `cmd/main/main.go` - Simplified to single CCXT call

**Deprecated:**
- `internal/exchange/_deprecated/bybit/`
- `internal/exchange/_deprecated/gate/`
- `internal/exchange/_deprecated/mexc/`

### Environment Variables:

```bash
# Required
USER_EXCHANGE_KEY_ID=...
APP_MASTER_KEY=...
MONGO_URI=...
REDIS_ADDR=...

# Optional (Telegram)
TELEGRAM_BOT_TOKEN=...
TELEGRAM_CHAT_ID=...
```

### Next Steps:
1. Test on exchange testnet
2. Configure Telegram bot and get chat ID
3. Deploy and monitor via Prometheus/Grafana
