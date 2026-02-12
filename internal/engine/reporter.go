package engine

import (
	"encoding/json"
	"log"
	"sync"
	"time"
)

// Reporter emits structured metrics, summaries, and sanitized trade logs.
type Reporter struct {
	mu  sync.Mutex
	cfg *ReportingConfig

	// Accumulation for summaries
	tickMetrics      []EngineMetrics
	tradeLog         []TradeLogEntry
	lastDailySummary time.Time

	// Callbacks (set by engine)
	OnMetrics      func(EngineMetrics)         // called each metrics interval
	OnDailySummary func(DailySummary)          // called once per day
	OnAlert        func(level, message string) // called on warning/critical
}

func NewReporter(cfg *ReportingConfig) *Reporter {
	return &Reporter{
		cfg:              cfg,
		tickMetrics:      make([]EngineMetrics, 0, 8640),
		tradeLog:         make([]TradeLogEntry, 0, 1024),
		lastDailySummary: time.Now().Truncate(24 * time.Hour),
	}
}

// EmitMetrics records and broadcasts a metrics snapshot
func (r *Reporter) EmitMetrics(m EngineMetrics) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.tickMetrics = append(r.tickMetrics, m)

	// Trim to 24h (at 10s interval → 8640 points)
	if len(r.tickMetrics) > 8640 {
		r.tickMetrics = r.tickMetrics[len(r.tickMetrics)-8640:]
	}

	// Emit via callback
	if r.OnMetrics != nil {
		r.OnMetrics(m)
	}

	// Log key metrics
	log.Printf("[METRICS] mode=%s mid=%.6f spread=%.1fbps depth=$%.0f inv=%.2f%% dd=%.2f%% fills=%.1f/min uptime=%.1f%%",
		m.Mode, m.Mid, m.AvgSpreadBps, m.DepthNotional,
		m.InvRatio*100, m.Drawdown24h*100,
		m.FillsPerMin, m.QuoteUptime*100)

	// Check for daily summary
	r.checkDailySummary(m)
}

// RecordTrade records a sanitized trade (no strategy internals)
func (r *Reporter) RecordTrade(entry TradeLogEntry) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.tradeLog = append(r.tradeLog, entry)

	// Trim to last 10000 trades
	if len(r.tradeLog) > 10000 {
		r.tradeLog = r.tradeLog[len(r.tradeLog)-10000:]
	}
}

// Alert sends an alert via the configured callback
func (r *Reporter) Alert(level, message string) {
	log.Printf("[ALERT:%s] %s", level, message)
	if r.OnAlert != nil {
		r.OnAlert(level, message)
	}
}

// checkDailySummary checks if it's time to emit a daily summary
func (r *Reporter) checkDailySummary(current EngineMetrics) {
	now := current.Timestamp
	summaryHour := r.cfg.DailySummaryHourUTC

	// Check if we've crossed the summary hour since last summary
	todaySummaryTime := time.Date(now.Year(), now.Month(), now.Day(),
		summaryHour, 0, 0, 0, time.UTC)

	if now.After(todaySummaryTime) && r.lastDailySummary.Before(todaySummaryTime) {
		r.generateDailySummary(todaySummaryTime)
		r.lastDailySummary = now
	}
}

func (r *Reporter) generateDailySummary(date time.Time) {
	// Compute summary from accumulated tick metrics
	dayStart := date.Add(-24 * time.Hour)

	var sumSpread, sumDepth, sumInvRatio, maxDD float64
	var count int
	startNAV, endNAV := 0.0, 0.0

	for _, m := range r.tickMetrics {
		if m.Timestamp.Before(dayStart) {
			continue
		}
		sumSpread += m.AvgSpreadBps
		sumDepth += m.DepthNotional
		sumInvRatio += m.InvRatio
		if m.Drawdown24h > maxDD {
			maxDD = m.Drawdown24h
		}
		if startNAV == 0 {
			startNAV = m.NAV
		}
		endNAV = m.NAV
		count++
	}

	if count == 0 {
		return
	}

	summary := DailySummary{
		Date:             date.Format("2006-01-02"),
		AvgSpreadBps:     sumSpread / float64(count),
		AvgDepthNotional: sumDepth / float64(count),
		TotalFills:       len(r.tradeLog), // approximation
		MaxDrawdown:      maxDD,
		StartNAV:         startNAV,
		EndNAV:           endNAV,
		InvRatioAvg:      sumInvRatio / float64(count),
	}

	log.Printf("[REPORT] Daily summary for %s: avg_spread=%.1fbps avg_depth=$%.0f max_dd=%.2f%%",
		summary.Date, summary.AvgSpreadBps, summary.AvgDepthNotional, summary.MaxDrawdown*100)

	if r.OnDailySummary != nil {
		r.OnDailySummary(summary)
	}
}

// GetRecentMetrics returns the last N metrics snapshots
func (r *Reporter) GetRecentMetrics(n int) []EngineMetrics {
	r.mu.Lock()
	defer r.mu.Unlock()

	if n > len(r.tickMetrics) {
		n = len(r.tickMetrics)
	}
	result := make([]EngineMetrics, n)
	copy(result, r.tickMetrics[len(r.tickMetrics)-n:])
	return result
}

// GetTradeLog returns sanitized trade log entries (no strategy leakage)
func (r *Reporter) GetTradeLog(limit int) []TradeLogEntry {
	r.mu.Lock()
	defer r.mu.Unlock()

	if limit > len(r.tradeLog) {
		limit = len(r.tradeLog)
	}
	result := make([]TradeLogEntry, limit)
	copy(result, r.tradeLog[len(r.tradeLog)-limit:])
	return result
}

// MetricsJSON returns current metrics as JSON (for HTTP endpoint)
func MetricsJSON(m EngineMetrics) []byte {
	data, err := json.Marshal(m)
	if err != nil {
		return []byte("{}")
	}
	return data
}
