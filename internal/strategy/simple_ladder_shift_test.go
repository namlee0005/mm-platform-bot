package strategy

import (
	"context"
	"math"
	"testing"

	"mm-platform-engine/internal/config"
	"mm-platform-engine/internal/core"
)

// newTestStrategy creates a SimpleLadderStrategy with realistic defaults for testing.
// mid=0.008, tickSize=0.000001, 5 levels, spread 40-100bps, depth 200bps, depth=$100/side
func newTestStrategy(numLevels int) *SimpleLadderStrategy {
	cfg := &SimpleLadderConfig{
		SimpleConfig: &config.SimpleConfig{
			SpreadMinBps:        40,
			SpreadMaxBps:        100,
			NumLevels:           numLevels,
			TargetDepthNotional: 100, // $100 per side
			DepthBps:            200,
			LevelGapTicksMax:    3,
		},
	}
	s := NewSimpleLadderStrategy(cfg)
	s.tickSize = 0.000001
	s.stepSize = 0.01
	s.minNotional = 1.0
	s.maxOrderQty = 1_000_000
	return s
}

// makeLiveOrder is a helper to create a LiveOrder at a given price/side.
func makeLiveOrder(id, side string, price float64) core.LiveOrder {
	return core.LiveOrder{
		OrderID: id,
		Side:    side,
		Price:   price,
		Qty:     100,
	}
}

// makeBalance returns a balanced BalanceState for mid price.
func makeBalance(mid float64) *core.BalanceState {
	// $500 quote free + base equivalent of $500 → balanced 50/50
	return &core.BalanceState{
		QuoteFree: 500,
		BaseFree:  500 / mid,
	}
}

// assertAmend verifies output is TickActionAmend with zero cancels and N adds.
func assertAmend(t *testing.T, out *core.TickOutput, wantAdds int) {
	t.Helper()
	if out == nil {
		t.Fatal("expected non-nil output")
	}
	if out.Action != core.TickActionAmend {
		t.Fatalf("expected AMEND, got %s (reason: %s)", out.Action, out.Reason)
	}
	if len(out.OrdersToCancel) != 0 {
		t.Errorf("expected 0 cancels, got %d", len(out.OrdersToCancel))
	}
	if len(out.OrdersToAdd) != wantAdds {
		t.Errorf("expected %d orders to add, got %d", wantAdds, len(out.OrdersToAdd))
	}
}

// assertSide verifies all added orders are on the expected side.
func assertSide(t *testing.T, orders []core.DesiredOrder, side string) {
	t.Helper()
	for _, o := range orders {
		if o.Side != side {
			t.Errorf("expected side %s, got %s (price=%.6f)", side, o.Side, o.Price)
		}
	}
}

// assertBuyPrices checks all added BUY orders are below mid and above depth limit.
// Note: spread is randomized [spreadMin..spreadMax] so we only assert the outer bound (depthBps).
func assertBuyPrices(t *testing.T, orders []core.DesiredOrder, mid, depthBps float64) {
	t.Helper()
	minPrice := mid * (1.0 - depthBps/10000.0)
	for _, o := range orders {
		if o.Side != "BUY" {
			continue
		}
		if o.Price >= mid {
			t.Errorf("BUY price %.8f must be < mid %.8f", o.Price, mid)
		}
		if o.Price < minPrice {
			t.Errorf("BUY price %.8f below depth limit %.8f (depth=%.0fbps)", o.Price, minPrice, depthBps)
		}
	}
}

// assertSellPrices checks all added SELL orders are above mid and below depth limit.
func assertSellPrices(t *testing.T, orders []core.DesiredOrder, mid, depthBps float64) {
	t.Helper()
	maxPrice := mid * (1.0 + depthBps/10000.0)
	for _, o := range orders {
		if o.Side != "SELL" {
			continue
		}
		if o.Price <= mid {
			t.Errorf("SELL price %.8f must be > mid %.8f", o.Price, mid)
		}
		if o.Price > maxPrice {
			t.Errorf("SELL price %.8f above depth limit %.8f (depth=%.0fbps)", o.Price, maxPrice, depthBps)
		}
	}
}

