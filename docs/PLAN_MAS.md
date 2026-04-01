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

## 2. Pillar 1: Fair Value (Micro-Price + OFI)

### 2.1 Micro-Price

The naive mid-price `(best_bid + best_ask) / 2` ignores orderbook imbalance. A better fair value estimate is the **volume-weighted mid**:

```
micro_price = (best_ask × bid_qty + best_bid × ask_qty) / (bid_qty + ask_qty)
```

This pulls the fair value toward the side with more liquidity — if there are 10x more bids than asks at the top of book, the true fair value is closer to the ask.

### 2.2 OFI Adjustment

Order Flow Imbalance (OFI) measures net directional pressure from recent trades:

```
ofi = Σ (buy_volume - sell_volume) over last N seconds
ofi_normalized = ofi / max_observed_ofi   ∈ [-1.0, +1.0]
```

The final fair value used for quoting:

```
fair_value = micro_price + (ofi_normalized × ofi_alpha × tick_size)
```

- `ofi_alpha` defaults to `3.0` ticks — calibrate per instrument.
- Positive OFI → fair value shifts up → bid shallower, ask tighter.
- Negative OFI → fair value shifts down → bid tighter, ask shallower.

**Config:**
```yaml
mas:
  fair_value:
    ofi_window: 10s
    ofi_alpha: 3.0
    ofi_decay: 0.85
```

---

## 3. Pillar 2: Volatility Regime

Two regimes: **Sideways** and **Spike**.

### 3.1 Regime Detection

```
realized_vol = stddev(mid_price_returns, window=60s) × sqrt(annualization_factor)
```

| Condition | Regime |
|-----------|--------|
| `realized_vol < vol_threshold` | Sideways |
| `realized_vol >= vol_threshold` | Spike |

`vol_threshold` defaults to `0.0025` (0.25% per minute). Hysteresis: enter Spike at 0.0025, exit at 0.0015 — prevents thrashing.

### 3.2 Sideways Quoting (Normal)

Standard symmetric quoting around `fair_value`. Spread = `base_spread`. Sizes uniform across L1–L5.

```
bid_price[L] = fair_value - base_spread/2 - (L × tick_size × level_step)
ask_price[L] = fair_value + base_spread/2 + (L × tick_size × level_step)
size[L] = base_size
```

### 3.3 Spike Quoting (Defensive)

In a Spike, informed traders are active. The goal: **get out of their way at L0–L3, but catch over-extended moves at L5–L9**.

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

**Config:**
```yaml
mas:
  volatility:
    vol_threshold: 0.0025
    vol_exit_threshold: 0.0015
    vol_window: 60s
    spike_near_spread_multiplier: 4.0
    spike_near_size_ratio: 0.20
    spike_deep_offset_ticks: 15
    spike_deep_size_ratio: 1.50
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
    Regime          VolatilityRegime
    FairValue       decimal.Decimal
    OFINormalized   float64
    NetPosition     decimal.Decimal
    InventoryRatio  float64
    ToxicPausedSide map[Side]time.Time
    ToxicFillCount  map[Side]int
    RealizedVol     float64
}

type VolatilityRegime int

const (
    RegimeSideways VolatilityRegime = 0
    RegimeSpike    VolatilityRegime = 1
)
```

---

## 7. Quote Generation Flow

```
every tick:
  1. Compute micro_price from top-of-book
  2. Compute ofi_normalized from recent trade tape
  3. fair_value = micro_price + ofi_adjustment
  4. realized_vol = stddev(recent returns)
  5. regime = classify(realized_vol)
  6. inventory_ratio = net_position / max_inventory
  7. skew_ticks = inventory_ratio × max_skew_ticks

  if regime == Sideways:
    quote L1–L5 uniformly around fair_value + skew
  elif regime == Spike:
    quote L0–L3 wide + thin (near) + skew
    quote L5–L9 deep + thick (catch) + skew

  for each new fill:
    check toxicity
    if toxic: pause side N seconds
```

---

## 8. Risk Invariants (Non-Negotiable)

1. **Fair value always uses micro-price + OFI — never raw mid-price.**
2. **In Spike regime, L0–L3 sizes never exceed `spike_near_size_ratio`.**
3. **L5–L9 orders exist in Spike regime — bot never fully disappears.**
4. **Inventory beyond `emergency_cap` → immediate one-sided cancel.**
5. **Toxicity pause is side-specific — opposite side always keeps quoting.**
6. **All monetary values use `decimal.Decimal` — never `float64`.**