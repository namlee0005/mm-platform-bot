package strategy

import (
	"math"
	"math/rand"

	"mm-platform-engine/internal/core"
)

// ──────────────────────────────────────────────────────────────────────────────
// Constants
// ──────────────────────────────────────────────────────────────────────────────

const (
	maxDistancePct = 0.02 // ±2% hard cap

	// Step 4: spread scaling
	baseSpreadPct = 0.0005 // 5 bps base half-spread

	// Step 5: ladder gamma
	gammaNormal   = 1.8
	gammaModerate = 1.4
	gammaSpike    = 1.2
	spikeAlpha    = 0.25 // widening factor

	// Step 6: size weights
	normalDecayK    = 30.0
	spikeDecayInner = 0.5 // exp(-0.5 * i) decay in spike mode

	// Step 7: inventory
	invTanhScale = 1.5 // tanh(1.5 * inv)
	invSizeSkew  = 0.8
	invPriceSkew = 0.3

	// Step 8: OFI tilt
	// Positive OFI = buy pressure = strong bid support.
	// Strategy: trade AGAINST the flow — widen bids (others already bidding),
	// tighten asks (sell into buyers). This is the anti-adverse-selection approach
	// for illiquid tokens where OFI reflects support, not informed flow.
	ofiTilt = 0.1

	// Step 11: extreme protection
	extremeThreshold   = 5.0
	extremeReduction   = 0.1  // minimum keep ratio for top levels
	extremeNoQuoteZone = 10.0 // ±10 bps no-quote zone near fair

	// Step 12: size throttling — hyperbolic curve: 1/(1+0.5*S)
	// S=0→100%, S=2→50%, S=4→33%, S=8→20%
	sizeThrottleCoeff = 0.5
	sizeThrottleFloor = 0.2 // never below 20% of target
)

// ──────────────────────────────────────────────────────────────────────────────
// Params & Output
// ──────────────────────────────────────────────────────────────────────────────

// SpikeDepthParams holds all inputs for the v2 spike-adaptive depth engine.
type SpikeDepthParams struct {
	// Market data
	BestBid     float64
	BestAsk     float64
	BestBidSize float64
	BestAskSize float64
	TickSize    float64
	StepSize    float64
	MinNotional float64
	MaxOrderQty float64

	// Strategy config
	NLevels    int
	TotalDepth float64 // target notional per side ($)
	DepthBps   float64 // max depth in bps (e.g. 200 = 2%)

	// Signals
	Sigma         float64 // realized vol (sqrt of EWM variance)
	Ret           float64 // most recent log return
	NormalizedOFI float64 // order flow imbalance in [-1, 1]
	InvRatio      float64 // q/Q: normalized inventory in [-1, 1]

	// Scaling
	SizeMult float64 // drawdown/recovery multiplier

	// Queue awareness (optional, 0 = not available)
	QueuePositionRatio float64 // 0 = front, 1 = back
}

// SpikeDepthResult contains the engine output including computed fair price.
type SpikeDepthResult struct {
	Mid        float64
	Microprice float64
	FairPrice  float64
	SpikeScore float64
	Orders     []core.DesiredOrder
}

// ──────────────────────────────────────────────────────────────────────────────
// Engine — 12-step
// ──────────────────────────────────────────────────────────────────────────────

