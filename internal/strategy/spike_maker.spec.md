# SpikeMaker Strategy — Spec & Logic

## Overview

SpikeMaker is a two-sided market making strategy that generates bid/ask depth
within ±2% of a fair price. It adapts spread, size distribution, and inventory
skew in real-time based on volatility regime, order flow imbalance, and inventory
position.

**Files:**
- `spike_depth_engine.go` — Pure-function 12-step depth generation engine
- `spike_maker.go` — Strategy wrapper (vol tracking, toxicity fill response, converge)
- `spike_depth_engine_test.go` — Unit + visual tests

---

## Architecture

```
                      ┌─────────────────┐
  WebSocket ────────► │  BaseBot.tick() │
  (orderbook,         │                 │
   trades,            │  every 5s       │
   fills)             └────────┬────────┘
                               │
                               ▼
                    ┌──────────────────────┐
                    │  SpikeMaker.Tick()   │
                    │                      │
                    │  1. EWM vol update   │
                    │  2. Risk state       │
                    │  3. Compute OFI      │
                    │  4. Call engine ──────┼──► generateSpikeAdaptiveOrders()
                    │  5. Budget constraint │        │
                    │  6. Scoped converge  │        ▼
                    │                      │    SpikeDepthResult {
                    └──────────┬───────────┘      Mid, Microprice, FairPrice,
                               │                  SpikeScore, Orders[]
                               ▼                }
                    ┌──────────────────────┐
                    │  Exchange            │
                    │  cancel + create     │
                    └──────────────────────┘
```

---

## 12-Step Depth Engine

### Step 1: Mid & Microprice

```
mid = (best_bid + best_ask) / 2

microprice = (best_ask × bid_size + best_bid × ask_size) / (bid_size + ask_size)
```

**Why microprice?** Raw mid treats bid and ask equally. But if bid side has $1000
depth and ask side has $200, the "true" price is closer to the ask (less
resistance to upward move). Microprice captures this imbalance.

**Example:**
```
best_bid=0.007495  bid_size=$1000
best_ask=0.007505  ask_size=$200
mid       = 0.007500
microprice = 0.007503  ← skewed toward ask (bid support is stronger)
```

---

### Step 2: Fair Price

```
fair = mid
fair += 0.7 × (microprice - mid)      # orderbook imbalance signal
fair += 0.1 × OFI × mid × 0.001      # order flow direction (~1bps per unit)
fair += 0.05 × ret × mid              # recent momentum
```

**Why not use raw mid?** Raw mid is symmetric and ignores directional signals.
Fair price incorporates 3 signals:

1. **Microprice pull (0.7):** Dominant signal. If bid side is heavy, price is
   likely to go up → fair price shifts up. Weight 0.7 because orderbook
   imbalance is the most reliable short-term predictor on illiquid tokens.

2. **OFI (0.1):** Weaker signal. Measures net buying/selling pressure within 1%
   of mid. Scaled by `mid × 0.001` so 1 unit OFI ≈ 1bps shift. Lower weight
   because OFI can be noisy on thin books.

3. **Momentum (0.05):** Weakest signal. Recent return (log price change).
   Captures trend but prone to noise. Small weight to avoid overreacting.

**All orders are placed around fair_price, not raw mid.** This means when buy
pressure exists, the entire ladder shifts up — bids get more aggressive, asks
get harder to hit.

---

### Step 3: Spike Score

```
S = |ret| / max(sigma, 1e-9)     # capped at 10
```

**What it measures:** How many "standard deviations" the last price move was.
Think of it as a z-score for the most recent return.

| S | Regime | Meaning |
|---|--------|---------|
| 0-1.0 | Normal | Typical noise, tight spread |
| 1.0-1.5 | Elevated | Slightly unusual, spread starts widening |
| 1.5-3.0 | Moderate spike | Significant move, size shifts deeper |
| 3.0-5.0 | Spike | Large move, aggressive protection |
| 5.0+ | Extreme | Crisis, no-quote zone near fair |

**sigma** comes from EWM (Exponentially Weighted Moving) variance tracked in
`spike_maker.go`:
```
ewmVar = 0.94 × ewmVar + 0.06 × ret²    # half-life ~30 ticks ≈ 2.5 min
sigma = sqrt(ewmVar)
```

---

### Step 4: Spread Scaling

```
spread = 5bps × (1 + 2 × S^0.7)
```

**Why S^0.7?** Sublinear scaling — spread widens fast initially but flattens at
high S. Prevents spread from becoming absurdly wide during extreme spikes.

| S | Spread |
|---|--------|
| 0 | 5 bps |
| 1 | 15 bps |
| 2 | 21 bps |
| 4 | 32 bps |
| 8 | 47 bps |

This is the **minimum distance** of the innermost order from fair price.

---

### Step 5: Ladder Shape (Distances)

```
d_i = 0.02 × (i/N)^gamma    # i = 1..N
```

**gamma** controls how levels are distributed:

