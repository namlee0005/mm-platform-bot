package mas

import (
	"log/slog"
	"time"

	"github.com/shopspring/decimal"
)

// QuoteConfig holds parameters for quote generation across regimes.
// Passed alongside MASConfig during Init; not hot-reloaded.
type QuoteConfig struct {
	BaseSpread           decimal.Decimal `yaml:"base_spread"`
	BaseSize             decimal.Decimal `yaml:"base_size"`
	SpikeNearSpreadMul   decimal.Decimal `yaml:"spike_near_spread_multiplier"`
	SpikeNearSizeRatio   decimal.Decimal `yaml:"spike_near_size_ratio"`
	SpikeDeepOffsetTicks int             `yaml:"spike_deep_offset_ticks"`
	SpikeDeepSizeRatio   decimal.Decimal `yaml:"spike_deep_size_ratio"`
	ComplianceFatTailPct decimal.Decimal `yaml:"compliance_fat_tail_pct"`
}

// TickOutput is returned by Tick to the execution layer.
type TickOutput struct {
	Orders     []Order
	CancelBids bool
	CancelAsks bool
	CancelAll  bool
	State      MASState
}

// Strategy is the interface the execution layer calls each tick.
type Strategy interface {
	Name() string
	Init(cfg *MASConfig, quoteCfg *QuoteConfig) error
	Tick(book OrderBook, trades []TradeEvent, now time.Time) TickOutput
	OnFill(fill Fill)
	OnOrderUpdate(orderID string, status string)
	UpdateConfig(cfg *MASConfig)
}

// MASStrategy is the capital-preservation market-making strategy.
// Single-goroutine tick pipeline: no concurrent field access during Tick.
type MASStrategy struct {
	state     MASState
	ofi       OFITracker
	vol       *VolatilityTracker
	toxicity  ToxicityPauseManager
	priceHist *PriceHistory
	quoteCfg  QuoteConfig
	logger    *slog.Logger
}

// Compile-time interface check.
var _ Strategy = (*MASStrategy)(nil)

// NewMASStrategy constructs the strategy. Call Init before first Tick.
func NewMASStrategy(logger *slog.Logger) *MASStrategy {
	if logger == nil {
		logger = slog.Default()
	}
	return &MASStrategy{logger: logger}
}

func (s *MASStrategy) Name() string { return "MAS-CapitalPreservation" }

// Init wires config, creates sub-components, resets state.
func (s *MASStrategy) Init(cfg *MASConfig, quoteCfg *QuoteConfig) error {
	MASConfigPtr.Store(cfg)
	s.quoteCfg = *quoteCfg
	s.vol = NewVolatilityTracker(cfg.Volatility.EWMAHalflifeSeconds)
	s.priceHist = NewPriceHistory(4096)
	s.ofi.Reset()
	s.toxicity.Reset()
	s.state = MASState{
		Regime:           RegimeSideways,
		LastFeedTimestamp: time.Now(),
	}
	return nil
}

