package modules

import (
	"encoding/json"
	"log"
	"sync"
	"time"

	"mm-platform-engine/internal/core"
)

// ReportingConfig contains configuration for reporting
type ReportingConfig struct {
	MetricsIntervalSec  int    `json:"metrics_interval_sec" bson:"metrics_interval_sec"`
	DailySummaryHourUTC int    `json:"daily_summary_hour_utc" bson:"daily_summary_hour_utc"`
	WeeklySummaryDay    string `json:"weekly_summary_day" bson:"weekly_summary_day"`
}

// EngineMetrics is the realtime metrics snapshot emitted each tick
type EngineMetrics struct {
	Timestamp     time.Time `json:"timestamp"`
	Mode          core.Mode `json:"mode"`
	Mid           float64   `json:"mid"`
	AvgSpreadBps  float64   `json:"avg_spread_bps"`
	DepthNotional float64   `json:"depth_notional"`
	NumBidOrders  int       `json:"num_bid_orders"`
	NumAskOrders  int       `json:"num_ask_orders"`
	InvRatio      float64   `json:"inv_ratio"`
	InvDeviation  float64   `json:"inv_deviation"`
	SkewBps       float64   `json:"skew_bps"`
	SizeTilt      float64   `json:"size_tilt"`
	Drawdown24h   float64   `json:"drawdown_24h"`
	FillsPerMin   float64   `json:"fills_per_min"`
	CancelPerMin  float64   `json:"cancel_per_min"`
	QuoteUptime   float64   `json:"quote_uptime"`
	RealizedVol   float64   `json:"realized_vol"`
	FillRate      float64   `json:"fill_rate"`
	BaseValue     float64   `json:"base_value"`
	QuoteValue    float64   `json:"quote_value"`
	NAV           float64   `json:"nav"`
}

// DailySummary is emitted once per day
type DailySummary struct {
	Date             string                `json:"date"`
	Symbol           string                `json:"symbol"`
	AvgSpreadBps     float64               `json:"avg_spread_bps"`
	AvgDepthNotional float64               `json:"avg_depth_notional"`
	TotalFills       int                   `json:"total_fills"`
	TotalVolume      float64               `json:"total_volume"`
	MaxDrawdown      float64               `json:"max_drawdown"`
	QuoteUptime      float64               `json:"quote_uptime"`
	StartNAV         float64               `json:"start_nav"`
	EndNAV           float64               `json:"end_nav"`
	InvRatioAvg      float64               `json:"inv_ratio_avg"`
	TimeInModes      map[core.Mode]float64 `json:"time_in_modes"`
}

// TradeLogEntry is a sanitized fill record for external reporting
type TradeLogEntry struct {
	Timestamp time.Time `json:"timestamp"`
	Symbol    string    `json:"symbol"`
	Side      string    `json:"side"`
	Price     float64   `json:"price"`
	Qty       float64   `json:"qty"`
	Notional  float64   `json:"notional"`
	Fee       float64   `json:"fee"`
	FeeAsset  string    `json:"fee_asset"`
	TradeID   string    `json:"trade_id"`
}

// Reporter emits structured metrics, summaries, and sanitized trade logs.
type Reporter struct {
	mu  sync.Mutex
	cfg *ReportingConfig

	// Accumulation for summaries
	tickMetrics      []EngineMetrics
	tradeLog         []TradeLogEntry
	lastDailySummary time.Time

	// Callbacks
	OnMetrics      func(EngineMetrics)
	OnDailySummary func(DailySummary)
	OnAlert        func(level, message string)
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

	if len(r.tickMetrics) > 8640 {
		r.tickMetrics = r.tickMetrics[len(r.tickMetrics)-8640:]
	}

	if r.OnMetrics != nil {
		r.OnMetrics(m)
	}

	log.Printf("[METRICS] mode=%s mid=%.6f spread=%.1fbps depth=$%.0f inv=%.2f%% dd=%.2f%% fills=%.1f/min uptime=%.1f%%",
		m.Mode, m.Mid, m.AvgSpreadBps, m.DepthNotional,
		m.InvRatio*100, m.Drawdown24h*100,
		m.FillsPerMin, m.QuoteUptime*100)

	r.checkDailySummary(m)
}

// RecordTrade records a sanitized trade
func (r *Reporter) RecordTrade(entry TradeLogEntry) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.tradeLog = append(r.tradeLog, entry)

	if len(r.tradeLog) > 10000 {
		r.tradeLog = r.tradeLog[len(r.tradeLog)-10000:]
	}
}

// Alert sends an alert
func (r *Reporter) Alert(level, message string) {
	log.Printf("[ALERT:%s] %s", level, message)
	if r.OnAlert != nil {
		r.OnAlert(level, message)
	}
}

func (r *Reporter) checkDailySummary(current EngineMetrics) {
	now := current.Timestamp
	summaryHour := r.cfg.DailySummaryHourUTC

	todaySummaryTime := time.Date(now.Year(), now.Month(), now.Day(),
		summaryHour, 0, 0, 0, time.UTC)

	if now.After(todaySummaryTime) && r.lastDailySummary.Before(todaySummaryTime) {
		r.generateDailySummary(todaySummaryTime)
		r.lastDailySummary = now
	}
}

func (r *Reporter) generateDailySummary(date time.Time) {
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
		TotalFills:       len(r.tradeLog),
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

// GetTradeLog returns sanitized trade log entries
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

// MetricsJSON returns current metrics as JSON
func MetricsJSON(m EngineMetrics) []byte {
	data, err := json.Marshal(m)
	if err != nil {
		return []byte("{}")
	}
	return data
}
