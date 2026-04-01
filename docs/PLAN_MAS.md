# MAS Strategy — Capital Preservation Market Maker

**Motto:** _"Không thua tiền là được"_ — Just don't lose money.
**Date:** 2026-04-01
**Files:** `internal/strategy/mas_strategy.go`, `internal/strategy/mas_engine.go`

---

## 1. Design Philosophy

This strategy is **not** a PnL-maximizer. It is a **loss-minimizer**. Every algorithm choice optimizes for one thing first: avoid giving money to informed flow. Profit is a side effect of surviving long enough.

The four pillars:
1. **Fair Value** — Quote around a smarter mid, not the naive mid.
2. **Volatility Regime** — Behave differently when the market is moving fast.
3. **Inventory Control** — Never let inventory drift into unhedgeable territory.
4. **Toxicity Pause** — If you got hit by informed flow, stop feeding that side.

---

## 2. Pillar 1: Fair Value (State-Dependent)

Fair value computation depends on the current **market state**, determined by the Spike Score `S` from Pillar 2.

### 2.1 Normal State (S < 2.0): Micro-Price + OFI

#### Micro-Price

The naive mid-price `(best_bid + best_ask) / 2` ignores orderbook imbalance. A better fair value estimate is the **volume-weighted mid**:

```
micro_price = (best_ask × bid_qty + best_bid × ask_qty) / (bid_qty + ask_qty)
```

This pulls the fair value toward the side with more liquidity — if there are 10x more bids than asks at the top of book, the true fair value is closer to the ask.

#### OFI Adjustment

Order Flow Imbalance (OFI) measures net directional pressure from recent trades:

```
ofi = Σ (buy_volume - sell_volume) over last N seconds
ofi_normalized = ofi / max_observed_ofi   ∈ [-1.0, +1.0]
```

The final fair value in Normal state:

```
fair_value = micro_price + (ofi_normalized × ofi_alpha × tick_size)
```

- `ofi_alpha` defaults to `3.0` ticks — calibrate per instrument.
- Positive OFI → fair value shifts up → bid shallower, ask tighter.
- Negative OFI → fair value shifts down → bid tighter, ask shallower.

### 2.2 Under Attack State (S >= 2.0): VWAP Anchor Override

**Problem:** During spike events, Micro-Price becomes **unreliable**. Informed attackers spam/spoof one side of the book, yanking the volume-weighted mid into a trap. If we keep trusting Micro-Price + OFI, our bids follow the noise upward and we get dumped on at inflated levels.

**Solution:** When `S >= 2.0`, override the fair value entirely:

```
fair_value = max(vwap_3m, avg_sell_price_1h)
```

| Component | Definition |
|-----------|------------|
| `vwap_3m` | Volume-Weighted Average Price over the last 3 minutes of actual trades |
| `avg_sell_price_1h` | Average execution price of all sell trades in the last 1 hour |

**Why these two anchors:**
- `vwap_3m` is resistant to spoofing — it uses **executed** trades, not resting orders. Short window tracks genuine moves.
- `avg_sell_price_1h` is a structural floor — the price at which real sellers were willing to transact. If the current dump is just noise, this holds.
- `max()` ensures we always anchor to the **higher** of the two, keeping bids conservative during attacks.

**Effect on quoting:**
- **Bids shift deep down** — the anchored fair value resists the artificial pull-up from spoofed books, so bids are placed well below where the attacker wants to dump.
- **Asks stay elevated** — we keep offering at fair levels, catching any genuine demand if the spike is real.
- Net result: the bot **refuses to be the exit liquidity** for the attacker.

**SLA Note:** Exchanges expect tighter quoting under normal conditions, but dropping SLA KPIs (spread, uptime at L1) for a few minutes during genuine volatility is acceptable and standard practice. Capital preservation beats compliance metrics during spikes.

**Config:**
```yaml
mas:
  fair_value:
    ofi_window: 10s
    ofi_alpha: 3.0
    ofi_decay: 0.85
    # Under Attack overrides
    vwap_window: 3m
    avg_sell_price_window: 1h
```

---

## 3. Pillar 2: Volatility Regime & Market State

Three states, determined by the **Spike Score (S)**: **Sideways**, **Spike**, and **Under Attack**.

### 3.1 Spike Score & State Detection

```
realized_vol = stddev(mid_price_returns, window=60s) × sqrt(annualization_factor)
S = realized_vol / vol_threshold
```

| Condition | State | Fair Value Source |
|-----------|-------|-------------------|
| `S < 1.0` | **Sideways** | Micro-Price + OFI |
| `1.0 <= S < 2.0` | **Spike** | Micro-Price + OFI |
| `S >= 2.0` | **Under Attack** | `max(vwap_3m, avg_sell_price_1h)` |