// Tick runs the full pipeline: fair value -> regime -> quotes -> caps.
func (s *MASStrategy) Tick(book OrderBook, trades []TradeEvent, now time.Time) TickOutput {
	cfg := MASConfigPtr.Load()
	if cfg == nil {
		return TickOutput{CancelAll: true}
	}

	s.state.LastFeedTimestamp = now

	// --- 1. Ingest trades into OFI accumulator ---
	for _, t := range trades {
		s.ofi.Update(t.Side == Bid, t.Qty)
	}

	// --- 2. Compute fair value: EWMA Mid-Price + OFI adjustment ---
	midPrice := book.BestBid.Add(book.BestAsk).Div(decimal.NewFromInt(2))
	ewmaMid := s.midTracker.Update(midPrice, now)
	ofiNorm := s.ofi.Normalized()
	fairValue := ComputeFairValue(ewmaMid, ofiNorm, cfg.FairValue)

	s.state.FairValue = fairValue
	s.state.OFINormalized = ofiNorm

	// Record mid-price for toxicity lookback.
	s.priceHist.Add(PriceSnapshot{Mid: midPrice, Timestamp: now})

	// --- 3. Update EWMA volatility and classify regime ---
	midF, _ := midPrice.Float64()
	s.vol.Update(midF)
	ewmaVol := s.vol.EWMAVol()
	s.state.EWMAVol = ewmaVol
	s.state.EWMAVar = s.vol.EWMAVar()
	s.state.Regime = ClassifyRegime(ewmaVol, s.state.Regime, cfg.Volatility)

	// --- 4. Inventory ratio for skew ---
	s.state.InventoryRatio = ComputeInventoryRatio(s.state.NetPosition, cfg.Inventory)

	// --- 5. Generate L0-L9 quotes based on regime ---
	var orders []Order
	switch s.state.Regime {
	case RegimeSideways:
		orders = s.buildSidewaysQuotes(fairValue)
	case RegimeSpike:
		orders = s.buildSpikeQuotes(fairValue, cfg)
	}

	// --- 6. Apply inventory skew to all orders ---
	orders = ApplyInventorySkew(orders, s.state.InventoryRatio, cfg.Inventory, cfg.FairValue.TickSize)

	// --- 7. Compliance fat-tail: force L9 to exactly 1.95% from mid ---
	orders = s.applyComplianceFatTail(orders, midPrice)

	// --- 8. Toxicity pauses: remove paused side except L9 anchor ---
	bidPaused := s.toxicity.IsPaused(Bid)
	askPaused := s.toxicity.IsPaused(Ask)
	if bidPaused {
		orders = filterPausedSide(orders, Bid)
		side := Bid
		s.state.ToxicPausedSide = &side
	} else if askPaused {
		orders = filterPausedSide(orders, Ask)
		side := Ask
		s.state.ToxicPausedSide = &side
	} else {
		s.state.ToxicPausedSide = nil
	}

	// --- 9. Emergency hard caps: removes ALL including L9 ---
	cancelBids, cancelAsks := CheckEmergencyCap(s.state.NetPosition, cfg.Inventory)
	if cancelBids {
		orders = removeSide(orders, Bid)
	}
	if cancelAsks {
		orders = removeSide(orders, Ask)
	}

	bidToxic, askToxic := s.toxicity.Counts()
	s.state.ToxicFillCount = bidToxic + askToxic

	s.logger.Info("tick",
		"regime", s.state.Regime.String(),
		"fair_value", fairValue.String(),
		"ewma_vol", ewmaVol,
		"inventory_ratio", s.state.InventoryRatio,
		"orders", len(orders),
		"cancel_bids", cancelBids,
		"cancel_asks", cancelAsks,
		"bid_paused", bidPaused,
		"ask_paused", askPaused,
	)

	return TickOutput{
		Orders:     orders,
		CancelBids: cancelBids,
		CancelAsks: cancelAsks,
		State:      s.state,
	}
}

// buildSidewaysQuotes generates L0-L9 bid+ask with uniform spread and size.
func (s *MASStrategy) buildSidewaysQuotes(fairValue decimal.Decimal) []Order {
	qcfg := s.quoteCfg
	orders := make([]Order, 0, 20) // 10 levels * 2 sides

	for lvl := L0; lvl <= L9; lvl++ {
		bidPrice := PriceAtLevel(Bid, lvl, fairValue, qcfg.BaseSpread)
		askPrice := PriceAtLevel(Ask, lvl, fairValue, qcfg.BaseSpread)
		orders = append(orders,
			Order{Side: Bid, Level: lvl, Price: bidPrice, Size: qcfg.BaseSize},
			Order{Side: Ask, Level: lvl, Price: askPrice, Size: qcfg.BaseSize},
		)
	}
	return orders
}

// buildSpikeQuotes generates regime-aware quotes:
//   - L0-L3: widened spread (thin size) to avoid fills near fair value
//   - L4:    empty buffer zone with no orders
//   - L5-L9: deep offset (thick size) for capital preservation
func (s *MASStrategy) buildSpikeQuotes(fairValue decimal.Decimal, cfg *MASConfig) []Order {
	qcfg := s.quoteCfg
	orders := make([]Order, 0, 18) // (4 near + 5 deep) * 2 sides

	// Near levels: widened spread, thin size.
	nearSpread := qcfg.BaseSpread.Mul(qcfg.SpikeNearSpreadMul)
	nearSize := qcfg.BaseSize.Mul(qcfg.SpikeNearSizeRatio)

	for lvl := L0; lvl <= L3; lvl++ {
		bidPrice := PriceAtLevel(Bid, lvl, fairValue, nearSpread)
		askPrice := PriceAtLevel(Ask, lvl, fairValue, nearSpread)
		orders = append(orders,
			Order{Side: Bid, Level: lvl, Price: bidPrice, Size: nearSize},
			Order{Side: Ask, Level: lvl, Price: askPrice, Size: nearSize},
		)
	}

	// L4: intentionally empty as a buffer zone enforced by omission.

	// Deep levels: offset from the near edge, thick size.
	nearEdge := nearSpread.Mul(decimal.NewFromInt(4)) // L3 edge = nearSpread * (3+1)
	deepStep := cfg.FairValue.TickSize.Mul(decimal.NewFromInt(int64(qcfg.SpikeDeepOffsetTicks)))
	deepSize := qcfg.BaseSize.Mul(qcfg.SpikeDeepSizeRatio)

	for lvl := L5; lvl <= L9; lvl++ {
		rank := decimal.NewFromInt(int64(lvl - L5 + 1))
		totalOffset := nearEdge.Add(deepStep.Mul(rank))
		bidPrice := fairValue.Sub(totalOffset)
		askPrice := fairValue.Add(totalOffset)
		orders = append(orders,
			Order{Side: Bid, Level: lvl, Price: bidPrice, Size: deepSize},
			Order{Side: Ask, Level: lvl, Price: askPrice, Size: deepSize},
		)
	}

	return orders
}

