package strategy

import (
	"fmt"
	"log"
	"math"
	"math/rand"

	"mm-platform-engine/internal/core"
)

// ──────────────────────────────────────────────────────────────────────────────
// V2 Constants
// ──────────────────────────────────────────────────────────────────────────────

const (
	// Step 6: v2 size weight base
	v2SizeBase = 1.2

	// Step 9: ninja protection scaling (100bps = 0.01)
	ninjaSpreadScale = 0.01

	// Step 12: v2 extreme no-quote zone ±15bps (widened from v1's ±10bps)
	v2ExtremeNoQuoteZone = 15.0

	// Step 13: compliance boundary
	complianceBoundaryPct = 0.0195 // 1.95% from mid
)

// ──────────────────────────────────────────────────────────────────────────────
// Params & Output
// ──────────────────────────────────────────────────────────────────────────────

// SpikeDepthV2Params holds all inputs for the v2 13-step spike-adaptive depth engine.
type SpikeDepthV2Params struct {
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
	NLevels      int
	TotalDepth   float64 // target notional per side ($)
	DepthBps     float64 // max depth in bps (e.g. 200 = 2%)
	SpreadMinBps float64 // minimum half-spread in bps

	// Signals
	Sigma         float64 // realized vol (sqrt of EWM variance)
	Ret           float64 // most recent log return
	NormalizedOFI float64 // order flow imbalance in [-1, 1]
	InvRatio      float64 // normalized inventory in [-1, 1]

	// Scaling
	SizeMult float64 // drawdown/recovery multiplier

	// Queue awareness (optional, 0 = not available)
	QueuePositionRatio float64 // 0 = front, 1 = back

	// V2: Ninja Protection — accumulated volumes in 30s window
	AccBuyVol30s  float64
	AccSellVol30s float64

	// V2: Compliance Toggle — "capital" or "compliance"
	ComplianceMode string
}

// SpikeDepthV2Result contains the v2 engine output.
type SpikeDepthV2Result struct {
	Mid        float64
	Microprice float64
	FairPrice  float64
	SpikeScore float64
	Orders     []core.DesiredOrder

	// Debug fields for logging
	Gamma      float64
	Spread     float64 // as fraction (e.g. 0.008 = 80bps)
	InvSkew    float64 // tanh-scaled inventory skew
	BidScale   float64 // size multiplier for bid side
	AskScale   float64 // size multiplier for ask side
	SizeMult   float64 // spike throttle multiplier
	WeightL0   float64 // weight of level 0 (normalized)
	WeightLast float64 // weight of last level (normalized)
}

// ──────────────────────────────────────────────────────────────────────────────
// Engine — 13-step pipeline
// ──────────────────────────────────────────────────────────────────────────────