`vol_threshold` defaults to `0.0025` (0.25% per minute). Hysteresis: enter Spike at `S=1.0`, exit at `S=0.6`. Enter Under Attack at `S=2.0`, exit at `S=1.5`. Prevents thrashing between states.

### 3.2 Sideways Quoting (S < 1.0)

Standard symmetric quoting around `fair_value`. Spread = `base_spread`. Sizes uniform across L1–L5.

```
bid_price[L] = fair_value - base_spread/2 - (L × tick_size × level_step)
ask_price[L] = fair_value + base_spread/2 + (L × tick_size × level_step)
size[L] = base_size
```

### 3.3 Spike Quoting (1.0 <= S < 2.0)

In a Spike, informed traders are active. The goal: **get out of their way at L0–L3, but catch over-extended moves at L5–L9**. Fair value still uses Micro-Price + OFI — the book is stressed but not yet manipulated.

```
L0–L3 (Near Touch): Massively widened spread + thinned size
  bid_price[L] = fair_value - (base_spread × spike_near_multiplier) / 2 - (L × wide_step)
  size[L] = base_size × spike_near_size_ratio   # e.g. 0.20 → 20% of normal

L4 (Buffer): Empty — no orders placed

L5–L9 (Deep Catch): Normal spread increments but THICK size
  bid_price[L] = fair_value - deep_offset - ((L-5) × tick_size × level_step)
  size[L] = base_size × spike_deep_size_ratio   # e.g. 1.50 → 150% of normal
```

**Rationale:** L0–L3 catches noise fills from informed flow — unprofitable. L5–L9 only fills on genuine over-extension (panic dumps/pumps) — these fills revert.

### 3.4 Under Attack Quoting (S >= 2.0)

The book is actively being manipulated. Micro-Price is **compromised** by spoofing/spamming. Fair value switches to the VWAP anchor (see §2.2). Quoting becomes maximally defensive:

```
L0–L3 (Near Touch): PULLED — no bid orders at all
  bid orders cancelled entirely
  ask_price[L] = fair_value + base_spread/2 + (L × tick_size × level_step)
  ask_size[L] = base_size × attack_ask_size_ratio   # e.g. 0.50 → 50% of normal

L4 (Buffer): Empty — no orders placed

L5–L9 (Deep Catch): Bids placed FAR below anchored fair value
  bid_price[L] = fair_value - attack_deep_offset - ((L-5) × tick_size × level_step)
  bid_size[L] = base_size × attack_deep_size_ratio   # e.g. 2.00 → 200% of normal
```

**Effect:**
- Near-touch bids are **completely absent** — the attacker has no one to dump on at reasonable prices.
- Asks remain elevated to capture any genuine buying demand.
- Deep bids only exist at extreme levels — they catch panic over-shoots that revert.
- The bot becomes a **wall of asks and a void of bids** near the market.

**SLA Impact:** Pulling L0–L3 bids means the bot temporarily fails spread and uptime SLA requirements. This is **acceptable** — exchanges understand that market makers widen or pull during genuine volatility. A few minutes of SLA miss is nothing compared to getting dumped on for 5–10x the daily PnL.

**Config:**
```yaml
mas:
  volatility:
    vol_threshold: 0.0025
    vol_window: 60s
    # State transition thresholds (as multiples of vol_threshold)
    spike_enter_score: 1.0
    spike_exit_score: 0.6
    attack_enter_score: 2.0
    attack_exit_score: 1.5
    # Spike quoting (1.0 <= S < 2.0)
    spike_near_spread_multiplier: 4.0
    spike_near_size_ratio: 0.20
    spike_deep_offset_ticks: 15
    spike_deep_size_ratio: 1.50
    # Under Attack quoting (S >= 2.0)
    attack_ask_size_ratio: 0.50
    attack_deep_offset_ticks: 25
    attack_deep_size_ratio: 2.00
```

---

## 4. Pillar 3: Inventory Control

### 4.1 Inventory Skew

When inventory deviates from zero, skew quotes to mean-revert it:

```
inventory_ratio = net_position / max_inventory   ∈ [-1.0, +1.0]
skew_ticks = inventory_ratio × max_skew_ticks

bid_price[L] -= skew_ticks   # Long inventory → lower bids (discourage buying)
ask_price[L] -= skew_ticks   # Long inventory → lower asks (encourage selling)
```

- `max_skew_ticks` defaults to `5`. At max inventory, bids shift 5 ticks down, asks shift 5 ticks down — hard asymmetric pressure to sell.
- Skew applies in both Sideways and Spike regimes.

### 4.2 Hard Inventory Caps

```yaml
mas:
  inventory:
    max_inventory: 1000
    max_skew_ticks: 5
    emergency_cap: 1200
    emergency_side_cancel: true
```