// runTick is a convenience wrapper that inits strategy state and calls Tick.
func runTick(t *testing.T, s *SimpleLadderStrategy, mid float64, liveOrders []core.LiveOrder) *core.TickOutput {
	t.Helper()

	// peakNAV=0 → strategy initializes peak from current NAV → drawdown=0 → NORMAL mode
	s.mu.Lock()
	if s.peakNAV == 0 {
		s.currentMode = ModeNormal
	}
	s.mu.Unlock()

	snap := &core.Snapshot{
		BestBid:     mid * 0.9999,
		BestAsk:     mid * 1.0001,
		Mid:         mid,
		TickSize:    s.tickSize,
		StepSize:    s.stepSize,
		MinNotional: s.minNotional,
	}
	balance := makeBalance(mid)

	input := &core.TickInput{
		Snapshot:   snap,
		Balance:    balance,
		LiveOrders: liveOrders,
	}

	// Print input state
	bidCount, askCount := 0, 0
	for _, o := range liveOrders {
		if o.Side == "BUY" {
			bidCount++
		} else {
			askCount++
		}
	}
	t.Logf("  INPUT: mid=%.6f live=%d (bid=%d ask=%d) expected=%d/side",
		mid, len(liveOrders), bidCount, askCount, s.cfg.NumLevels)

	out, err := s.Tick(context.Background(), input)
	if err != nil {
		t.Fatalf("Tick error: %v", err)
	}

	// Print output state
	t.Logf("  OUTPUT: action=%-12s reason=%s | cancel=%d add=%d",
		out.Action, out.Reason, len(out.OrdersToCancel), len(out.OrdersToAdd))
	for _, o := range out.OrdersToAdd {
		t.Logf("    + %s price=%.6f qty=%.2f tag=%s", o.Side, o.Price, o.Qty, o.Tag)
	}
	for _, id := range out.OrdersToCancel {
		t.Logf("    - cancel %s", id)
	}

	return out
}

// =============================================================================
// TC-01: Full ladder intact → no shift, KEEP or normal amend (not shift path)
// =============================================================================
func TestShiftLadder_TC01_FullLadder_NoShift(t *testing.T) {
	mid := 0.008
	s := newTestStrategy(5)

	// 5 BUY + 5 SELL = full ladder (expectedPerSide=5)
	live := []core.LiveOrder{
		makeLiveOrder("b1", "BUY", mid*0.9960),
		makeLiveOrder("b2", "BUY", mid*0.9950),
		makeLiveOrder("b3", "BUY", mid*0.9940),
		makeLiveOrder("b4", "BUY", mid*0.9930),
		makeLiveOrder("b5", "BUY", mid*0.9920),
		makeLiveOrder("s1", "SELL", mid*1.0040),
		makeLiveOrder("s2", "SELL", mid*1.0050),
		makeLiveOrder("s3", "SELL", mid*1.0060),
		makeLiveOrder("s4", "SELL", mid*1.0070),
		makeLiveOrder("s5", "SELL", mid*1.0080),
	}

	out := runTick(t, s, mid, live)

	// Must NOT be shift path → should be KEEP or normal AMEND (never shiftLadder)
	if out.Action == core.TickActionAmend && len(out.OrdersToCancel) == 0 && len(out.OrdersToAdd) > 0 {
		// Only fail if reason contains "shift_ladder"
		if out.Reason == "shift_ladder: bid=5/5 ask=5/5" {
			t.Error("TC-01 FAIL: should not enter shiftLadder when ladder is full")
		}
	}
	t.Logf("TC-01 OK: action=%s reason=%s", out.Action, out.Reason)
}

// =============================================================================
// TC-02: 1 BUY filled → shift adds 1 inner BUY
// =============================================================================
func TestShiftLadder_TC02_OneBuyFilled(t *testing.T) {
	mid := 0.008
	s := newTestStrategy(5)

	// Only 4 BUYs remain (1 fill happened)
	live := []core.LiveOrder{
		makeLiveOrder("b2", "BUY", mid*0.9950),
		makeLiveOrder("b3", "BUY", mid*0.9940),
		makeLiveOrder("b4", "BUY", mid*0.9930),
		makeLiveOrder("b5", "BUY", mid*0.9920),
		makeLiveOrder("s1", "SELL", mid*1.0040),
		makeLiveOrder("s2", "SELL", mid*1.0050),
		makeLiveOrder("s3", "SELL", mid*1.0060),
		makeLiveOrder("s4", "SELL", mid*1.0070),
		makeLiveOrder("s5", "SELL", mid*1.0080),
	}

	out := runTick(t, s, mid, live)

	assertAmend(t, out, 1)
	assertSide(t, out.OrdersToAdd, "BUY")
	assertBuyPrices(t, out.OrdersToAdd, mid, s.cfg.DepthBps)
	t.Logf("TC-02 OK: added %v", out.OrdersToAdd[0])
}