// applyComplianceFatTail overrides L9 bid/ask to sit exactly at
// ComplianceFatTailPct (default 1.95%) from mid-price.
// L9 is the compliance anchor and survives toxicity pauses.
// Only the circuit breaker or emergency cap may remove it.
func (s *MASStrategy) applyComplianceFatTail(orders []Order, midPrice decimal.Decimal) []Order {
	pct := s.quoteCfg.ComplianceFatTailPct
	if pct.IsZero() {
		pct = decimal.NewFromFloat(0.0195)
	}

	offset := midPrice.Mul(pct)
	l9Bid := midPrice.Sub(offset)
	l9Ask := midPrice.Add(offset)

	foundBid, foundAsk := false, false
	for i := range orders {
		if orders[i].Level != L9 {
			continue
		}
		if orders[i].Side == Bid {
			orders[i].Price = l9Bid
			foundBid = true
		} else {
			orders[i].Price = l9Ask
			foundAsk = true
		}
	}

	// Ensure L9 exists even if regime builder omitted it.
	if !foundBid {
		orders = append(orders, Order{Side: Bid, Level: L9, Price: l9Bid, Size: s.quoteCfg.BaseSize})
	}
	if !foundAsk {
		orders = append(orders, Order{Side: Ask, Level: L9, Price: l9Ask, Size: s.quoteCfg.BaseSize})
	}

	return orders
}

// filterPausedSide removes orders for a paused side but keeps L9 (compliance anchor).
func filterPausedSide(orders []Order, pausedSide Side) []Order {
	result := make([]Order, 0, len(orders))
	for _, o := range orders {
		if o.Side == pausedSide && o.Level != L9 {
			continue
		}
		result = append(result, o)
	}
	return result
}

// removeSide strips ALL orders for a side including L9 (emergency cap override).
func removeSide(orders []Order, side Side) []Order {
	result := make([]Order, 0, len(orders))
	for _, o := range orders {
		if o.Side != side {
			result = append(result, o)
		}
	}
	return result
}

// OnFill updates position, evaluates toxicity, applies pause if warranted.
func (s *MASStrategy) OnFill(fill Fill) {
	cfg := MASConfigPtr.Load()
	if cfg == nil {
		return
	}

	// Update net position.
	if fill.Side == Bid {
		s.state.NetPosition = s.state.NetPosition.Add(fill.Qty)
	} else {
		s.state.NetPosition = s.state.NetPosition.Sub(fill.Qty)
	}

	// Evaluate toxicity against price history.
	isToxic := EvaluateFill(fill, s.priceHist, s.state.Regime, cfg.Toxicity, cfg.FairValue.TickSize)
	pauseDur := s.toxicity.RecordFill(fill.Side, isToxic)

	if isToxic {
		s.logger.Warn("toxic fill detected",
			"side", fill.Side,
			"price", fill.Price.String(),
			"qty", fill.Qty.String(),
			"pause_duration", pauseDur.String(),
			"order_id", fill.OrderID,
		)
	}
}

// OnOrderUpdate handles order lifecycle events. Currently a no-op placeholder.
func (s *MASStrategy) OnOrderUpdate(orderID string, status string) {
	// Order lifecycle tracking (ack, partial, cancelled) would go here.
}

// UpdateConfig hot-swaps the config pointer. Re-creates the volatility tracker
// if the halflife changed.
func (s *MASStrategy) UpdateConfig(cfg *MASConfig) {
	prev := MASConfigPtr.Load()
	MASConfigPtr.Store(cfg)

	if prev == nil || prev.Volatility.EWMAHalflifeSeconds != cfg.Volatility.EWMAHalflifeSeconds {
		s.vol = NewVolatilityTracker(cfg.Volatility.EWMAHalflifeSeconds)
	}
}