func generateSpikeAdaptiveOrdersV2(p SpikeDepthV2Params) SpikeDepthV2Result {
	empty := SpikeDepthV2Result{}
	if p.BestBid <= 0 || p.BestAsk <= 0 || p.NLevels <= 0 || p.TotalDepth <= 0 {
		return empty
	}

	n := p.NLevels
	safeMaxDist := maxDistancePct - outerSafetyBps/10000.0
	maxDist := safeMaxDist
	if p.DepthBps > 0 {
		maxDist = math.Min(maxDist, (p.DepthBps-15.0)/10000.0)
	}
	if maxDist <= 0 {
		maxDist = safeMaxDist
	}

	// ── Step 1: Mid & Microprice ──
	mid := (p.BestBid + p.BestAsk) / 2.0
	totalTopSize := p.BestBidSize + p.BestAskSize
	microprice := mid
	if totalTopSize > 1e-9 {
		microprice = (p.BestAsk*p.BestBidSize + p.BestBid*p.BestAskSize) / totalTopSize
	}

	// ── Step 2: Fair price ──
	fairPrice := mid
	fairPrice += 0.7 * (microprice - mid)
	fairPrice += 0.1 * p.NormalizedOFI * mid * 0.001
	fairPrice += 0.05 * p.Ret * mid

	// ── Step 3: Spike score ──
	S := computeSpikeScore(p.Ret, p.Sigma)

	// ── Step 4: Spread scaling ──
	spread := baseSpreadPct * (1.0 + 2.0*math.Pow(S, 0.7))
	if p.SpreadMinBps > 0 {
		minSpread := p.SpreadMinBps / 10000.0
		if spread < minSpread {
			spread = minSpread
		}
	}

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

	// Ensure minimum distance = spread
	for i := range distances {
		if distances[i] < spread {
			distances[i] = spread
		}
	}

	// Deterministic jitter
	rng := rand.New(rand.NewSource(int64(fairPrice * 1e8)))
	for i := range distances {
		jitter := 1.0 + (rng.Float64()*0.04 - 0.02)
		distances[i] *= jitter
		if distances[i] > maxDist {
			distances[i] = maxDist
		}
	}

	// ── Step 6: Size profile (V2: 1.2^i, flat if S>3) ──
	weights := make([]float64, n)
	var totalW float64
	if S > 3.0 {
		// Flat distribution during high spike — equal across all levels
		for i := range weights {
			weights[i] = 1.0
			totalW += 1.0
		}
	} else {
		// Exponential: thin at L0, thick at outer levels
		for i := 0; i < n; i++ {
			weights[i] = math.Pow(v2SizeBase, float64(i))
			totalW += weights[i]
		}
	}
	if totalW > 0 {
		for i := range weights {
			weights[i] /= totalW
		}
	}

	// ── Step 7: Inventory skew ──
	inv := clamp(p.InvRatio, -1.0, 1.0)
	skew := math.Tanh(invTanhScale * inv)

	bidSizes := make([]float64, n)
	askSizes := make([]float64, n)
	for i := range weights {
		baseSize := p.TotalDepth * weights[i] * p.SizeMult
		bidSizes[i] = baseSize * math.Max(0.2, 1.0-invSizeSkew*skew)
		askSizes[i] = baseSize * math.Max(0.2, 1.0+invSizeSkew*skew)
	}

	// ── Step 8: OFI directional tilt ──
	ofi := clamp(p.NormalizedOFI, -1.0, 1.0)
	bidDistances := make([]float64, n)
	askDistances := make([]float64, n)
	for i, d := range distances {
		bidDistances[i] = d * (1.0 + ofiTilt*ofi)
		askDistances[i] = d * (1.0 - ofiTilt*ofi)
		bidDistances[i] = clamp(bidDistances[i], spread, maxDist)
		askDistances[i] = clamp(askDistances[i], spread, maxDist)
	}

	// ── Step 9: Ninja Protection (V2 NEW) ──
	// Widen spread on side that's being nibbled by accumulated small fills
	addSpreadBid := 0.0
	addSpreadAsk := 0.0
	if p.TotalDepth > 0 {
		addSpreadAsk = (p.AccBuyVol30s / p.TotalDepth) * ninjaSpreadScale
		addSpreadBid = (p.AccSellVol30s / p.TotalDepth) * ninjaSpreadScale
	}

	// ── Step 10: Price generation (with Ninja spread) ──
	bidPrices := make([]float64, n)
	askPrices := make([]float64, n)
	for i := range distances {
		rawBid := fairPrice * (1.0 - bidDistances[i] - addSpreadBid)
		rawAsk := fairPrice * (1.0 + askDistances[i] + addSpreadAsk)

		// Inventory price skew
		bidPriceShift := clamp(1.0+invPriceSkew*skew, 0.5, 2.0)
		askPriceShift := clamp(1.0-invPriceSkew*skew, 0.5, 2.0)
		bidPrices[i] = sdeRoundToTick(fairPrice-(fairPrice-rawBid)*bidPriceShift, p.TickSize)
		askPrices[i] = sdeRoundToTick(fairPrice+(rawAsk-fairPrice)*askPriceShift, p.TickSize)
	}

	// Dedup: ensure min 2-tick gaps
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

	// ── Step 11: Queue awareness ──
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

	// ── Step 12: Extreme protection (V2: ±15bps no-quote zone) ──
	if S >= extremeThreshold {
		noQuoteInner := fairPrice * v2ExtremeNoQuoteZone / 10000.0
		for i := 0; i < n; i++ {
			if math.Abs(bidPrices[i]-fairPrice) < noQuoteInner {
				bidSizes[i] = 0
			}
			if math.Abs(askPrices[i]-fairPrice) < noQuoteInner {
				askSizes[i] = 0
			}
		}
		absSkew := math.Abs(skew)
		reduction := 0.5 - (0.5-extremeReduction)*absSkew
		for i := 0; i < 2 && i < n; i++ {
			if skew > 0 {
				bidSizes[i] *= reduction
			} else if skew < 0 {
				askSizes[i] *= reduction
			}
		}
	}

	// ── Step 13: Compliance Toggle (V2 NEW) ──
	if p.ComplianceMode == "compliance" {
		// Compliance-First: keep full size, push outermost level to 1.95% boundary
		// No size throttle (multiplier = 1.0)
		if n > 0 {
			bidPrices[n-1] = sdeRoundToTick(mid*(1.0-complianceBoundaryPct), p.TickSize)
			askPrices[n-1] = sdeRoundToTick(mid*(1.0+complianceBoundaryPct), p.TickSize)
		}
	} else {
		// Capital-First (default): apply size throttle 1/(1+0.5×S) only when S≥2.
		// S<2 is normal market noise — no throttle to maintain full depth.
		sizeMultiplier := 1.0
		if S >= 2.0 {
			sizeMultiplier = 1.0 / (1.0 + sizeThrottleCoeff*S)
			if sizeMultiplier < sizeThrottleFloor {
				sizeMultiplier = sizeThrottleFloor
			}
		}
		for i := range bidSizes {
			bidSizes[i] *= sizeMultiplier
			if bidSizes[i] > 0 && bidSizes[i] < p.MinNotional*1.1 {
				bidSizes[i] = p.MinNotional * 1.1
			}
			askSizes[i] *= sizeMultiplier
			if askSizes[i] > 0 && askSizes[i] < p.MinNotional*1.1 {
				askSizes[i] = p.MinNotional * 1.1
			}
		}
	}

	// ── Build output ──
	hardMinBid := fairPrice * (1.0 - maxDistancePct)
	hardMaxAsk := fairPrice * (1.0 + maxDistancePct)
	safeMinBid := fairPrice * (1.0 - safeMaxDist)
	safeMaxAsk := fairPrice * (1.0 + safeMaxDist)

	orders := make([]core.DesiredOrder, 0, n*2)

	for i := 0; i < n; i++ {
		price := bidPrices[i]
		if price <= 0 || price >= fairPrice {
			log.Printf("[SDEv2] BID L%d SKIP: price=%.8f >= fair=%.8f or <=0", i, price, fairPrice)
			continue
		}
		if p.BestAsk > 0 && price >= p.BestAsk {
			log.Printf("[SDEv2] BID L%d SKIP: price=%.8f >= bestAsk=%.8f", i, price, p.BestAsk)
			continue
		}
		minBound := hardMinBid
		if n > 2 && i >= n-2 {
			minBound = safeMinBid
		}
		if price < minBound {
			price = sdeRoundToTick(minBound, p.TickSize)
			if price >= fairPrice || (p.BestAsk > 0 && price >= p.BestAsk) {
				continue
			}
			bidPrices[i] = price
		}
		qty := sdeFloorToStep(bidSizes[i]/price, p.StepSize)
		if qty <= 0 || price*qty < p.MinNotional {
			log.Printf("[SDEv2] BID L%d SKIP: qty=%.1f notional=%.2f < min=%.2f (size$=%.2f)", i, qty, price*qty, p.MinNotional, bidSizes[i])
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
			Tag:        fmt.Sprintf("mm_B_%d", i),
		})
	}

	for i := 0; i < n; i++ {
		price := askPrices[i]
		if price <= 0 || price <= fairPrice {
			log.Printf("[SDEv2] ASK L%d SKIP: price=%.8f <= fair=%.8f or <=0", i, price, fairPrice)
			continue
		}
		if p.BestBid > 0 && price <= p.BestBid {
			log.Printf("[SDEv2] ASK L%d SKIP: price=%.8f <= bestBid=%.8f", i, price, p.BestBid)
			continue
		}
		maxBound := hardMaxAsk
		if n > 2 && i >= n-2 {
			maxBound = safeMaxAsk
		}
		if price > maxBound {
			price = sdeRoundToTick(maxBound, p.TickSize)
			if price <= fairPrice || (p.BestBid > 0 && price <= p.BestBid) {
				continue
			}
			askPrices[i] = price
		}
		qty := sdeFloorToStep(askSizes[i]/price, p.StepSize)
		if qty <= 0 || price*qty < p.MinNotional {
			log.Printf("[SDEv2] ASK L%d SKIP: qty=%.1f notional=%.2f < min=%.2f (size$=%.2f)", i, qty, price*qty, p.MinNotional, askSizes[i])
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
			Tag:        fmt.Sprintf("mm_S_%d", i),
		})
	}

	// Compute debug values for logging
	bidScale := math.Max(0.2, 1.0-invSizeSkew*skew)
	askScale := math.Max(0.2, 1.0+invSizeSkew*skew)
	var weightL0, weightLast float64
	if n > 0 {
		weightL0 = weights[0]
		weightLast = weights[n-1]
	}

	return SpikeDepthV2Result{
		Mid:        mid,
		Microprice: microprice,
		FairPrice:  fairPrice,
		SpikeScore: S,
		Orders:     orders,
		Gamma:      gamma,
		Spread:     spread,
		InvSkew:    skew,
		BidScale:   bidScale,
		AskScale:   askScale,
		WeightL0:   weightL0,
		WeightLast: weightLast,
	}
}