// =============================================================================
// TC-03: 1 SELL filled → shift adds 1 inner SELL
// =============================================================================
func TestShiftLadder_TC03_OneSellFilled(t *testing.T) {
	mid := 0.008
	s := newTestStrategy(5)

	live := []core.LiveOrder{
		makeLiveOrder("b1", "BUY", mid*0.9960),
		makeLiveOrder("b2", "BUY", mid*0.9950),
		makeLiveOrder("b3", "BUY", mid*0.9940),
		makeLiveOrder("b4", "BUY", mid*0.9930),
		makeLiveOrder("b5", "BUY", mid*0.9920),
		// s1 (innermost sell) was filled
		makeLiveOrder("s2", "SELL", mid*1.0050),
		makeLiveOrder("s3", "SELL", mid*1.0060),
		makeLiveOrder("s4", "SELL", mid*1.0070),
		makeLiveOrder("s5", "SELL", mid*1.0080),
	}

	out := runTick(t, s, mid, live)

	assertAmend(t, out, 1)
	assertSide(t, out.OrdersToAdd, "SELL")
	assertSellPrices(t, out.OrdersToAdd, mid, s.cfg.DepthBps)
	t.Logf("TC-03 OK: added %v", out.OrdersToAdd[0])
}

// =============================================================================
// TC-04: Both sides have 1 fill → shift adds 1 BUY + 1 SELL
// =============================================================================
func TestShiftLadder_TC04_BothSidesFilled(t *testing.T) {
	mid := 0.008
	s := newTestStrategy(5)

	live := []core.LiveOrder{
		// 4 BUYs (inner fill)
		makeLiveOrder("b2", "BUY", mid*0.9950),
		makeLiveOrder("b3", "BUY", mid*0.9940),
		makeLiveOrder("b4", "BUY", mid*0.9930),
		makeLiveOrder("b5", "BUY", mid*0.9920),
		// 4 SELLs (inner fill)
		makeLiveOrder("s2", "SELL", mid*1.0050),
		makeLiveOrder("s3", "SELL", mid*1.0060),
		makeLiveOrder("s4", "SELL", mid*1.0070),
		makeLiveOrder("s5", "SELL", mid*1.0080),
	}

	out := runTick(t, s, mid, live)

	assertAmend(t, out, 2) // 1 BUY + 1 SELL
	buyCount, sellCount := 0, 0
	for _, o := range out.OrdersToAdd {
		if o.Side == "BUY" {
			buyCount++
		} else {
			sellCount++
		}
	}
	if buyCount != 1 || sellCount != 1 {
		t.Errorf("TC-04: expected 1 BUY + 1 SELL, got %d BUY %d SELL", buyCount, sellCount)
	}
	t.Logf("TC-04 OK: added BUY+SELL")
}

// =============================================================================
// TC-05: 2 BUY fills → L0 và L1 fill (orderbook fill từ best price), L2-L4 còn lại
// =============================================================================
func TestShiftLadder_TC05_TwoBuyFills(t *testing.T) {
	mid := 0.008
	s := newTestStrategy(5)

	live := []core.LiveOrder{
		// L0 và L1 đã fill, còn lại L2, L3, L4
		makeLiveOrder("b3", "BUY", mid*0.9940), // L2
		makeLiveOrder("b4", "BUY", mid*0.9930), // L3
		makeLiveOrder("b5", "BUY", mid*0.9920), // L4
		makeLiveOrder("s1", "SELL", mid*1.0040),
		makeLiveOrder("s2", "SELL", mid*1.0050),
		makeLiveOrder("s3", "SELL", mid*1.0060),
		makeLiveOrder("s4", "SELL", mid*1.0070),
		makeLiveOrder("s5", "SELL", mid*1.0080),
	}

	out := runTick(t, s, mid, live)

	assertAmend(t, out, 2)
	assertSide(t, out.OrdersToAdd, "BUY")
	assertBuyPrices(t, out.OrdersToAdd, mid, s.cfg.DepthBps)
	t.Logf("TC-05 OK: added 2 BUYs %v", out.OrdersToAdd)
}

