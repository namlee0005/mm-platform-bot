package engine

import (
	"testing"
)

func TestSimpleConfig_ToEngineConfig(t *testing.T) {
	sc := &SimpleConfig{
		Symbol:              "BTCUSDT",
		BaseAsset:           "BTC",
		QuoteAsset:          "USDT",
		SpreadMinBps:        50,  // 0.5%
		SpreadMaxBps:        100, // 1%
		NumLevels:           20,
		TargetDepthNotional: 1000,
		TargetRatio:         0.5,
	}

	cfg := sc.ToEngineConfig()

	// Verify basic info
	if cfg.Symbol != "BTCUSDT" {
		t.Errorf("Symbol = %s, want BTCUSDT", cfg.Symbol)
	}

	// Verify spread
	if cfg.Spread.BaseSpreadBps != 75 { // (50+100)/2
		t.Errorf("BaseSpreadBps = %.1f, want 75", cfg.Spread.BaseSpreadBps)
	}

	// Verify depth levels
	if cfg.Depth.NumLevels != 20 {
		t.Errorf("NumLevels = %d, want 20", cfg.Depth.NumLevels)
	}

	// Verify offsets are generated and increasing
	if len(cfg.Depth.OffsetsBps) != 20 {
		t.Errorf("OffsetsBps len = %d, want 20", len(cfg.Depth.OffsetsBps))
	}

	for i := 1; i < len(cfg.Depth.OffsetsBps); i++ {
		if cfg.Depth.OffsetsBps[i] <= cfg.Depth.OffsetsBps[i-1] {
			t.Errorf("OffsetsBps not increasing at index %d: %v", i, cfg.Depth.OffsetsBps)
		}
	}

	// Verify sizes are generated
	if len(cfg.Depth.SizeMult) != 20 {
		t.Errorf("SizeMult len = %d, want 20", len(cfg.Depth.SizeMult))
	}

	// All sizes should be positive
	for i, size := range cfg.Depth.SizeMult {
		if size <= 0 {
			t.Errorf("SizeMult[%d] = %.2f, want > 0", i, size)
		}
	}
}

func TestDefaultSimpleConfig(t *testing.T) {
	sc := DefaultSimpleConfig("ETHUSDT", "ETH", "USDT")

	if sc.Symbol != "ETHUSDT" {
		t.Errorf("Symbol = %s, want ETHUSDT", sc.Symbol)
	}
	if sc.SpreadMinBps != 50 {
		t.Errorf("SpreadMinBps = %.1f, want 50", sc.SpreadMinBps)
	}
	if sc.NumLevels != 20 {
		t.Errorf("NumLevels = %d, want 20", sc.NumLevels)
	}
	if sc.TargetDepthNotional != 1000 {
		t.Errorf("TargetDepthNotional = %.1f, want 1000", sc.TargetDepthNotional)
	}
}

func TestAsymmetricLadder(t *testing.T) {
	ladder := NewAsymmetricLadder(10, 50, 100)

	// Verify both sides have correct length
	if len(ladder.BidOffsets) != 10 {
		t.Errorf("BidOffsets len = %d, want 10", len(ladder.BidOffsets))
	}
	if len(ladder.AskOffsets) != 10 {
		t.Errorf("AskOffsets len = %d, want 10", len(ladder.AskOffsets))
	}

	// Verify asymmetry (bid and ask should differ)
	allSame := true
	for i := 0; i < len(ladder.BidOffsets); i++ {
		if ladder.BidOffsets[i] != ladder.AskOffsets[i] {
			allSame = false
			break
		}
	}
	if allSame {
		t.Error("BidOffsets and AskOffsets should be different (asymmetric)")
	}

	// Verify offsets are increasing on each side
	for i := 1; i < len(ladder.BidOffsets); i++ {
		if ladder.BidOffsets[i] <= ladder.BidOffsets[i-1] {
			t.Errorf("BidOffsets not increasing at %d", i)
		}
		if ladder.AskOffsets[i] <= ladder.AskOffsets[i-1] {
			t.Errorf("AskOffsets not increasing at %d", i)
		}
	}
}

func TestAsymmetricLadder_RegenerateSide(t *testing.T) {
	ladder := NewAsymmetricLadder(5, 50, 100)
	originalBid := make([]int, len(ladder.BidOffsets))
	copy(originalBid, ladder.BidOffsets)

	// Regenerate bid side
	ladder.RegenerateSide("BUY", 5, 50, 100)

	// Bid should likely be different (random)
	// Ask should remain unchanged
	if len(ladder.BidOffsets) != 5 {
		t.Errorf("BidOffsets len after regen = %d, want 5", len(ladder.BidOffsets))
	}
}

func TestGenerateRandomLadder_IncreasingOffsets(t *testing.T) {
	// Run multiple times to verify consistency
	for run := 0; run < 10; run++ {
		offsets, sizes := generateRandomLadder(15, 30, 80)

		if len(offsets) != 15 {
			t.Errorf("Run %d: offsets len = %d, want 15", run, len(offsets))
		}
		if len(sizes) != 15 {
			t.Errorf("Run %d: sizes len = %d, want 15", run, len(sizes))
		}

		// Offsets must be strictly increasing
		for i := 1; i < len(offsets); i++ {
			if offsets[i] <= offsets[i-1] {
				t.Errorf("Run %d: offsets not increasing at %d: %v", run, i, offsets)
			}
		}

		// All sizes must be positive
		for i, s := range sizes {
			if s <= 0 {
				t.Errorf("Run %d: size[%d] = %.2f, must be > 0", run, i, s)
			}
		}
	}
}
