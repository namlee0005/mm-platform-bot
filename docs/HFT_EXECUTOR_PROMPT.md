# HFT Executor Rewrite Prompt

> Copy-paste this entire file as a prompt to Claude to implement the executor rewrite.

---

## Task

Rewrite `internal/engine/executor.go` to implement two critical patterns:

1. **State-Based Conflation** — A background worker drains the latest desired state. Intermediate ticks are dropped (conflated). Only the newest state matters.
2. **Cancel-First Execution** — All cancel diffs execute concurrently with a hard barrier (WaitGroup) before any amend or place diffs fire.

The rewrite must preserve all existing interfaces, callbacks, and WS-first/REST-fallback behavior.

---

## Codebase Context

### Core Types (defined in `internal/core/interfaces.go`)

```go
type TickAction string

const (
    TickActionKeep      TickAction = "KEEP"
    TickActionReplace   TickAction = "REPLACE"
    TickActionAmend     TickAction = "AMEND"
    TickActionCancelAll TickAction = "CANCEL_ALL"
)

type TickOutput struct {
    Action         TickAction
    DesiredOrders  []DesiredOrder     // For REPLACE: all desired orders
    OrdersToAdd    []DesiredOrder     // For AMEND: orders to add
    OrdersToCancel []string           // For AMEND: order IDs to cancel
    Metrics        map[string]float64
    NewMode        Mode
    Reason         string
}

type LiveOrder struct {
    OrderID       string
    ClientOrderID string
    Side          string
    Price         float64
    Qty           float64
    RemainingQty  float64
    LevelIndex    int
    PlacedAt      time.Time
}

type DesiredOrder struct {
    Side       string  // "BUY" or "SELL"
    Price      float64
    Qty        float64
    LevelIndex int
    Tag        string  // client order ID
}

type DiffAction string

const (
    DiffPlace  DiffAction = "PLACE"
    DiffAmend  DiffAction = "AMEND"
    DiffCancel DiffAction = "CANCEL"
)

type OrderDiff struct {
    Action  DiffAction
    Desired *DesiredOrder // non-nil for PLACE and AMEND
    Live    *LiveOrder    // non-nil for AMEND and CANCEL
    Reason  string
}
```

### Exchange Interfaces (defined in `internal/exchange/exchange.go`)

```go
type Exchange interface {
    PlaceOrder(ctx context.Context, order *OrderRequest) (*Order, error)
    CancelOrder(ctx context.Context, symbol, orderID string) error
    CancelAllOrders(ctx context.Context, symbol string) error
    // ... other methods unchanged
}

type WSOrderExchange interface {
    Exchange
    PlaceOrderWs(ctx context.Context, order *OrderRequest) (*Order, error)
    EditOrderWs(ctx context.Context, orderID, symbol string, newPrice, newQty float64) (*Order, error)
    CancelOrderWs(ctx context.Context, symbol, orderID string) error
    CancelAllOrdersWs(ctx context.Context, symbol string) error
    HasWsOrderSupport() bool
    HasWsEditSupport() bool
}
```

### Dependencies (defined in `internal/modules/`)

```go
// OrderDiffEngine computes diffs between desired and live orders.
// Usage: diffs = ex.diffEngine.Diff(desiredOrders, liveOrders)
type OrderDiffEngine struct { ... }

// ExecutionEngine executes diffs via REST (fallback path).
// Usage: err = ex.execEngine.Execute(ctx, diffs)
type ExecutionEngine struct { ... }

// CountByAction counts places, amends, cancels in a diff slice.
func CountByAction(diffs []core.OrderDiff) (places, amends, cancels int)
```

---

## Architecture: State-Based Conflation

### Problem

The engine emits ticks at high frequency. If a previous execution is in-flight (WS calls take 5-50ms each across multiple levels), new ticks must NOT queue up — only the latest state matters. Queuing causes:
- Stale order placements at outdated prices
- Unbounded goroutine/memory growth
- Increased latency between market state and resting orders

### Solution: Mutex + Pending Slot + execGate

Use a `sync.Mutex`-protected `*pendingExecution` pointer as a single-slot conflation buffer. A buffered-1 channel (`execGate`) acts as a "worker running" semaphore.

```go
type pendingExecution struct {
    output     *core.TickOutput
    liveOrders []core.LiveOrder
}

type Executor struct {
    exch       exchange.Exchange
    diffEngine *modules.OrderDiffEngine
    execEngine *modules.ExecutionEngine
    symbol     string

    // State-based conflation
    mu      sync.Mutex
    pending *pendingExecution // nil = nothing queued

    // Worker semaphore: buffered-1 channel.
    // Non-blocking send starts worker; worker drains on exit.
    execGate chan struct{}

    // Callbacks (unchanged)
    onOrderEvent  core.OrderEventCallback
    onOrderPlaced func(order *exchange.Order, levelIndex int)
    onOrderGone   func(orderID string)
}
```

### Conflation Flow (pseudocode)