// =============================================================================
// TC-06: All 5 BUY filled (L0→L4 lần lượt) → shift rebuild đủ 5 BUY từ inner ra outer
// =============================================================================
func TestShiftLadder_TC06_AllBuysFilled(t *testing.T) {
	mid := 0.008
	s := newTestStrategy(5)

	live := []core.LiveOrder{
		// No BUYs at all
		makeLiveOrder("s1", "SELL", mid*1.0040),
		makeLiveOrder("s2", "SELL", mid*1.0050),
		makeLiveOrder("s3", "SELL", mid*1.0060),
		makeLiveOrder("s4", "SELL", mid*1.0070),
		makeLiveOrder("s5", "SELL", mid*1.0080),
	}

	out := runTick(t, s, mid, live)

	if out.Action != core.TickActionAmend && out.Action != core.TickActionReplace {
		t.Fatalf("TC-06: expected AMEND or REPLACE, got %s", out.Action)
	}
	if out.Action == core.TickActionAmend {
		if len(out.OrdersToAdd) != 5 {
			t.Errorf("TC-06: expected 5 BUYs added, got %d", len(out.OrdersToAdd))
		}
		for _, o := range out.OrdersToAdd {
			if o.Side != "BUY" {
				t.Errorf("TC-06: expected all BUY, got %s", o.Side)
			}
			if o.Price >= mid {
				t.Errorf("TC-06: BUY price %.8f must be < mid %.8f", o.Price, mid)
			}
		}
	}
	t.Logf("TC-06 OK: action=%s adds=%d", out.Action, len(out.OrdersToAdd))
}

// =============================================================================
// TC-07: No duplicate price — new order must not clash with existing BUY price
// =============================================================================
func TestShiftLadder_TC07_NoDuplicatePrice(t *testing.T) {
	mid := 0.008
	s := newTestStrategy(5)

	// 4 BUYs at specific prices
	live := []core.LiveOrder{
		makeLiveOrder("b2", "BUY", mid*0.9950),
		makeLiveOrder("b3", "BUY", mid*0.9940),
		makeLiveOrder("b4", "BUY", mid*0.9930),
		makeLiveOrder("b5", "BUY", mid*0.9920),
		makeLiveOrder("s1", "SELL", mid*1.0040),
		makeLiveOrder("s2", "SELL", mid*1.0050),
		makeLiveOrder("s3", "SELL", mid*1.0060),
		makeLiveOrder("s4", "SELL", mid*1.0070),
		makeLiveOrder("s5", "SELL", mid*1.0080),
	}

	out := runTick(t, s, mid, live)

	if out.Action == core.TickActionAmend {
		for _, newOrder := range out.OrdersToAdd {
			for _, existing := range live {
				if existing.Side == newOrder.Side {
					diff := math.Abs(newOrder.Price - existing.Price)
					if diff < s.tickSize {
						t.Errorf("TC-07: new order price %.8f too close to existing %.8f (diff=%.8f < tickSize=%.8f)",
							newOrder.Price, existing.Price, diff, s.tickSize)
					}
				}
			}
		}
	}
	t.Logf("TC-07 OK: no duplicate prices")
}

// =============================================================================
// TC-08: Recovery mode does not break shift ladder → still returns AMEND, not REPLACE
// =============================================================================
func TestShiftLadder_TC08_RecoveryMode_StillAmend(t *testing.T) {
	mid := 0.008
	s := newTestStrategy(5)

	live := []core.LiveOrder{
		makeLiveOrder("b2", "BUY", mid*0.9950),
		makeLiveOrder("b3", "BUY", mid*0.9940),
		makeLiveOrder("b4", "BUY", mid*0.9930),
		makeLiveOrder("b5", "BUY", mid*0.9920),
		makeLiveOrder("s1", "SELL", mid*1.0040),
		makeLiveOrder("s2", "SELL", mid*1.0050),
		makeLiveOrder("s3", "SELL", mid*1.0060),
		makeLiveOrder("s4", "SELL", mid*1.0070),
		makeLiveOrder("s5", "SELL", mid*1.0080),
	}

	// Recovery mode: peakNAV slightly above current NAV to trigger drawdown > DrawdownReducePct=2%
	// current NAV ≈ $1000; peakNAV=1030 → drawdown ≈ 2.9%
	s.mu.Lock()
	s.currentMode = ModeRecovery
	s.peakNAV = 1030
	s.mu.Unlock()

	out := runTick(t, s, mid, live)

	// Key assertion: must still be AMEND (not REPLACE) even in recovery mode
	if out.Action != core.TickActionAmend {
		t.Fatalf("TC-08 FAIL: expected AMEND in recovery mode, got %s (reason: %s)", out.Action, out.Reason)
	}
	if len(out.OrdersToCancel) != 0 {
		t.Errorf("TC-08 FAIL: expected 0 cancels, got %d", len(out.OrdersToCancel))
	}
	// sizeMult < 1.0 in recovery → qty should not exceed pyramid outer max
	// outer level (L4 of 5) with factor=3: notional = 100 * 3/10 = 30, maxQty = 30 * 1.3 / 0.008 = 4875
	outerNotional := pyramidNotional(s.cfg.NumLevels-1, s.cfg.NumLevels, s.cfg.TargetDepthNotional, s.cfg.PyramidFactor)
	maxPossibleQty := outerNotional * sizeJitterMax / mid
	for _, o := range out.OrdersToAdd {
		if o.Qty > maxPossibleQty {
			t.Errorf("TC-08 FAIL: qty %.2f exceeds max possible %.2f in recovery mode", o.Qty, maxPossibleQty)
		}
	}
	t.Logf("TC-08 OK: AMEND in recovery, adds=%d, cancel=0", len(out.OrdersToAdd))
}

