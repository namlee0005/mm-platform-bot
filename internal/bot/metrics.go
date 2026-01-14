package bot

import (
	"mm-platform-engine/internal/types"
	"mm-platform-engine/internal/utils"
	"sort"
	"time"
)

// FillEvent represents a single fill event
type FillEvent struct {
	Side      types.OrderSide
	Price     float64
	Qty       float64
	Notional  float64
	Timestamp int64 // Unix milliseconds
}

// CancelEvent represents a cancel event
type CancelEvent struct {
	Timestamp int64 // Unix milliseconds
}

// MetricsAggregator maintains rolling window metrics
type MetricsAggregator struct {
	fills      []FillEvent
	cancels    []CancelEvent
	ttfSamples []float64 // Time-to-fill samples in seconds
	windowMs   int64     // Rolling window size in milliseconds
}

// NewMetricsAggregator creates a new metrics aggregator
func NewMetricsAggregator(windowMs int64) *MetricsAggregator {
	return &MetricsAggregator{
		fills:      make([]FillEvent, 0),
		cancels:    make([]CancelEvent, 0),
		ttfSamples: make([]float64, 0),
		windowMs:   windowMs,
	}
}

// RecordFill records a fill event
func (m *MetricsAggregator) RecordFill(side types.OrderSide, price, qty float64, timestamp int64) {
	m.fills = append(m.fills, FillEvent{
		Side:      side,
		Price:     price,
		Qty:       qty,
		Notional:  price * qty,
		Timestamp: timestamp,
	})
}

// RecordCancel records a cancel event
func (m *MetricsAggregator) RecordCancel(timestamp int64) {
	m.cancels = append(m.cancels, CancelEvent{
		Timestamp: timestamp,
	})
}

// RecordTimeToFill records a time-to-fill sample
func (m *MetricsAggregator) RecordTimeToFill(seconds float64) {
	m.ttfSamples = append(m.ttfSamples, seconds)
}

// ComputeMetrics computes the rolling metrics at the given timestamp
func (m *MetricsAggregator) ComputeMetrics(nowMs int64, lastInvRatio, prevInvRatio float64, hourMs int64) types.RollingMetrics {
	// Clean old data outside the window
	m.cleanOldData(nowMs)

	windowMinutes := float64(m.windowMs) / (60.0 * 1000.0)
	if windowMinutes < utils.Epsilon {
		windowMinutes = 1.0
	}

	// Count fills
	fillCount := float64(len(m.fills))
	fillsPerMin := fillCount / windowMinutes

	// Compute bid/ask fill notional
	var bidNotional, askNotional float64
	for _, fill := range m.fills {
		if fill.Side == types.OrderSideBuy {
			bidNotional += fill.Notional
		} else {
			askNotional += fill.Notional
		}
	}

	// Compute hit imbalance
	hitImbalance := utils.ComputeHitImbalance(bidNotional, askNotional)

	// Compute TTF P50
	ttfP50 := 0.0
	if len(m.ttfSamples) > 0 {
		ttfP50 = computeMedian(m.ttfSamples)
	}

	// Compute inventory drift per hour
	invDrift := 0.0
	if hourMs > 0 {
		windowHours := float64(m.windowMs) / float64(hourMs)
		if windowHours > utils.Epsilon {
			invDrift = (lastInvRatio - prevInvRatio) / windowHours
		}
	}

	// Compute cancel rate
	cancelCount := float64(len(m.cancels))
	cancelPerMin := cancelCount / windowMinutes

	return types.RollingMetrics{
		FillsPerMin:      fillsPerMin,
		BidFillsNotional: bidNotional,
		AskFillsNotional: askNotional,
		HitImbalance:     hitImbalance,
		TtfP50Sec:        ttfP50,
		InvDriftPerHour:  invDrift,
		CancelPerMin:     cancelPerMin,
	}
}

// cleanOldData removes data outside the rolling window
func (m *MetricsAggregator) cleanOldData(nowMs int64) {
	cutoff := nowMs - m.windowMs

	// Clean fills
	validFills := make([]FillEvent, 0, len(m.fills))
	for _, fill := range m.fills {
		if fill.Timestamp >= cutoff {
			validFills = append(validFills, fill)
		}
	}
	m.fills = validFills

	// Clean cancels
	validCancels := make([]CancelEvent, 0, len(m.cancels))
	for _, cancel := range m.cancels {
		if cancel.Timestamp >= cutoff {
			validCancels = append(validCancels, cancel)
		}
	}
	m.cancels = validCancels

	// Keep only recent TTF samples (last 100)
	if len(m.ttfSamples) > 100 {
		m.ttfSamples = m.ttfSamples[len(m.ttfSamples)-100:]
	}
}

// computeMedian computes the median of a slice of floats
func computeMedian(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}

	// Copy and sort
	sorted := make([]float64, len(values))
	copy(sorted, values)
	sort.Float64s(sorted)

	n := len(sorted)
	if n%2 == 0 {
		return (sorted[n/2-1] + sorted[n/2]) / 2.0
	}
	return sorted[n/2]
}

// GetCurrentTimeMs returns the current time in milliseconds
func GetCurrentTimeMs() int64 {
	return time.Now().UnixNano() / int64(time.Millisecond)
}