| gamma | Shape | When |
|-------|-------|------|
| 1.8 | Dense near mid, sparse at depth | Normal (S < 1.5) |
| 1.4 | More evenly spread | Moderate (1.5 ≤ S < 3) |
| 1.2 | Nearly uniform | Spike (S ≥ 3) |

**Why adaptive gamma?**
- Normal: you want most liquidity near the touch (captures spread). Inner levels
  are tightly packed, outer levels are sparse fill-or-kill insurance.
- Spike: you DON'T want liquidity near the touch (adverse selection risk). Flatten
  the distribution so depth is spread more evenly → less exposure near mid.

**Spike widening:**
```
d_i *= (1 + 0.25 × S)    # cap at 2% (maxDist)
```
Pushes ALL levels further from fair. At S=4, all distances double.

**Jitter:**
```
d_i *= (1 + uniform(-0.02, 0.02))    # ±2% random per level
```
Makes the ladder look organic, not like a bot with rigid spacing. Uses
deterministic seed (`int64(fairPrice × 1e8)`) so jitter is stable across ticks
at the same price → reduces unnecessary order churn.

**Price dedup:** After rounding to tick_size, capped outer levels may collapse to
the same price. Engine enforces minimum 2-tick gap between adjacent levels:
```
if bidPrices[i-1] - bidPrices[i] < 2×tickSize:
    bidPrices[i] = bidPrices[i-1] - 2×tickSize
```

---

### Step 6: Size Profile (Weight Distribution)

**Two regimes with smooth blend:**

```
blend = clamp((S - 1) / 2, 0, 1)    # 0 at S≤1, 1 at S≥3
w[i] = (1-blend) × wNormal + blend × wSpike
```

**Normal (blend=0):**
```
w[i] = exp(-30 × d_i)
```
Exponential decay — most size near mid, little at depth. This is optimal for
earning spread in calm markets: you want fills near the touch where the edge is.

```
L0 (5bps):   $650    ████████████████
L3 (36bps):  $596    ██████████████
L6 (100bps): $492    ████████████
L9 (185bps): $382    █████████
```

**Spike (blend=1):**
```
w[i] = d_i^1.0 × exp(-0.5 × i)
```
Power law × inner decay — size peaks in middle levels, NOT at the touch. This
protects against adverse selection: the inner orders that get filled first are
small, the bulk of your liquidity is deeper and safer.

```
L0 (32bps):  $340    ████████
L2 (87bps):  $341    ████████
L4 (164bps): $236    ██████
L9 (193bps): $ 21    █
```

**Smooth blend** prevents order book from "jumping" when S crosses 1.5. At S=2,
the profile is a 50/50 mix of both regimes.

---

### Step 7: Inventory Skew

```
inv = q/Q                              # normalized inventory [-1, 1]
skew = tanh(1.5 × inv)                # nonlinear, saturates at ±1

bid_size[i] = base × max(0.2, 1 - 0.8 × skew)
ask_size[i] = base × max(0.2, 1 + 0.8 × skew)
```

**Why tanh?** Linear skew overreacts at extremes. tanh saturates:
- inv=0.3 → skew=0.41 (moderate response)
- inv=0.6 → skew=0.72 (strong response)
- inv=1.0 → skew=0.91 (near-maximum, but never 1.0)

This means even at maximum inventory, you still keep 20% of size on the heavy
side (never fully one-sided), maintaining some market presence.

**Example (long 50%, skew=0.64):**
```
bid_size = base × max(0.2, 1 - 0.8×0.64) = base × 0.49   (halved)
ask_size = base × max(0.2, 1 + 0.8×0.64) = base × 1.51   (50% more)
```
Result: bid=$2654, ask=$8137 — 3:1 sell bias.

---

### Step 8: OFI Directional Tilt

```
d_bid[i] = d[i] × (1 + 0.1 × OFI)
d_ask[i] = d[i] × (1 - 0.1 × OFI)
```

**Convention:** Positive OFI = more bid depth = buy support.

