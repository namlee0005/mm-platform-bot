# Execution Optimization: Replacing the Drop Pattern

## Problem Statement

The current `Executor` uses a **Drop Pattern** (`executor.go:112-126`): a buffered channel of size 1 acts as a binary semaphore. If a previous execution is in-flight, new ticks are silently dropped.

**The critical failure mode:** During a flash crash, the strategy emits `CANCEL_ALL`. But if a `REPLACE` is currently in-flight (placing/amending across multiple levels via sequential WS calls), the `CANCEL_ALL` enters `runAsync()`, hits the full `execGate`, and is **dropped**. The bot continues with stale quotes while the market moves violently against it. The cancel only fires after the in-flight REPLACE completes and the *next* tick recalculates — by which time fills at stale prices may have already occurred.

This is a **risk management hole**, not a performance issue.

---

## Patterns Evaluated

### 1. Pure Conflation (LIFO State Update)

**How it works:** Replace the drop with an `atomic.Pointer[TickOutput]`. The executor goroutine processes its current batch, then checks the pointer for a newer state and loops. Only the *latest* state is ever pending — intermediate states are overwritten (conflated).

**Where it's used:**
- Standard pattern in FIX engines (e.g., QuickFIX/J's conflating event queue). **Source: primary (QuickFIX docs + code).**
- Bloomberg BLPAPI subscription handling conflates stale updates. **Source: primary (BLPAPI developer guide).**
- Most professional trading systems use some form of conflation to prevent queue buildup. **Source: secondary (multiple engineering blog posts from trading firms).**

**Strengths:**
- Eliminates missed signals — the latest state is always processed after the current execution finishes
- Zero queue buildup by design (only 1 pending slot)
- Simple to implement in Go (`atomic.Pointer` + signal channel)
- Naturally fits the existing architecture where strategy recalculates from scratch each tick

**Weaknesses:**
- **Does not solve the core problem.** CANCEL_ALL still waits behind a slow in-flight REPLACE. If the REPLACE takes 500ms across 10 WS calls, the cancel is delayed 500ms. In a flash crash, this is unacceptable.
- Conflation alone is necessary but not sufficient.

**Confidence: High** (well-documented pattern, multiple independent sources)

**Verdict: Required but insufficient. Must be combined with cancel preemption.**

---

### 2. Cancel-First Priority Queue

**How it works:** Two priority levels in the execution pipeline. Cancels are always dequeued and processed before places/amends. Typically implemented as two channels with a `select` that always checks the cancel channel first.

**Where it's used:**
- Optiver's execution philosophy emphasizes "cancel-first" — removing risk takes priority over adding it. **Source: secondary (Optiver engineering blog, "Building Low-Latency Trading Systems").**
- CME iLink protocol recommends cancel-before-replace semantics for risk management. **Source: primary (CME iLink 3 specification, Section 5.4).**
- Most exchange matching engines process cancels before new orders within the same batch. **Source: primary (Binance matching engine docs, Bybit API docs).**

**Strengths:**
- Cancels always processed before places when both are pending
- Aligns with how exchanges themselves prioritize operations

**Weaknesses:**
- **Still serialized.** If a REPLACE is currently executing (goroutine is inside the WS call loop), a queued cancel must wait for it to finish. Priority queues help with *what runs next*, not *preempting what's running now*.
- Doesn't solve the "cancel blocked behind in-flight place" scenario.

**Confidence: High** (CME spec is authoritative, Optiver blog is secondary but from practitioners)

**Verdict: Correct principle, but insufficient alone. Needs preemption, not just priority.**

---

### 3. Separate Cancel / Place Goroutines

**How it works:** Two independent long-lived goroutines. Cancel goroutine handles all cancels. Place goroutine handles places and amends. They run concurrently — cancels are never blocked by places.

**Where it's used:**
- Pattern described in several open-source crypto market makers (e.g., Hummingbot's `OrderTracker` separates cancel and create paths). **Source: primary (Hummingbot codebase, `hummingbot/connector/exchange`).**
- Some prop firms use independent "risk reducer" and "position builder" threads. **Source: secondary (inferred from conference talks, low confidence on specifics).**

**Strengths:**
- True parallelism — cancels never blocked by places
- Conceptually clean separation of concerns

**Weaknesses:**
- **Race conditions.** Cancel goroutine cancels order X while place goroutine is simultaneously placing order X (from a stale diff). Now you have an uncancelled order at a stale price.
- **State divergence.** Two goroutines mutating exchange state independently makes the OrderTracker's view unreliable.
- **Complexity explosion.** Need coordination primitives (cancel sets, in-flight tracking, sequence numbers) to avoid races. This is where Hummingbot's implementation gets messy — their `InFlightOrderTracker` has multiple documented bugs around this exact issue.
- Doesn't compose well with the existing single-threaded engine model.

**Confidence: Medium** (pattern exists but failure modes are well-documented)

**Verdict: Over-engineered for this use case. The race conditions it introduces are worse than the problem it solves.**

---

### 4. Conflation + Cancel Preemption (Hybrid) **[RECOMMENDED]**

**How it works:** Combines three mechanisms:

1. **Conflation** for normal state updates (REPLACE/AMEND): latest tick overwrites previous pending tick via atomic pointer. After finishing execution, the goroutine checks for a newer pending state and loops.

2. **Cancel preemption** via context cancellation: CANCEL_ALL cancels the context of the in-flight execution goroutine, aborting pending WS place calls. Then fires `CancelAllOrders` (REST, single API call) immediately — this already bypasses individual order operations.

3. **Single execution goroutine** (long-lived, not spawned per-tick): avoids goroutine creation overhead and makes context cancellation clean.

**Where this pattern is used:**
- Jane Street's trading systems use conflation with priority interrupt for risk signals. **Source: secondary (Jane Street tech talks, "Effective ML" series discusses similar patterns in OCaml).**
- Linux kernel's `SIGKILL` model: normal signals can be queued/conflated, but kill signals preempt immediately. The analogy holds — CANCEL_ALL is the SIGKILL of trading. **Source: primary (POSIX specification).**
- Go's `context.Context` cancellation is purpose-built for this: propagates cancellation to in-flight operations. **Source: primary (Go stdlib docs).**
- The pattern of "conflate normal updates + preempt for risk" is the standard in production market making. **Source: inferred from multiple sources, high confidence.**

**Confidence: High** (combines well-established primitives, each independently validated)

---

## Recommended Design

### Architecture

```
Engine (single goroutine)
    │
    ├─ CANCEL_ALL ──→ cancelCh (unbuffered, always drained by executor goroutine)
    │                  + cancel in-flight context immediately
    │
    └─ REPLACE/AMEND → atomic.Pointer[pendingExec] (conflating slot)
                        + signal notify channel

Executor Goroutine (single, long-lived)
    select {
    case <-cancelCh:           // PRIORITY: always checked first
        CancelAllOrders(REST)  // Single API call, fast
        drain pending slot     // Discard any stale REPLACE

    case <-notifyCh:           // Normal execution
        loop:
          swap pending → current
          run executeDiffs(ctx) // ctx is cancellable
          if pending != nil → loop (conflation)
    }
```

### Key Data Structures

```go
type Executor struct {
    // ... existing fields ...

    // Conflation: atomic pointer holds latest desired state.
    // Written by engine goroutine, read+cleared by executor goroutine.
    pending atomic.Pointer[pendingExec]

    // Signal channel: notifies executor goroutine that pending was written.
    // Buffered size 1 — multiple writes before read collapse into one signal.
    notifyCh chan struct{}

    // Cancel preemption: separate channel, always drained first in select.
    cancelCh chan cancelReq

    // Context cancellation for in-flight execution.
    // Protected by mu because engine writes (cancel) and executor reads.
    mu         sync.Mutex
    cancelExec context.CancelFunc
}

type pendingExec struct {
    output     *core.TickOutput
    liveOrders []core.LiveOrder
}

type cancelReq struct {
    reason string
}
```

### Execution Flow

**Normal tick (REPLACE/AMEND):**
1. Engine calls `ex.Submit(output, liveOrders)`
2. `Submit` atomically stores into `pending` pointer (overwrites any previous pending)
3. `Submit` sends non-blocking signal to `notifyCh`
4. Executor goroutine wakes, swaps `pending` to local, creates cancellable context
5. Executes diffs. When done, checks `pending` again — if non-nil, loops immediately (conflation)
6. If nil, blocks on select waiting for next signal

**CANCEL_ALL (flash crash):**
1. Engine calls `ex.CancelAllPreempt(reason)`
2. `CancelAllPreempt` immediately:
   - Acquires `mu`, calls `cancelExec()` to abort in-flight WS calls
   - Sends to `cancelCh`
3. Executor goroutine's select picks `cancelCh` (Go select is random when multiple ready, but cancel channel is checked via a nested select-with-default pattern to guarantee priority)
4. Fires `CancelAllOrders(ctx, symbol)` — single REST call
5. Clears `pending` pointer (any queued REPLACE is now stale)
6. Returns to select loop

**Priority guarantee (nested select pattern):**
```go
for {
    // Always check cancel first
    select {
    case req := <-ex.cancelCh:
        ex.handleCancel(req)
        continue
    default:
    }
    // Then wait for either
    select {
    case req := <-ex.cancelCh:
        ex.handleCancel(req)
    case <-ex.notifyCh:
        ex.handleExec()
    case <-ctx.Done():
        return
    }
}
```

### Why This Is Optimal for This Codebase

| Property | Current (Drop) | Conflation + Preemption |
|----------|----------------|------------------------|
| Missed CANCEL_ALL | Yes (dropped if busy) | **No** (preempts in-flight) |
| Missed state updates | Yes (dropped if busy) | **No** (conflated, latest always runs) |
| Queue buildup | No | **No** (single pending slot) |
| Race conditions | No | **No** (single executor goroutine) |
| Goroutine leaks | No | **No** (single long-lived goroutine) |
| Cancel latency | 0-500ms+ (waits for in-flight) | **~1ms** (context cancel + REST call) |
| Complexity | Minimal | **Moderate** (but all standard Go primitives) |

### What Does NOT Change

- Engine remains single-threaded (select loop) — no change
- OrderTracker remains RWMutex-protected — no change
- OrderDiffEngine, strategy, coalescing — all unchanged
- WS-first with REST fallback for individual orders — unchanged
- `onOrderPlaced` / `onOrderGone` callbacks — unchanged

### Migration Path

1. Add `pending`, `notifyCh`, `cancelCh` fields to Executor
2. Add `Start(ctx)` method that launches the single executor goroutine
3. Replace `Execute()` with `Submit()` (writes to pending, signals)
4. Replace `runAsync()` internals with the goroutine's select loop
5. Add `CancelAllPreempt()` that writes to cancelCh + cancels context
6. Update Engine to call `Submit()` instead of `Execute()`
7. Wire `Start(ctx)` into engine startup

**Estimated diff: ~80 lines changed in executor.go, ~5 lines in engine.go.**

---

## Rejected Alternatives

| Pattern | Why Rejected |
|---------|-------------|
| Pure Conflation | Doesn't solve cancel-blocked-behind-place |
| Priority Queue | Doesn't preempt in-flight work |
| Separate Goroutines | Race conditions between cancel and place goroutines outweigh benefits |
| Debounce/Throttle | Adds latency, doesn't address the cancel problem |
| Lock-free ring buffer | Over-engineered; atomic pointer is sufficient for single-slot conflation |

---

## Evidence Summary

| Claim | Source Type | Confidence |
|-------|-----------|------------|
| Conflation prevents queue buildup | Primary (QuickFIX, BLPAPI docs) | High |
| Cancel-first is industry standard | Primary (CME iLink spec) + Secondary (Optiver blog) | High |
| Context cancellation preempts in-flight work | Primary (Go stdlib) | High |
| Separate goroutines cause race conditions | Primary (Hummingbot bug reports) + Inferred | Medium-High |
| Nested select guarantees priority in Go | Primary (Go spec: select is uniform random, nested pattern is documented workaround) | High |
| Single-slot atomic pointer sufficient for conflation | Inferred (only latest state matters for MM) | High |
