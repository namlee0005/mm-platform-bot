# MAS Strategy — Implementation Plan

**Strategy:** Capital Preservation Market Maker (`mas_strategy.go` + `mas_engine.go`)
**Spec:** `docs/PLAN_MAS.md`

---

## Phase 1: Data Structures & Config (Day 1)

### 1.1 Config & State Models
- [ ] Define `MASConfig` struct in `internal/strategy/mas_engine.go` with nested `FairValueConfig`, `VolatilityConfig`, `InventoryConfig`, `ToxicityConfig`, `CircuitBreakerConfig`
- [ ] Define `MASState` struct: `Regime`, `FairValue`, `OFINormalized`, `NetPosition`, `InventoryRatio`, `ToxicPausedSide`, `ToxicFillCount`, `RealizedVol`, `SessionLoss`, `LastFeedTimestamp`
- [ ] All monetary fields use `decimal.Decimal` (shopspring/decimal) — enforce via code review
- [ ] Wire config to `config.yaml` under `mas:` key; validate at startup (panic on invalid values)
- [ ] Config stored in `atomic.Pointer[MASConfig]`; `OnTick` snapshots once at entry — never reloads mid-tick
- [ ] `CircuitBreakerConfig` fields: `MaxLossPerSession decimal.Decimal`, `MaxDriftThreshold decimal.Decimal`, `DriftConsecutiveLimit int`, `StaleFeedTimeoutMs int`

### 1.2 Depth Level Enum
- [ ] Define `DepthLevel` (`L0`–`L9`) and `Side` (`Bid`/`Ask`) types
- [ ] Implement `PriceAtLevel(side Side, level DepthLevel, fairValue decimal.Decimal, spread decimal.Decimal) decimal.Decimal`

### 1.3 Canonical Tick Contract
- [ ] Define `HistoricalTick` struct: `Timestamp time.Time`, `Book OrderBook`, `Trades []Trade`, `Fills []Fill`
- [ ] Both backtest runner and live adapter feed `chan HistoricalTick` — shared interface, no schema divergence

**Milestone:** Config loads, structs compile, all types defined, `HistoricalTick` interface in place.

---

## Phase 2: Fair Value Engine (Day 2)

### 2.1 Micro-Price
- [ ] Implement `ComputeMicroPrice(bestBid, bestAsk decimal.Decimal, bidQty, askQty decimal.Decimal) decimal.Decimal`
- [ ] Formula: `(bestAsk × bidQty + bestBid × askQty) / (bidQty + askQty)`
- [ ] Unit test: balanced book → returns mid-price; imbalanced book → pulls toward heavier side

### 2.2 OFI Calculation
- [ ] Implement `OFITracker` with `Add(trade Trade)` and `Normalized() decimal.Decimal` methods
- [ ] Internal rolling state uses `float64` for performance; `Normalized()` converts to `decimal.Decimal` at boundary before use in price math
- [ ] Exponential decay over `ofi_window`; normalize against **exponential running max with a floor** (`ofi_floor = 1e-6`) — never normalize against raw rolling max (division instability at session start)
- [ ] Unit test: buy-heavy sequence → positive OFI; sell-heavy → negative; decay resets toward zero; session-start normalization never returns Inf or NaN

### 2.3 Fair Value Combinator
- [ ] `ComputeFairValue(microPrice decimal.Decimal, ofiNorm decimal.Decimal, cfg FairValueConfig) decimal.Decimal`
- [ ] `fair_value = micro_price + ofi_normalized × ofi_alpha × tick_size`

**Milestone:** `ComputeFairValue` returns correct values across unit tests for all OFI extremes; session-start normalization stable.

---

## Phase 3: Volatility Regime Classifier (Day 3)