func generateSpikeAdaptiveOrders(p SpikeDepthParams) SpikeDepthResult {
	empty := SpikeDepthResult{}
	if p.BestBid <= 0 || p.BestAsk <= 0 || p.NLevels <= 0 || p.TotalDepth <= 0 {
		return empty
	}

	n := p.NLevels
	maxDist := maxDistancePct
	if p.DepthBps > 0 {
		maxDist = math.Min(maxDist, (p.DepthBps-15.0)/10000.0)
	}
	if maxDist <= 0 {
		maxDist = maxDistancePct
	}

	// ── Step 1: Mid & Microprice ──
	mid := (p.BestBid + p.BestAsk) / 2.0
	totalTopSize := p.BestBidSize + p.BestAskSize
	microprice := mid
	if totalTopSize > 1e-9 {
		microprice = (p.BestAsk*p.BestBidSize + p.BestBid*p.BestAskSize) / totalTopSize
	}

	// ── Step 2: Fair price ──
	// FIX C4: Scale OFI by mid*0.001 instead of tickSize*100 (~1bps per unit OFI)
	fairPrice := mid
	fairPrice += 0.7 * (microprice - mid)
	fairPrice += 0.1 * p.NormalizedOFI * mid * 0.001
	fairPrice += 0.05 * p.Ret * mid

	// ── Step 3: Spike score ──
	S := computeSpikeScore(p.Ret, p.Sigma)

	// ── Step 4: Spread scaling (smooth) ──
	spread := baseSpreadPct * (1.0 + 2.0*math.Pow(S, 0.7))

	// ── Step 5: Ladder distances ──
	gamma := gammaNormal
	if S >= 3.0 {
		gamma = gammaSpike
	} else if S >= 1.5 {
		gamma = gammaModerate
	}

	distances := make([]float64, n)
	for i := 0; i < n; i++ {
		t := float64(i+1) / float64(n)
		distances[i] = maxDist * math.Pow(t, gamma)
	}

	// Spike widening
	for i := range distances {
		distances[i] *= (1.0 + spikeAlpha*S)
		if distances[i] > maxDist {
			distances[i] = maxDist
		}
	}

	// Ensure minimum distance = spread (don't quote inside spread)
	for i := range distances {
		if distances[i] < spread {
			distances[i] = spread
		}
	}

	// FIX C3: Deterministic jitter from fair price seed (stable across ticks at same price)
	rng := rand.New(rand.NewSource(int64(fairPrice * 1e8)))
	for i := range distances {
		jitter := 1.0 + (rng.Float64()*0.04 - 0.02) // ±2%
		distances[i] *= jitter
		if distances[i] > maxDist {
			distances[i] = maxDist
		}
	}

	// ── Step 6: Size profile ──
	// FIX BUG 2: Smooth blend between normal and spike regimes (no hard switch)
	blend := clamp01((S - 1.0) / 2.0) // 0 at S<=1, 1 at S>=3

	weights := make([]float64, n)
	var totalW float64
	for i, d := range distances {
		wNormal := math.Exp(-normalDecayK * d)
		wSpike := math.Pow(math.Max(d, 1e-9), 1.0) * math.Exp(-spikeDecayInner*float64(i))
		weights[i] = (1.0-blend)*wNormal + blend*wSpike
		if weights[i] < 1e-12 {
			weights[i] = 1e-12
		}
		totalW += weights[i]
	}
	if totalW > 0 {
		for i := range weights {
			weights[i] /= totalW
		}
	}

	// ── Step 7: Inventory skew (nonlinear tanh) ──
	inv := clamp(p.InvRatio, -1.0, 1.0)
	skew := math.Tanh(invTanhScale * inv) // smooth, saturates at ±1

	bidSizes := make([]float64, n)
	askSizes := make([]float64, n)
	for i := range weights {
		baseSize := p.TotalDepth * weights[i] * p.SizeMult
		bidSizes[i] = baseSize * math.Max(0.2, 1.0-invSizeSkew*skew)
		askSizes[i] = baseSize * math.Max(0.2, 1.0+invSizeSkew*skew)
	}

	// ── Step 8: OFI directional tilt on distances ──
	// Positive OFI (buy pressure / strong bid support) →
	//   widen bids (don't compete with crowd), tighten asks (sell into buyers)
	ofi := clamp(p.NormalizedOFI, -1.0, 1.0)
	bidDistances := make([]float64, n)
	askDistances := make([]float64, n)
	for i, d := range distances {
		bidDistances[i] = d * (1.0 + ofiTilt*ofi)
		askDistances[i] = d * (1.0 - ofiTilt*ofi)
		bidDistances[i] = clamp(bidDistances[i], spread, maxDist)
		askDistances[i] = clamp(askDistances[i], spread, maxDist)
	}

	// ── Step 9: Generate prices around fair_price ──
	bidPrices := make([]float64, n)
	askPrices := make([]float64, n)
	for i := range distances {
		rawBid := fairPrice * (1.0 - bidDistances[i])
		rawAsk := fairPrice * (1.0 + askDistances[i])

		// Inventory price skew: long (skew>0) → bids further, asks closer
		bidPriceShift := clamp(1.0+invPriceSkew*skew, 0.5, 2.0)
		askPriceShift := clamp(1.0-invPriceSkew*skew, 0.5, 2.0)
		bidPrices[i] = sdeRoundToTick(fairPrice-(fairPrice-rawBid)*bidPriceShift, p.TickSize)
		askPrices[i] = sdeRoundToTick(fairPrice+(rawAsk-fairPrice)*askPriceShift, p.TickSize)
	}

	// FIX BUG 1: Dedup prices — ensure each level has unique price (min 2 ticks apart)
	// After rounding, capped distances may collapse to the same tick.
	for i := 1; i < n; i++ {
		minBidGap := p.TickSize * 2
		if bidPrices[i-1]-bidPrices[i] < minBidGap && bidPrices[i] > 0 {
			bidPrices[i] = sdeRoundToTick(bidPrices[i-1]-minBidGap, p.TickSize)
		}
	}
	for i := 1; i < n; i++ {
		minAskGap := p.TickSize * 2
		if askPrices[i]-askPrices[i-1] < minAskGap && askPrices[i] > 0 {
			askPrices[i] = sdeRoundToTick(askPrices[i-1]+minAskGap, p.TickSize)
		}
	}

	// ── Step 10: Queue awareness ──
	queuePriceImprove := 0.0
	if p.QueuePositionRatio > 0.7 {
		queuePriceImprove = p.TickSize * (1.0 + (p.QueuePositionRatio-0.7)*3.0)
	}
	if queuePriceImprove > 0 {
		for i := 0; i < minInt(2, n); i++ {
			bidPrices[i] = sdeRoundToTick(bidPrices[i]+queuePriceImprove, p.TickSize)
			askPrices[i] = sdeRoundToTick(askPrices[i]-queuePriceImprove, p.TickSize)
		}
	}

	// ── Step 11: Extreme protection ──
	if S >= extremeThreshold {
		noQuoteInner := fairPrice * extremeNoQuoteZone / 10000.0
		for i := 0; i < n; i++ {
			if math.Abs(bidPrices[i]-fairPrice) < noQuoteInner {
				bidSizes[i] = 0
			}
			if math.Abs(askPrices[i]-fairPrice) < noQuoteInner {
				askSizes[i] = 0
			}
		}
		// FIX C2: Gradual reduction based on |skew| instead of binary check
		// skew=0 → no reduction, |skew|=1 → 90% reduction
		absSkew := math.Abs(skew)
		reduction := extremeReduction + (1.0-extremeReduction)*(1.0-absSkew)
		for i := 0; i < 2 && i < n; i++ {
			if skew > 0 {
				bidSizes[i] *= reduction
			} else if skew < 0 {
				askSizes[i] *= reduction
			}
		}
	}

	// ── Step 12: Size throttling ──
	// FIX C1: Hyperbolic curve instead of exp — gentler, keeps depth for exchange reqs
	// 1/(1+0.5*S): S=0→100%, S=2→50%, S=4→33%, S=8→20%
	sizeMultiplier := 1.0 / (1.0 + sizeThrottleCoeff*S)
	if sizeMultiplier < sizeThrottleFloor {
		sizeMultiplier = sizeThrottleFloor
	}
	for i := range bidSizes {
		bidSizes[i] *= sizeMultiplier
		askSizes[i] *= sizeMultiplier
	}

	// ── Build output ──
	orders := make([]core.DesiredOrder, 0, n*2)

	for i := 0; i < n; i++ {
		price := bidPrices[i]
		if price <= 0 || price >= fairPrice {
			continue
		}
		if p.BestAsk > 0 && price >= p.BestAsk {
			continue
		}
		qty := sdeFloorToStep(bidSizes[i]/price, p.StepSize)
		if qty <= 0 || price*qty < p.MinNotional {
			continue
		}
		if p.MaxOrderQty > 0 && qty > p.MaxOrderQty {
			qty = p.MaxOrderQty
		}
		orders = append(orders, core.DesiredOrder{
			Side:       "BUY",
			Price:      price,
			Qty:        qty,
			LevelIndex: i,
		})
	}

	for i := 0; i < n; i++ {
		price := askPrices[i]
		if price <= 0 || price <= fairPrice {
			continue
		}
		if p.BestBid > 0 && price <= p.BestBid {
			continue
		}
		qty := sdeFloorToStep(askSizes[i]/price, p.StepSize)
		if qty <= 0 || price*qty < p.MinNotional {
			continue
		}
		if p.MaxOrderQty > 0 && qty > p.MaxOrderQty {
			qty = p.MaxOrderQty
		}
		orders = append(orders, core.DesiredOrder{
			Side:       "SELL",
			Price:      price,
			Qty:        qty,
			LevelIndex: i,
		})
	}

	return SpikeDepthResult{
		Mid:        mid,
		Microprice: microprice,
		FairPrice:  fairPrice,
		SpikeScore: S,
		Orders:     orders,
	}
}

// computeSpikeScore computes S = |ret| / max(sigma, eps), capped at 10.
func computeSpikeScore(ret, sigma float64) float64 {
	S := math.Abs(ret) / math.Max(sigma, 1e-9)
	if S > 10.0 {
		S = 10.0
	}
	return S
}

// ── Helpers ──

func sdeRoundToTick(price, tickSize float64) float64 {
	if tickSize <= 0 {
		return price
	}
	return math.Round(price/tickSize) * tickSize
}

func sdeFloorToStep(qty, stepSize float64) float64 {
	if stepSize <= 0 {
		return qty
	}
	prec := 0.0
	s := stepSize
	for s < 1.0 && prec < 10 {
		s *= 10
		prec++
	}
	factor := math.Pow(10, prec)
	result := math.Floor(qty/stepSize) * stepSize
	return math.Round(result*factor) / factor
}

func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func clamp01(v float64) float64 {
	return clamp(v, 0, 1)
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
