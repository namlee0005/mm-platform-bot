# Lessons Learned

## 2026-02-26: Mutex Deadlock in Nested Function Calls

**Mistake:** Added `s.mu.RLock()` inside `computeDesiredOrders()` while it's called from `getOrRegenerateLadder()` which already holds `s.mu.Lock()`.

**Result:** Deadlock - bot hangs after "Rebalance combined:" log.

**Fix:** Don't acquire locks inside functions that are called from lock-holding contexts. Read shared state directly when already inside lock context.

**Rule:** Before adding lock acquisition, trace the call stack to verify no caller already holds the lock.

## 2026-02-26: Log Format Precision Mismatch

**Mistake:** Used `%.6f` for mid price in tick summary log, while calculation used full precision.

**Result:** Log showed `mid=0.000037` (rounded up) instead of actual `mid=0.00003666`, making orders appear to exceed ±2% limit when they were actually correct.

**Fix:** Use `%.8f` for price logging to match calculation precision.

**Rule:** Use consistent precision (%.8f) for all price-related logs to avoid confusion during debugging.

## 2026-03-20: Division by Zero Causing Inf Values in Order Generation

**Mistake:** In `generateSideOrders()`, calculated `qty := sizeNotional / price` without validating that `price > 0`.

**Result:** When `mid` price was 0 or invalid (from edge cases like missing order book data), `price` became 0, causing `qty` to be `+Inf`. Orders with `qty=+Inf, price=0` were generated and sent to the exchange.

**Fix:** Added multi-layer validation:
1. In `calculateFairPrice()`: Validate result before returning, fallback to mid if invalid
2. In `Tick()`: Check fairPrice is valid before proceeding
3. In `computeDesiredOrders()`: Validate mid > 0 at start
4. In `generateSideOrders()`: Validate price > 0 before division, and validate qty after calculation

**Rule:** Always validate divisors before division operations. Check for `<= 0`, `math.IsNaN()`, and `math.IsInf()` for any value that will be used as a divisor or sent to external APIs.

## 2026-03-24: CCXT Precision Mode Mismatch (TICK_SIZE vs DECIMAL_PLACES)

**Mistake:** Assumed CCXT returns precision as integer decimal places (e.g., `8`), but most exchanges use TICK_SIZE mode where precision is a float (e.g., `0.00000001`).

**Code:**
```go
pricePrecision = int(*market.Precision.Price)  // int(0.00000001) = 0
tickSize = math.Pow10(-0) = 1.0
```

**Result:** `tickSize = 1.0` instead of `0.00000001`. Small prices like `0.00052` rounded to `0` via `math.Round(0.00052/1) * 1 = 0`.

**Fix:** Detect precision mode by checking if value >= 1 (DECIMAL_PLACES) or < 1 (TICK_SIZE):
```go
if rawPrecision >= 1 {
    pricePrecision = int(rawPrecision)  // DECIMAL_PLACES mode
} else if rawPrecision > 0 {
    pricePrecision = int(-math.Log10(rawPrecision))  // TICK_SIZE mode
}
```

**Rule:** When using CCXT, always check precision mode. Most exchanges (Bybit, Binance, etc.) use TICK_SIZE mode where precision values are floats representing the actual minimum increment.

## 2026-03-24: Duplicate WebSocket CANCELED Events

**Mistake:** Only tracked pending cancels in a map that was deleted on first event. Assumed one CANCELED event per order.

**Result:** WebSocket sent duplicate CANCELED events for the same order seconds apart (e.g., 10:41:41 and 10:41:59). First event deleted from `pendingCancels`, second event (with no entry) was treated as "External cancel", causing ladder regeneration and order count mismatch (bot shows 6, exchange shows 5).

**Fix:** Added `recentlyCancelled map[string]int64` to track confirmed internal cancels with timestamp. When a cancel is confirmed, move order ID from `pendingCancels` to `recentlyCancelled`. Before treating cancel as external, check if order exists in `recentlyCancelled`. Clean up entries older than 60 seconds.

**Rule:** WebSocket events can be duplicated or delayed. Always implement idempotent event handling with time-based duplicate detection. Track state transitions (not just pending state) to handle late-arriving duplicate events.

## 2026-03-24: Order Tracker Out of Sync with Exchange

**Mistake:** Added orders to local tracker immediately after REST API success, before WebSocket confirmation. Assumed REST success = order exists on exchange.

**Result:** When exchange silently rejects an order (no WebSocket event), local tracker has stale entry. Bot thinks it has 6 orders, exchange only has 5.

**Fix:** Added `SyncOrdersIntervalMs` config to sync tracker with exchange periodically:
- `0` = sync every tick (for strategies with few orders like simple_ladder ~6)
- `300000` = sync every 5 min (for strategies with many orders like depth_filler ~50)

**Rule:** For order state, always use exchange as source of truth. Local tracking is just a cache that must be periodically reconciled. Balance sync frequency vs API rate limits based on order count.

## 2026-03-24: External Cancel Should Not Force Full Regeneration

**Mistake:** When detecting external cancel (user cancelled 1 order on exchange UI), cleared `cachedLadder = nil` to force regeneration.

**Result:** User cancels 1 order → bot regenerates entire ladder with new random prices → `computeOrderDiff` finds most orders don't match → REPLACE all 6 orders. Overkill reaction.

**Fix:** Don't clear `cachedLadder` on external cancel. Let normal tick logic handle it via `notional_low` check. If notional is still sufficient, KEEP. If not, AMEND only the missing order(s).

**Rule:** External cancel of 1 order should result in adding 1 order (if needed), not replacing all orders. Don't overreact to single events.

## 2026-03-24: WebSocket Events Should Not Affect Strategy Logic

**Mistake:** Strategy's `OnFill` and `OnOrderUpdate` callbacks modified internal state (cooldowns, pending cancels, shift queues) that affected the Tick loop logic. This created race conditions and REPLACE loops when fills/cancels happened.

**Result:** WebSocket events → modified strategy state → Tick detected stale state → REPLACE all orders → more WebSocket events → loop

**Fix:** Completely refactored architecture:
1. **WebSocket callbacks in strategy**: Only log events (1-3 lines max). No state tracking.
2. **BaseBot**: Handles all order tracking, Redis, MongoDB operations
3. **Main loop**: Syncs from exchange every tick (source of truth), independent of WebSocket
4. **Gap detection**: `detectAndFillGaps()` runs every tick, adds orders at ladder edges

**Rule:** Separate concerns strictly:
- WebSocket = notifications only (logging, external push)
- Main loop = source of truth sync + gap detection
- Strategy state = minimal (only what's needed for order generation)

**Benefits:**
- No race conditions between WebSocket and Tick loop
- Simpler code (~150 lines removed from SimpleLadderStrategy)
- Fill 1 order → detect gap next tick → add 1 order (not REPLACE all)