### 3.1 Realized Vol — EWMA Primary, Rolling Secondary
- [ ] Implement `VolatilityTracker` supporting two modes: `RollingStdDev` (configurable `vol_window`) and `EWMA` (configurable halflife, default 15s)
- [ ] Default mode: `EWMA` — more responsive to flash events; `RollingStdDev` available as fallback via config
- [ ] `RealizedVol() float64` — returns active mode's estimate
- [ ] Unit test: flat prices → near-zero vol; random walk → positive vol; EWMA responds within 2× halflife; rolling responds within full window
- [ ] Document in config comments: 60s rolling window lags flash crashes by ~30s; EWMA at halflife=15s detects same event in ~10s

### 3.2 Regime FSM
- [ ] Implement `ClassifyRegime(vol float64, current VolatilityRegime, cfg VolatilityConfig) VolatilityRegime`
- [ ] Hysteresis: enter Spike at `vol_threshold`, exit at `vol_exit_threshold`
- [ ] Unit test: confirm no thrashing at boundary values

**Milestone:** Regime switches correctly on simulated price series; EWMA detects flash-crash scenario faster than rolling window.

---

## Phase 4: Quote Generator (Days 4–5)

### 4.1 Sideways Quote Builder
- [ ] `BuildSidewaysQuotes(state MASState, cfg MASConfig) []Order`
- [ ] L1–L5 uniform sizing, skew applied
- [ ] Unit test: long inventory → bid prices shift down; short → ask prices shift up

### 4.2 Spike Quote Builder
- [ ] `BuildSpikeQuotes(state MASState, cfg MASConfig) []Order`
- [ ] L0–L3: `spike_near_spread_multiplier × base_spread`, `spike_near_size_ratio` sizes
- [ ] L4: empty (buffer)
- [ ] L5–L9: `spike_deep_offset_ticks` below fair value, `spike_deep_size_ratio` sizes
- [ ] Unit test: verify L4 always empty; L0 spread ≥ 4× base spread; L5–L9 sizes ≥ L0–L3 sizes

### 4.3 Skew Application
- [ ] `ApplyInventorySkew(orders []Order, inventoryRatio float64, cfg InventoryConfig) []Order`
- [ ] Unit test: at `inventory_ratio = 1.0`, skew = `max_skew_ticks` applied to both sides

### 4.4 Emergency Cap Handler
- [ ] `CheckEmergencyCap(netPosition decimal.Decimal, cfg InventoryConfig) (cancelBids, cancelAsks bool)`
- [ ] Unit test: position > `emergency_cap` → returns `cancelBids=true`

**Milestone:** Quote builder returns correct order sets for Sideways + Spike + extreme inventory.

---

## Phase 5: Toxicity Monitor (Day 6)

### 5.1 Fill Evaluator
- [ ] `EvaluateFill(fill Fill, priceHistory PriceHistory, cfg ToxicityConfig) bool`
- [ ] Returns `true` if price moved `toxic_move_ticks` against fill within `toxic_detection_window`
- [ ] `toxic_detection_window` is configurable per regime: normal regime uses default (8s); spike regime uses extended window (`toxic_spike_detection_window`, default 45s) to catch whale accumulation patterns

### 5.2 Tiered Pause Manager
- [ ] `ToxicityPauseManager.RecordFill(side Side, isToxic bool) time.Duration`
- [ ] Returns pause duration based on rolling count tier (0s / 15s / 45s / 120s)
- [ ] `IsPaused(side Side) bool` — checks `ToxicPausedSide` expiry
- [ ] Unit test: 6 toxic fills → 120s pause; only bid paused, ask still active

**Milestone:** Toxicity detection fires correctly on simulated adverse fills; pause is side-specific; spike regime uses extended detection window.

---

## Phase 5.5: Circuit Breaker, Position Reconciliation & Stale-Feed Guard (Day 6, parallel)

