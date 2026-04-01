# MAS Strategy — Implementation Plan

**Strategy:** Capital Preservation Market Maker (`mas_strategy.go` + `mas_engine.go`)
**Spec:** `docs/PLAN_MAS.md`

---

## Phase 1: Data Structures & Config (Day 1)

### 1.1 Config & State Models
- [ ] Define `MASConfig` struct in `internal/strategy/mas_engine.go` with nested `FairValueConfig`, `VolatilityConfig`, `InventoryConfig`, `ToxicityConfig`
- [ ] Define `MASState` struct: `Regime`, `FairValue`, `OFINormalized`, `NetPosition`, `InventoryRatio`, `ToxicPausedSide`, `ToxicFillCount`, `RealizedVol`
- [ ] All monetary fields use `decimal.Decimal` (shopspring/decimal) — enforce via code review
- [ ] Wire config to `config.yaml` under `mas:` key; validate at startup (panic on invalid values)

### 1.2 Depth Level Enum
- [ ] Define `DepthLevel` (`L0`–`L9`) and `Side` (`Bid`/`Ask`) types
- [ ] Implement `PriceAtLevel(side Side, level DepthLevel, fairValue decimal.Decimal, spread decimal.Decimal) decimal.Decimal`

**Milestone:** Config loads, structs compile, all types defined.

---

## Phase 2: Fair Value Engine (Day 2)

### 2.1 Micro-Price
- [ ] Implement `ComputeMicroPrice(bestBid, bestAsk decimal.Decimal, bidQty, askQty decimal.Decimal) decimal.Decimal`
- [ ] Formula: `(bestAsk × bidQty + bestBid × askQty) / (bidQty + askQty)`
- [ ] Unit test: balanced book → returns mid-price; imbalanced book → pulls toward heavier side

### 2.2 OFI Calculation
- [ ] Implement `OFITracker` with `Add(trade Trade)` and `Normalized() float64` methods
- [ ] Exponential decay over `ofi_window`; normalize against rolling max
- [ ] Unit test: buy-heavy sequence → positive OFI; sell-heavy → negative; decay resets toward zero

### 2.3 Fair Value Combinator
- [ ] `ComputeFairValue(microPrice decimal.Decimal, ofiNorm float64, cfg FairValueConfig) decimal.Decimal`
- [ ] `fair_value = micro_price + ofi_normalized × ofi_alpha × tick_size`

**Milestone:** `ComputeFairValue` returns correct values across unit tests for all OFI extremes.

---

## Phase 3: Volatility Regime Classifier (Day 3)

### 3.1 Realized Vol
- [ ] Implement `VolatilityTracker` with rolling returns buffer (configurable `vol_window`)
- [ ] `RealizedVol() float64` — stddev of log returns
- [ ] Unit test: flat prices → near-zero vol; random walk → positive vol

### 3.2 Regime FSM
- [ ] Implement `ClassifyRegime(vol float64, current VolatilityRegime, cfg VolatilityConfig) VolatilityRegime`
- [ ] Hysteresis: enter Spike at `vol_threshold`, exit at `vol_exit_threshold`
- [ ] Unit test: confirm no thrashing at boundary values

**Milestone:** Regime switches correctly on simulated price series.

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

### 5.2 Tiered Pause Manager
- [ ] `ToxicityPauseManager.RecordFill(side Side, isToxic bool) time.Duration`
- [ ] Returns pause duration based on rolling count tier (0s / 15s / 45s / 120s)
- [ ] `IsPaused(side Side) bool` — checks `ToxicPausedSide` expiry
- [ ] Unit test: 6 toxic fills → 120s pause; only bid paused, ask still active

**Milestone:** Toxicity detection fires correctly on simulated adverse fills; pause is side-specific.

---

## Phase 6: Strategy Orchestrator (Day 7)

### 6.1 `mas_strategy.go` Main Loop
- [ ] `MASStrategy.OnTick(book OrderBook, trades []Trade, fills []Fill) []OrderAction`
- [ ] Sequence: update OFI → compute fair value → classify regime → build quotes → apply skew → check emergency cap → check toxicity pauses → return actions
- [ ] `OrderAction` union type: `Place(order)`, `Cancel(id)`, `CancelSide(side)`

### 6.2 Integration
- [ ] Wire `MASStrategy` into exchange adapter (same interface as existing strategies)
- [ ] Log state snapshot per tick to `session-{id}.json`: regime, fair_value, ofi, vol, inventory, paused_sides

### 6.3 Config Hot-Reload
- [ ] Watch `config.yaml` for changes; reload `MASConfig` without restart

**Milestone:** Strategy compiles, runs dry against recorded market data, produces order actions.

---

## Phase 7: Backtesting & Calibration (Days 8–9)

- [ ] Run against 7-day historical data (calm + 3 spike events minimum)
- [ ] Measure: fill rate at L0–L3 vs. L5–L9 during spike events; inventory excursion max; toxic fill rate before/after pause
- [ ] Calibrate `ofi_alpha`, `vol_threshold`, `spike_near_size_ratio`, `spike_deep_size_ratio` per instrument
- [ ] Target: zero fills at L0–L3 during confirmed spike; inventory stays within `max_inventory` 99% of time

---

## Phase 8: Paper Trading (Days 10–14)

- [ ] Deploy against live feed with simulated order execution (no real capital)
- [ ] Monitor: PnL attribution (OFI edge, spread capture, inventory carry), regime transition frequency, toxicity pause triggers
- [ ] Fix any edge cases found; freeze config values before going live

---

## Milestones Summary

| Milestone | Target | Definition of Done |
|-----------|--------|-------------------|
| M1: Types Compile | Day 1 | All structs, enums, config load without error |
| M2: Fair Value | Day 2 | `ComputeFairValue` unit tests pass |
| M3: Regime | Day 3 | Regime FSM hysteresis tests pass |
| M4: Quotes | Day 5 | Sideways + Spike quote builders verified |
| M5: Toxicity | Day 6 | Tiered pause tests pass; side-specificity confirmed |
| M6: Live Dry Run | Day 7 | Strategy runs on recorded data without panic |
| M7: Backtested | Day 9 | Calibrated config, spike behavior verified |
| M8: Paper Live | Day 14 | 5 days paper trading with acceptable PnL variance |