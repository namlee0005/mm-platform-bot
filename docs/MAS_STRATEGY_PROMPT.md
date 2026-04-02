# MAS.go Strategy: Capital Preservation Market Maker (Prompt for Claude)

You are an expert High-Frequency Trading (HFT) Golang Engineer. Your task is to implement the `MAS.go` market-making strategy based on the exact specifications below. The primary goal of this strategy is **Capital Preservation (Loss Minimization)** on low-liquidity pairs, while satisfying exchange liquidity KPIs (e.g., $X depth within ±2%).

Please implement the strategy across these files:
- `internal/strategy/mas_types.go` (Config and State structs)
- `internal/strategy/mas_fair_value.go` (EWMA Mid-Price, VWAP, and OFI logic)
- `internal/strategy/mas_volatility.go` (EWMA Realized Volatility & Regime tracking)
- `internal/strategy/mas_inventory.go` (Inventory Skew & Emergency Caps)
- `internal/strategy/mas_toxicity.go` (Toxicity Pause logic)
- `internal/strategy/mas_strategy.go` (The main `core.Strategy` implementation)
- `internal/engine/executor.go` (Refactor to State-Based Conflation & Cancel-First Priority)

---

## 1. Fair Value Engine (Anti-Spoofing)
Do not use raw Mid-Price or Micro-Price, as they are highly vulnerable to spoofing on low-liquidity books.

**Normal State (Volatility S < 2.0):**
- Base price = EWMA of Mid-Price (e.g., 60s halflife).
- `FairValue = EWMA_MidPrice + (OFI_Normalized * OFI_Alpha * TickSize)`
- OFI (Order Flow Imbalance) must be calculated using `decimal.Decimal` to prevent float accumulation errors.

**Under Attack State (Spike / S >= 2.0):**
- When under attack, discard EWMA Mid-Price and OFI completely.
- If market is PUMPING (price spiking up): `FairValue = MAX(3m_VWAP, 1h_avg_sell_price)`
  - *Effect:* Keeps Asks high to sell expensively to FOMO, pushes Bids deep down so we don't buy the top.
- If market is DUMPING (price crashing down): `FairValue = MIN(3m_VWAP, 1h_avg_buy_price)`
  - *Effect:* Pushes Bids deep down to avoid catching the falling knife too early, keeps Asks low to get out if needed.

## 2. Volatility Regime & Elastic Grid
Track realized volatility using an EWMA variance tracker (15-20s window). Calculate a Spike Score `S`.

**Sideways Regime (Low Vol):**
- Normal base spread.
- Uniform size distribution across L0 to L8.

**Spike Regime (High Vol):**
- **L0-L3 (Near Touch):** Widen spread massively (e.g., `base_spread * 4.0`). Shrink sizes to a fraction (e.g., 20% of base size) to act as bait/dummy quotes.
- **L4:** Empty buffer zone. No quotes.
- **L5-L8 (Deep Catch):** Thick sizes (e.g., 150% of base size). Placed deep in the book to catch over-extended flash crashes or pumps.

## 3. Exchange Compliance (The "Fat-Tail" Anchor)
Exchanges require $X of liquidity within ±2% of the mid-price to earn rebates.
- **L9 Quote:** Regardless of the regime, force the L9 quote (both Bid and Ask) to be anchored at EXACTLY `1.95%` away from the actual exchange Mid-Price.
- Place the vast majority of the required compliant size in this L9 bucket.
- *Effect:* Meets the exchange KPI safely because retail takers rarely eat through 1.95% of the book in normal conditions.

## 4. Inventory Control (Skew & Hard Caps)
- **Inventory Ratio** = `NetPosition / MaxInventory` ∈ [-1.0, 1.0].
- **Skew:** `skew_ticks = InventoryRatio * MaxSkewTicks`. Shift both Bids and Asks down if Long (to sell easier, buy harder).
- **Emergency Hard Cap:** If `abs(NetPosition) >= EmergencyCap` (e.g., holding 1200 tokens when max is 1000).
  - *Action:* If Long, CANCEL ALL BIDS IMMEDIATELY. Only quote Asks. DO NOT average down (No DCA).

## 5. Toxicity Pause (Adverse Selection Defense)
- Track every fill. If the market price moves adversely by `ToxicMoveTicks` within `ToxicDetectionWindow` (e.g., 8 seconds) after a fill, mark it as a "Toxic Fill".
- Use a tiered penalty system based on rolling 5-minute toxic fill counts:
  - 2-3 toxic fills -> Pause the affected side for 15s.
  - 4-5 toxic fills -> Pause for 45s.
  - 6+ toxic fills -> Pause for 120s.
- *Action:* When a side is paused, output 0 desired orders for that side (except the L9 Compliance anchor, which must remain to protect SLAs).

## 6. Execution Engine (HFT Conflation)
Rewrite `internal/engine/executor.go`:
- **State-Based Conflation:** Do not use `time.After(2s)` or channels that drop ticks. Use a `sync.Mutex` protected `nextDesiredState`. A background goroutine continuously polls this state, computes diffs, and executes. Intermediate ticks are safely overwritten (conflated) before the executor picks them up.
- **Cancel-First Priority:** When executing diffs, ALWAYS dispatch `Cancel` requests concurrently (via WaitGroup) and wait for them to finish BEFORE dispatching `Place` or `Amend` requests. This ensures we free up capital and escape bad positions before trying to quote again.