### 5.5.1 Circuit Breaker
- [ ] Define `CircuitBreakerConfig`: `MaxLossPerSession decimal.Decimal`, `MaxDriftThreshold decimal.Decimal`, `DriftConsecutiveLimit int`, `StaleFeedTimeoutMs int`
- [ ] `CircuitBreaker.Check(state MASState) (trip bool, reason string)` — evaluates all trip conditions:
  1. `state.SessionLoss > MaxLossPerSession`
  2. Position reconciliation delta > `MaxDriftThreshold` for ≥ `DriftConsecutiveLimit` consecutive cycles
  3. Kill file `/tmp/mas_kill` exists OR `SIGUSR1` received
  4. **Feed staleness:** `time.Since(state.LastFeedTimestamp) > StaleFeedTimeoutMs` (overrides toxicity pause — cancels ALL orders including L9)
- [ ] On trip: cancel all open orders (no exceptions), log reason with timestamp, halt tick loop
- [ ] Unit test: inject loss > threshold → trip; inject drift × 3 → trip; create kill file → trip; simulate feed silence for `StaleFeedTimeoutMs + 1s` → trip, L9 cancelled

### 5.5.2 Position Reconciler
- [ ] `PositionReconciler` goroutine: polls exchange REST position endpoint every 5s
- [ ] Compares `RemotePosition` to `state.NetPosition`; exposes `Delta() decimal.Decimal`
- [ ] On delta exceed: increments `DriftConsecutiveCount`; resets on delta within threshold
- [ ] Unit test: mock REST returns position + 2 lots drift → consecutive counter increments; reconciles on next clean poll

### 5.5.3 Config Hot-Reload Safety Constraints
- [ ] Hot-reload via `atomic.Pointer[MASConfig]` is permitted for: spread multipliers, size ratios, OFI alpha, vol thresholds
- [ ] **Prohibited from hot-reload** (require full drain + restart): `emergency_cap`, `max_inventory`, `max_loss_per_session`, `max_drift_threshold`
- [ ] At startup, immutable params are hashed and written to `/tmp/mas_immutable.lock`; reload watcher validates hash before accepting change; panics if immutable params differ
- [ ] Unit test: reload with changed `emergency_cap` → watcher rejects and logs error, keeps prior config

### 5.5.4 GC Tuning
- [ ] Set `GOGC=400` (trigger at 4× live heap) via environment variable in deployment config — not hardcoded
- [ ] Call `runtime.GC()` explicitly in the idle path between ticks (after order dispatch, before next book update) — never between OFI accumulation and quote generation
- [ ] Unit test: run 10,000 ticks with 24h rolling OFI history loaded; confirm heap stays bounded; confirm no GC pause >5ms during quote generation phase

**Milestone:** All four conditions trip circuit breaker; stale feed cancels L9; immutable config reloads rejected; GC bounded.

---

## Phase 6: Strategy Orchestrator (Day 7)

### 6.1 `mas_strategy.go` Main Loop
- [ ] `MASStrategy.OnTick(tick HistoricalTick) []OrderAction`
- [ ] Tick-entry: snapshot `cfg := configPtr.Load()` — single immutable config for entire tick; update `state.LastFeedTimestamp = tick.Timestamp`
- [ ] Sequence: update OFI → compute fair value → classify regime → build quotes → apply skew → check emergency cap → check toxicity pauses → check circuit breaker → return actions
- [ ] `OrderAction` union type: `Place(order)`, `Cancel(id)`, `CancelSide(side)`, `Halt(reason)`

### 6.2 Integration
- [ ] Wire `MASStrategy` into exchange adapter via `chan HistoricalTick` — same interface as backtest runner (no gRPC boundary in execution path; add only if Python latency is measured as bottleneck)
- [ ] Log state snapshot per tick to `session-{id}.json`: regime, fair_value, ofi, vol, inventory, paused_sides, circuit_breaker_state, last_feed_age_ms

### 6.3 Config Hot-Reload
- [ ] Watch `config.yaml` for changes; update `atomic.Pointer[MASConfig]` — takes effect on next tick boundary, never mid-tick
- [ ] Reject changes to immutable params (see §5.5.3)

**Milestone:** Strategy compiles, runs dry against recorded market data; stale feed → L9 cancels; kill signal halts cleanly.

---