```go
// Execute is called by the engine on every tick.
func (ex *Executor) Execute(ctx context.Context, output *core.TickOutput, liveOrders []core.LiveOrder) {
    // 1. Handle TickActionKeep — no-op, return immediately.
    // 2. Handle TickActionCancelAll — clear pending under lock, dispatch cancelAll in goroutine.
    // 3. Handle TickActionReplace / TickActionAmend:
    //    a. Lock mu
    //    b. Overwrite ex.pending with new pendingExecution{output, liveOrders}
    //    c. Unlock mu
    //    d. Call triggerWorker(ctx)
}

// triggerWorker starts the background worker IF not already running.
func (ex *Executor) triggerWorker(ctx context.Context) {
    select {
    case ex.execGate <- struct{}{}: // Acquired — no worker running
        go func() {
            defer func() {
                <-ex.execGate // Release gate

                // CRITICAL: Re-check for pending state that arrived between
                // the inner loop's nil-check and the gate release.
                ex.mu.Lock()
                hasMore := ex.pending != nil
                ex.mu.Unlock()
                if hasMore {
                    ex.triggerWorker(ctx) // Re-trigger
                }
            }()

            for {
                // Swap pending to local under lock
                ex.mu.Lock()
                p := ex.pending
                ex.pending = nil
                ex.mu.Unlock()

                if p == nil {
                    return // Nothing pending — exit worker
                }

                // Process the latest state
                ex.processState(ctx, p)
            }
        }()

    default:
        // Worker already running. It will pick up the latest pending
        // on its next loop iteration — this tick is conflated (dropped).
        log.Printf("[EXECUTOR] Conflated — worker running, latest state queued")
    }
}
```

### Why This Pattern

| Property | Guarantee |
|----------|-----------|
| At most 1 worker goroutine | `execGate` buffered-1 ensures mutual exclusion |
| Latest state always processed | Worker loops until `pending == nil` |
| No queue buildup | Single pending slot — overwrites, never appends |
| No race on gate release | Deferred re-check after `<-execGate` catches the edge case |
| No goroutine leaks | Worker exits when idle, re-spawns on demand |

---

## Architecture: Cancel-First Execution

### Problem

If cancels and places execute concurrently or places go first:
- New orders placed at updated prices while stale orders still rest → duplicate liquidity, self-crossing risk
- Exchange rejects PostOnly orders that would cross the stale resting order on the same side

### Solution: Two-Phase WaitGroup Barrier

```go
func (ex *Executor) executeDiffs(ctx context.Context, diffs []core.OrderDiff) error {
    // Check for WS support; fall back to REST ExecutionEngine if unavailable.
    wsExch, hasWs := ex.exch.(exchange.WSOrderExchange)
    if !hasWs || !wsExch.HasWsOrderSupport() {
        return ex.execEngine.Execute(ctx, diffs)
    }

    // Partition diffs by action
    var cancels, amends, places []core.OrderDiff
    for _, d := range diffs {
        switch d.Action {
        case core.DiffCancel:
            cancels = append(cancels, d)
        case core.DiffAmend:
            amends = append(amends, d)
        case core.DiffPlace:
            places = append(places, d)
        }
    }

    // ── Phase 1: Concurrent cancels — HARD BARRIER ──────────────────
    // All cancels fire concurrently. WaitGroup blocks until ALL have
    // returned (success or fallback). No place/amend starts until
    // every cancel is confirmed gone.
    if len(cancels) > 0 {
        var wg sync.WaitGroup
        for _, d := range cancels {
            wg.Add(1)
            go func(d core.OrderDiff) {
                defer wg.Done()
                // Try WS cancel first
                if err := wsExch.CancelOrderWs(ctx, ex.symbol, d.Live.OrderID); err != nil {
                    // REST fallback
                    _ = ex.exch.CancelOrder(ctx, ex.symbol, d.Live.OrderID)
                }
                // Evict from tracker immediately — WS CANCELED event
                // may never arrive for already-gone orders.
                if ex.onOrderGone != nil {
                    ex.onOrderGone(d.Live.OrderID)
                }
            }(d)
        }
        wg.Wait() // ← BARRIER: blocks until all cancels complete
    }

    // ── Phase 2: Concurrent amends + places ─────────────────────────
    // Safe to place now — all stale orders are confirmed cancelled.
    var wg sync.WaitGroup

    for _, d := range amends {
        wg.Add(1)
        go func(d core.OrderDiff) {
            defer wg.Done()
            // Try WS edit if supported, else cancel+place fallback
            if wsExch.HasWsEditSupport() {
                if _, err := wsExch.EditOrderWs(ctx, d.Live.OrderID, ex.symbol, d.Desired.Price, d.Desired.Qty); err == nil {
                    return // Success
                }
            }
            // Fallback: cancel then place
            _ = wsExch.CancelOrderWs(ctx, ex.symbol, d.Live.OrderID)
            if ex.onOrderGone != nil {
                ex.onOrderGone(d.Live.OrderID)
            }
            ex.placeOrderWs(ctx, wsExch, d.Desired)
        }(d)
    }

    for _, d := range places {
        wg.Add(1)
        go func(d core.OrderDiff) {
            defer wg.Done()
            // WS place with REST fallback
            if _, err := ex.placeOrderWs(ctx, wsExch, d.Desired); err != nil {
                ex.placeOrderREST(ctx, d.Desired)
            }
        }(d)
    }

    wg.Wait()
    return nil
}
```