**Strategy: Trade AGAINST the flow.** When bid side is heavy:
- Bids wider (don't compete with the crowd on a supported side)
- Asks tighter (sell into the buy pressure)

**Why not trade WITH the flow?** On illiquid tokens, heavy bid depth usually
means support (market makers, projects), not informed flow. Informed traders
trade on the ASK side (market buys). So heavy bids = safe to sell into.

This is different from HFT on liquid markets where OFI often predicts direction.

---

### Step 9: Price Generation

```
rawBid = fair × (1 - d_bid[i])
rawAsk = fair × (1 + d_ask[i])

# Inventory price shift
bidPrice = fair - (fair - rawBid) × (1 + 0.3 × skew)
askPrice = fair + (rawAsk - fair) × (1 - 0.3 × skew)
```

**When long (skew > 0):**
- Bid distance multiplied by 1.3 → bids pushed FURTHER (discourage buying)
- Ask distance multiplied by 0.7 → asks pulled CLOSER (encourage selling)

This works WITH inventory size skew (Step 7) to create a double effect:
smaller bids that are also further away, larger asks that are also closer.

---

### Step 10: Queue Awareness

```
if queue_position > 0.7:    # back of queue
    improve price by 1-2 ticks (top 2 levels only)
if queue_position < 0.3:    # front of queue
    keep order (preserve priority)
```

**Why?** On exchanges with price-time priority, being at the front of the queue
at a price level means you get filled first. Canceling and reposting loses that
priority. Only improve price if you're stuck at the back.

Currently optional — `QueuePositionRatio=0` means this step is skipped.

---

### Step 11: Extreme Protection (S ≥ 5)

Three defenses:

1. **No-quote zone:** Kill all orders within ±10bps of fair price. During extreme
   spikes, anyone hitting these orders is likely informed → pure adverse selection.

2. **Top-level reduction (gradual):**
   ```
   reduction = 0.1 + 0.9 × (1 - |skew|)
   ```
   - skew=0 (balanced) → no extra reduction
   - skew=0.5 → top 2 levels keep 55%
   - skew=1.0 → top 2 levels keep 10%

   This is gradual, not binary. A tiny skew of 0.001 doesn't trigger 90% cut.

3. **Combined with size throttle:** At S=5, throttle = 33% → total depth is
   already much smaller. Extreme protection adds targeted cuts on top.

---

### Step 12: Size Throttling

```
multiplier = 1 / (1 + 0.5 × S)     # floor at 20%
```

**Hyperbolic, not exponential.** Old version used `exp(-0.8×S)` which was too
aggressive (4% at S=4, violating exchange minimum depth). New curve is gentler:

| S | Old (exp) | New (hyperbolic) | At $5800 target |
|---|-----------|-----------------|-----------------|
| 0 | 100% | 100% | $5800/side |
| 1 | 45% | 67% | $3886 |
| 2 | 20% | 50% | $2900 |
| 3 | 9% | 40% | $2320 |
| 4 | 4% | 33% | $1914 |
| 8 | 0.03% | 20% | $1160 |

At S=3, depth is $2320/side — still above typical exchange minimum of $1000-2000.

---

## Toxicity-Based Fill Response

When an order gets filled, the strategy determines **how much of the book to
rebuild** based on fill toxicity.

### Toxicity Scoring (in OnFill)

Three signals, take the worst:

| Signal | Moderate (1) | High (2) |
|--------|-------------|----------|
| Spike score S | ≥ 1.5 | ≥ 3.0 |
| Adverse price move | > 0.2% | > 0.5% |
| Same-side fills in 30s | ≥ 2 | ≥ 3 |

**Adverse move:** If you bought and the price immediately dropped, you were
adversely selected. Larger drop = more toxic.

**Repeated same-side fills:** 3 BUY fills in 30 seconds = someone is dumping
into your bids systematically.

### Rebuild Scope

| Toxicity | What to rebuild | What to keep |
|----------|----------------|--------------|
| 0 (normal) | Top 2 levels, fill side only | All deeper + opposite side |
| 1 (moderate) | Top 3 levels, fill side only | All deeper + opposite side |
| 2 (high) | Everything | Nothing |

**Why scoped rebuild?** Exchange queue priority. If your L5 bid has been sitting
for 10 minutes, it's at the front of the queue at that price — first to get
filled. Canceling it just because L0 was filled wastes that accumulated priority.

**Scope expires after 10 seconds** → reverts to full converge.

### Converge Algorithm

Standard converge diffs desired orders vs live orders:
- Match by price proximity (25bps tolerance)
- Keep orders that are close enough
- Cancel orphans, create new ones

With rebuild scope, converge is **restricted:**
- Orders outside scope → auto-keep (never cancelled)
- Only orders within scope are compared/replaced
- Opposite side → completely untouched

---

## Risk Modes

```
NORMAL    → sizeMult = 1.0
WARNING   → sizeMult = 0.8-1.0  (drawdown > 3%)
RECOVERY  → sizeMult = 0.3-1.0  (drawdown > 2%)
PAUSED    → cancel all orders    (drawdown > 5%)
```

sizeMult scales ALL order sizes uniformly, independent of the spike engine.

---

## Config (from SimpleConfig in MongoDB)

| Field | Default | Description |
|-------|---------|-------------|
| `num_levels` | 10 | Orders per side |
| `target_depth_notional` | 5800 | $ depth per side |
| `depth_bps` | 200 | Max distance from mid (200 = 2%) |
| `spread_min_bps` | 40 | Min spread (used by simple_ladder, not spike engine) |
| `init_base` | auto | Initial base balance (for inventory ratio) |
| `init_quote` | auto | Initial quote balance |
| `drawdown_limit_pct` | 0.05 | 5% drawdown → PAUSED |

Spike engine constants are hardcoded (not configurable per bot) to prevent
misconfiguration. If a specific token needs tuning, modify the constants in
`spike_depth_engine.go`.