## Phase 7: Backtesting & Calibration (Days 8–9)

- [ ] Backtest runner reads historical data as `chan HistoricalTick` — same interface as live adapter
- [ ] Run against 7-day historical data (calm + 3 spike events minimum; include at least one WebSocket gap/reconnect event)
- [ ] Measure: fill rate at L0–L3 vs. L5–L9 during spike events; inventory excursion max; toxic fill rate before/after pause; stale-feed trip frequency; GC pause distribution
- [ ] Calibrate `ofi_alpha`, `vol_threshold`, `spike_near_size_ratio`, `spike_deep_size_ratio`, `toxic_spike_detection_window` per instrument
- [ ] Targets: zero fills at L0–L3 during confirmed spike; inventory within `max_inventory` 99% of time; zero circuit breaker false-trips on clean historical data; stale-feed guard trips on every recorded reconnect gap

---

## Phase 8: Paper Trading (Days 10–14)

- [ ] Deploy against live feed with simulated order execution (no real capital)
- [ ] Monitor: PnL attribution (OFI edge, spread capture, inventory carry), regime transition frequency, toxicity pause triggers, position reconciliation delta distribution, stale-feed guard activations
- [ ] Confirm circuit breaker does NOT false-trip over 5-day window
- [ ] Fix any edge cases found; freeze config values before going live

---

## Architectural Decisions

| Decision | Choice | Rationale |
|---|---|---|
| OFI boundary type | `float64` internal, `decimal.Decimal` at price boundary | Performance in rolling math; precision where it enters monetary calculations |
| OFI normalization | Exp running max with floor `1e-6` | Prevents Inf/NaN at session start; more stable than raw rolling max |
| Vol estimator | EWMA (halflife=15s) default, rolling available | EWMA detects flash crashes in ~10s vs 30s lag for 60s rolling window |
| Config reload | `atomic.Pointer[MASConfig]`, loaded once per tick | Prevents mid-tick inconsistency; immutable params locked at startup |
| Kill switch | File + signal + loss threshold + stale feed | Defense-in-depth; no single failure mode blocks emergency stop |
| L9 during stale feed | **Cancel** — stale-data circuit breaker overrides compliance anchor | L9 priced against stale fair value is unhedged exposure, not a compliance asset |
| Position truth | Exchange REST poll every 5s | Local fill tracking can diverge on WS reconnect; exchange is authoritative |
| Data contract | `HistoricalTick` shared by backtest + live | Schema parity ensures backtest results transfer to production |
| Go/Python split | Single-process Go strategy; no gRPC execution boundary | Polymarket latency is exchange-RTT-bound; gRPC adds failure mode without measurable gain |
| GC strategy | `GOGC=400` + explicit `runtime.GC()` between ticks | Bounded heap; deterministic critical path; avoids OOM risk of `GOGC=off` |

---

## Milestones Summary

| Milestone | Target | Definition of Done |
|-----------|--------|-------------------|
| M1: Types Compile | Day 1 | All structs, enums, config load without error; `HistoricalTick` interface defined |
| M2: Fair Value | Day 2 | `ComputeFairValue` unit tests pass; OFI decimal boundary enforced; normalization floor stable |
| M3: Regime | Day 3 | Regime FSM hysteresis tests pass; EWMA detects flash-crash faster than rolling |
| M4: Quotes | Day 5 | Sideways + Spike quote builders verified |
| M5: Toxicity | Day 6 | Tiered pause tests pass; spike regime uses extended detection window |
| M5.5: Circuit Breaker | Day 6 | All four trip conditions verified; stale feed cancels L9; immutable config reloads rejected; GC bounded |
| M6: Live Dry Run | Day 7 | Strategy runs on recorded data; kill signal + stale feed halt cleanly |
| M7: Backtested | Day 9 | Calibrated config; zero false circuit trips; stale-feed guard trips on all recorded gaps |
| M8: Paper Live | Day 14 | 5 days paper trading; no reconciliation drift > threshold; heap bounded |