// =============================================================================
// TC-09: Price within valid range — all added orders must be inside [spreadMin, depthBps]
// =============================================================================
func TestShiftLadder_TC09_PriceRange(t *testing.T) {
	mid := 0.008
	s := newTestStrategy(5)

	live := []core.LiveOrder{
		makeLiveOrder("b2", "BUY", mid*0.9950),
		makeLiveOrder("b3", "BUY", mid*0.9940),
		makeLiveOrder("b4", "BUY", mid*0.9930),
		makeLiveOrder("b5", "BUY", mid*0.9920),
		makeLiveOrder("s1", "SELL", mid*1.0040),
		makeLiveOrder("s2", "SELL", mid*1.0050),
		makeLiveOrder("s3", "SELL", mid*1.0060),
		makeLiveOrder("s4", "SELL", mid*1.0070),
		makeLiveOrder("s5", "SELL", mid*1.0080),
	}

	out := runTick(t, s, mid, live)

	if out.Action != core.TickActionAmend {
		t.Skipf("TC-09: action=%s, skip price range check", out.Action)
	}

	// Valid range: prices must be strictly inside [mid ± depthBps] and on the correct side of mid.
	// Note: bid spread can be < spreadMinBps because the total spread is split by inventory ratio,
	// so we only assert the outer depth bound (not the inner spreadMin bound).
	depthBps := s.cfg.DepthBps // 200bps = 2%

	for _, o := range out.OrdersToAdd {
		if o.Side == "BUY" {
			minPrice := mid * (1.0 - depthBps/10000.0)
			if o.Price >= mid {
				t.Errorf("TC-09: BUY price %.8f must be < mid %.8f", o.Price, mid)
			}
			if o.Price < minPrice-s.tickSize {
				t.Errorf("TC-09: BUY price %.8f below depth limit %.8f", o.Price, minPrice)
			}
		} else {
			maxPrice := mid * (1.0 + depthBps/10000.0)
			if o.Price <= mid {
				t.Errorf("TC-09: SELL price %.8f must be > mid %.8f", o.Price, mid)
			}
			if o.Price > maxPrice+s.tickSize {
				t.Errorf("TC-09: SELL price %.8f above depth limit %.8f", o.Price, maxPrice)
			}
		}
	}
	t.Logf("TC-09 OK: all prices in valid range")
}

// =============================================================================
// TC-10: Minimum notional — no order below minNotional ($1)
// =============================================================================
func TestShiftLadder_TC10_MinNotional(t *testing.T) {
	mid := 0.008
	s := newTestStrategy(5)
	s.minNotional = 1.0

	live := []core.LiveOrder{
		makeLiveOrder("b2", "BUY", mid*0.9950),
		makeLiveOrder("b3", "BUY", mid*0.9940),
		makeLiveOrder("b4", "BUY", mid*0.9930),
		makeLiveOrder("b5", "BUY", mid*0.9920),
		makeLiveOrder("s1", "SELL", mid*1.0040),
		makeLiveOrder("s2", "SELL", mid*1.0050),
		makeLiveOrder("s3", "SELL", mid*1.0060),
		makeLiveOrder("s4", "SELL", mid*1.0070),
		makeLiveOrder("s5", "SELL", mid*1.0080),
	}

	out := runTick(t, s, mid, live)

	if out.Action == core.TickActionAmend {
		for _, o := range out.OrdersToAdd {
			notional := o.Price * o.Qty
			if notional < s.minNotional {
				t.Errorf("TC-10: order notional %.4f < minNotional %.4f (price=%.8f qty=%.4f)",
					notional, s.minNotional, o.Price, o.Qty)
			}
		}
		t.Logf("TC-10 OK: all orders above minNotional=%.2f", s.minNotional)
	}
}