When `abs(net_position) > emergency_cap`:
- If long beyond cap → cancel ALL bids immediately; only asks remain.
- If short beyond cap → cancel ALL asks immediately; only bids remain.

This is the last line of defense against a runaway inventory position.

---

## 5. Pillar 4: Toxicity Pause

### 5.1 Detection

A fill is **toxic** if the price moved adversely by more than `toxic_move_threshold` ticks within `toxic_detection_window` after the fill.

```go
// After each fill:
if priceMovedAgainst(fill, toxicMoveTicks, toxicWindow) {
    toxicFillCount[side]++
    if toxicFillCount[side] >= toxicFillsRequired {
        pauseSide(side, toxicPauseDuration)
    }
}
```

### 5.2 Pause Behavior

During a toxicity pause on side `S`:
- All orders on side `S` are cancelled immediately.
- No new orders placed on side `S` for `N` seconds.
- The opposite side continues quoting normally (or even tightens asks to sell inventory acquired toxically).

### 5.3 Tiered Pause Duration

| Toxic Fill Count (rolling 5min) | Pause Duration |
|---------------------------------|---------------|
| 2–3 fills | 15s |
| 4–5 fills | 45s |
| 6+ fills | 120s |

**Config:**
```yaml
mas:
  toxicity:
    toxic_move_ticks: 3
    toxic_detection_window: 8s
    tier1_count: 2
    tier1_pause: 15s
    tier2_count: 4
    tier2_pause: 45s
    tier3_count: 6
    tier3_pause: 120s
    rolling_window: 300s
```

---

## 6. Data Structures

```go
// mas_engine.go

type MASConfig struct {
    FairValue  FairValueConfig
    Volatility VolatilityConfig
    Inventory  InventoryConfig
    Toxicity   ToxicityConfig
}

type MASState struct {
    MarketState     MarketState
    SpikeScore      float64
    FairValue       decimal.Decimal
    FairValueSource string          // "micro_price_ofi" or "vwap_anchor"
    VWAP3m          decimal.Decimal
    AvgSellPrice1h  decimal.Decimal
    OFINormalized   float64
    NetPosition     decimal.Decimal
    InventoryRatio  float64
    ToxicPausedSide map[Side]time.Time
    ToxicFillCount  map[Side]int
    RealizedVol     float64
}

type MarketState int

const (
    StateSideways    MarketState = 0  // S < 1.0
    StateSpike       MarketState = 1  // 1.0 <= S < 2.0
    StateUnderAttack MarketState = 2  // S >= 2.0
)
```

---

## 7. Quote Generation Flow

```
every tick:
  1. Compute micro_price from top-of-book
  2. Compute ofi_normalized from recent trade tape
  3. Compute realized_vol = stddev(recent returns)
  4. S = realized_vol / vol_threshold
  5. market_state = classify(S)  // with hysteresis

  6. if market_state == Sideways OR market_state == Spike:
       fair_value = micro_price + ofi_adjustment
     elif market_state == UnderAttack:
       fair_value = max(vwap_3m, avg_sell_price_1h)

  7. inventory_ratio = net_position / max_inventory
  8. skew_ticks = inventory_ratio × max_skew_ticks

  if market_state == Sideways:
    quote L1–L5 uniformly around fair_value + skew
  elif market_state == Spike:
    quote L0–L3 wide + thin (near) + skew
    quote L5–L9 deep + thick (catch) + skew
  elif market_state == UnderAttack:
    PULL all L0–L3 bids
    quote L0–L3 asks only (elevated, reduced size)
    quote L5–L9 deep bids (FAR below fair_value, thick size) + skew

  for each new fill:
    check toxicity
    if toxic: pause side N seconds
```

---

## 8. Risk Invariants (Non-Negotiable)

1. **Normal/Spike: Fair value uses micro-price + OFI — never raw mid-price.**
2. **Under Attack: Fair value uses `max(vwap_3m, avg_sell_price_1h)` — never micro-price (it's compromised).**
3. **In Spike state, L0–L3 sizes never exceed `spike_near_size_ratio`.**
4. **In Under Attack state, L0–L3 bids are PULLED entirely — zero near-touch bid exposure.**
5. **L5–L9 orders exist in Spike and Under Attack states — bot never fully disappears from the deep book.**
6. **Inventory beyond `emergency_cap` → immediate one-sided cancel (overrides all states).**
7. **Toxicity pause is side-specific — opposite side always keeps quoting.**
8. **State transitions use hysteresis — no thrashing between states.**
9. **All monetary values use `decimal.Decimal` — never `float64`.**
10. **SLA KPI misses during Under Attack state are acceptable — capital preservation overrides compliance.**