### Why Cancel-First Matters

```
Timeline WITHOUT cancel-first:
  t=0ms  Place BID@100.50 (new price)     ← fires immediately
  t=2ms  Cancel BID@100.30 (stale)        ← fires concurrently
  t=3ms  Exchange sees BID@100.50 arrive WHILE BID@100.30 still rests
         → Two bids resting = double exposure, or PostOnly reject

Timeline WITH cancel-first:
  t=0ms  Cancel BID@100.30 (stale)        ← Phase 1
  t=5ms  wg.Wait() confirms cancel done   ← Barrier
  t=5ms  Place BID@100.50 (new price)     ← Phase 2, safe
         → Clean single bid, no overlap
```

---

## Additional Methods to Implement

### CancelAll (synchronous, for direct calls)

```go
func (ex *Executor) CancelAll(ctx context.Context, reason string) error {
    return ex.cancelAll(ctx, reason)
}

func (ex *Executor) cancelAll(ctx context.Context, reason string) error {
    // Always REST — WS CancelAll is unreliable on Bybit and causes panics
    // in some CCXT implementations due to concurrent map access.
    return ex.exch.CancelAllOrders(ctx, ex.symbol)
}
```

### CancelAll via Execute (async dispatch)

When `Execute` receives `TickActionCancelAll`:
1. Lock `mu`, set `pending = nil` (discard any queued REPLACE — it's now stale)
2. Unlock `mu`
3. Dispatch `cancelAll` in a goroutine (non-blocking to the engine)

### buildAmendDiffs (for TickActionAmend)

Convert `output.OrdersToCancel` ([]string of order IDs) and `output.OrdersToAdd` ([]DesiredOrder) into `[]OrderDiff` by looking up live orders in a map.

### processState (dispatches to diff engine)

```go
func (ex *Executor) processState(ctx context.Context, p *pendingExecution) error {
    // For REPLACE: diffs = diffEngine.Diff(output.DesiredOrders, liveOrders)
    // For AMEND:   diffs = buildAmendDiffs(output, liveOrders)
    // Then: executeDiffs(ctx, diffs)
}
```

### Order Placement Helpers

```go
func (ex *Executor) placeOrderWs(ctx, wsExch, desired) (*Order, error) {
    // Build OrderRequest with Symbol, Side, Type="LIMIT", TimeInForce="GTX" (PostOnly),
    // Price, Quantity, ClientOrderID=desired.Tag
    // Call wsExch.PlaceOrderWs(ctx, req)
    // On success: log and invoke ex.onOrderPlaced(order, desired.LevelIndex)
}

func (ex *Executor) placeOrderREST(ctx, desired) {
    // Same as WS but via ex.exch.PlaceOrder(ctx, req)
    // REST fallback — used when WS place fails
}
```

---

## Callback Setters (unchanged interface)

```go
func (ex *Executor) SetOrderEventCallback(cb core.OrderEventCallback)
func (ex *Executor) SetOnOrderPlaced(cb func(order *exchange.Order, levelIndex int))
func (ex *Executor) SetOnOrderGone(cb func(orderID string))
```

---

## Constructor

```go
func NewExecutor(exch, symbol, diffEngine, execEngine) *Executor {
    return &Executor{
        exch:       exch,
        symbol:     symbol,
        diffEngine: diffEngine,
        execEngine: execEngine,
        execGate:   make(chan struct{}, 1), // buffered-1 = worker semaphore
    }
}
```

---

## Requirements Checklist

- [ ] `execGate` is `make(chan struct{}, 1)` — buffered exactly 1
- [ ] `pending` is `*pendingExecution`, protected by `sync.Mutex`
- [ ] `triggerWorker` uses non-blocking send on `execGate` to start worker
- [ ] Worker loops until `pending == nil`, then exits
- [ ] Deferred re-check after `<-execGate` handles the arrival race
- [ ] `TickActionKeep` returns immediately (no-op)
- [ ] `TickActionCancelAll` clears pending under lock, dispatches cancelAll async
- [ ] `executeDiffs` partitions into cancels/amends/places
- [ ] Phase 1: all cancels fire concurrently, `wg.Wait()` is a hard barrier
- [ ] Phase 2: amends + places fire concurrently after Phase 1
- [ ] WS cancel with REST fallback on error
- [ ] `onOrderGone` called after every cancel (evicts ghost orders)
- [ ] WS place with REST fallback on error
- [ ] `onOrderPlaced` called after every successful place
- [ ] Amend: try WS edit first, fall back to cancel+place
- [ ] `cancelAll` uses REST only (not WS) — see code comment for reason
- [ ] All logging uses `[EXECUTOR]` prefix
- [ ] `TimeInForce = "GTX"` (PostOnly) on all placements
- [ ] Package is `package engine`
- [ ] Imports: `context`, `fmt`, `log`, `sync`, plus internal packages
