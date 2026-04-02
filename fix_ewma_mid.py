import re

# 1. Update mas_types.go
with open('internal/strategy/mas_types.go', 'r') as f:
    types = f.read()

types = types.replace(
    'OFIAlpha decimal.Decimal `yaml:"ofi_alpha"`',
    'OFIAlpha decimal.Decimal `yaml:"ofi_alpha"`\n\tEWMAMidWindow time.Duration `yaml:"ewma_mid_window"`'
)
with open('internal/strategy/mas_types.go', 'w') as f:
    f.write(types)

# 2. Update mas_fair_value.go
with open('internal/strategy/mas_fair_value.go', 'r') as f:
    fv = f.read()

# Replace MicroPrice with EWMAMidTracker
new_tracker = """// EWMAMidTracker maintains an Exponential Weighted Moving Average of the Mid-Price.
// This is highly resistant to spoofing on low-liquidity books.
type EWMAMidTracker struct {
	mu           sync.Mutex
	ewmaPrice    decimal.Decimal
	lastUpdate   time.Time
	halflife     time.Duration
	isInitialized bool
}

func NewEWMAMidTracker(halflife time.Duration) *EWMAMidTracker {
	return &EWMAMidTracker{
		halflife: halflife,
	}
}

// Update incorporates a new mid-price observation into the EWMA.
func (t *EWMAMidTracker) Update(mid decimal.Decimal, now time.Time) decimal.Decimal {
	t.mu.Lock()
	defer t.mu.Unlock()

	if !t.isInitialized {
		t.ewmaPrice = mid
		t.lastUpdate = now
		t.isInitialized = true
		return t.ewmaPrice
	}

	dt := now.Sub(t.lastUpdate).Seconds()
	if dt <= 0 {
		return t.ewmaPrice
	}

	// lambda = ln(2) / halflife
	halflifeSec := t.halflife.Seconds()
	if halflifeSec <= 0 {
		halflifeSec = 60.0 // fallback 60s
	}
	lambda := math.Ln2 / halflifeSec
	weight := math.Exp(-lambda * dt)
	decWeight := decimal.NewFromFloat(weight)
	invWeight := decimal.NewFromInt(1).Sub(decWeight)

	// EWMA_new = EWMA_old * weight + Mid_new * (1 - weight)
	t.ewmaPrice = t.ewmaPrice.Mul(decWeight).Add(mid.Mul(invWeight))
	t.lastUpdate = now
	return t.ewmaPrice
}
"""
fv = re.sub(r'// ComputeMicroPrice returns the quantity-weighted mid-price\.[\s\S]*?totalQty\)\n}', new_tracker, fv)

with open('internal/strategy/mas_fair_value.go', 'w') as f:
    f.write(fv)

# 3. Update mas_strategy.go
with open('internal/strategy/mas_strategy.go', 'r') as f:
    strat = f.read()

# Add Tracker to MASStrategy struct
strat = strat.replace(
    'volTracker *VolatilityTracker',
    'volTracker *VolatilityTracker\n\tmidTracker *EWMAMidTracker'
)

# Init Tracker in Init()
strat = strat.replace(
    's.volTracker = NewVolatilityTracker(s.cfg.Volatility)',
    's.volTracker = NewVolatilityTracker(s.cfg.Volatility)\n\ts.midTracker = NewEWMAMidTracker(s.cfg.FairValue.EWMAMidWindow)'
)

# Replace microPrice calculation in Tick()
old_tick = """	// --- 2. Compute fair value: micro-price + OFI adjustment ---
	microPrice := ComputeMicroPrice(book.BestBid, book.BestAsk, book.BidQty, book.AskQty)
	ofiNorm := s.ofi.Normalized()
	fairValue := ComputeFairValue(microPrice, ofiNorm, cfg.FairValue)"""

new_tick = """	// --- 2. Compute fair value: EWMA Mid-Price + OFI adjustment ---
	midPrice := book.BestBid.Add(book.BestAsk).Div(decimal.NewFromInt(2))
	ewmaMid := s.midTracker.Update(midPrice, now)
	ofiNorm := s.ofi.Normalized()
	fairValue := ComputeFairValue(ewmaMid, ofiNorm, cfg.FairValue)"""

strat = strat.replace(old_tick, new_tick)
strat = strat.replace('midPrice := book.BestBid.Add(book.BestAsk).Div(decimal.NewFromInt(2))\n\ts.priceHist.Add(PriceSnapshot{Mid: midPrice, Timestamp: now})', 's.priceHist.Add(PriceSnapshot{Mid: midPrice, Timestamp: now})')

with open('internal/strategy/mas_strategy.go', 'w') as f:
    f.write(strat)

