package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Metrics holds all Prometheus metrics for the MM bot
type Metrics struct {
	// Bot info
	Info *prometheus.GaugeVec

	// NAV and drawdown
	NAV          prometheus.Gauge
	PeakNAV      prometheus.Gauge
	DrawdownPct  prometheus.Gauge
	InventoryPct prometheus.Gauge

	// Order counts
	LiveOrdersTotal   *prometheus.GaugeVec
	OrdersPlacedTotal *prometheus.CounterVec
	OrdersCancelTotal *prometheus.CounterVec

	// Fill metrics
	FillsTotal     *prometheus.CounterVec
	FillVolume     *prometheus.CounterVec
	FillNotional   *prometheus.CounterVec
	LastFillPrice  *prometheus.GaugeVec
	TimeToFillSecs prometheus.Histogram

	// Market data
	MidPrice  prometheus.Gauge
	BestBid   prometheus.Gauge
	BestAsk   prometheus.Gauge
	SpreadBps prometheus.Gauge

	// Error counters
	ErrorsTotal *prometheus.CounterVec

	// Latency
	TickDurationSecs prometheus.Histogram
	APILatencySecs   *prometheus.HistogramVec
}

// NewMetrics creates and registers all Prometheus metrics
func NewMetrics(namespace, exchange, symbol, botID string) *Metrics {
	labels := prometheus.Labels{
		"exchange": exchange,
		"symbol":   symbol,
		"bot_id":   botID,
	}

	m := &Metrics{
		Info: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Namespace:   namespace,
			Name:        "bot_info",
			Help:        "Bot information",
			ConstLabels: labels,
		}, []string{"version", "bot_type"}),

		NAV: promauto.NewGauge(prometheus.GaugeOpts{
			Namespace:   namespace,
			Name:        "nav_usd",
			Help:        "Current Net Asset Value in USD",
			ConstLabels: labels,
		}),

		PeakNAV: promauto.NewGauge(prometheus.GaugeOpts{
			Namespace:   namespace,
			Name:        "peak_nav_usd",
			Help:        "Peak Net Asset Value in USD",
			ConstLabels: labels,
		}),

		DrawdownPct: promauto.NewGauge(prometheus.GaugeOpts{
			Namespace:   namespace,
			Name:        "drawdown_pct",
			Help:        "Current drawdown percentage from peak",
			ConstLabels: labels,
		}),

		InventoryPct: promauto.NewGauge(prometheus.GaugeOpts{
			Namespace:   namespace,
			Name:        "inventory_pct",
			Help:        "Current inventory ratio (base value / total value)",
			ConstLabels: labels,
		}),

		LiveOrdersTotal: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Namespace:   namespace,
			Name:        "live_orders_total",
			Help:        "Number of live orders by side",
			ConstLabels: labels,
		}, []string{"side"}),

		OrdersPlacedTotal: promauto.NewCounterVec(prometheus.CounterOpts{
			Namespace:   namespace,
			Name:        "orders_placed_total",
			Help:        "Total number of orders placed",
			ConstLabels: labels,
		}, []string{"side"}),

		OrdersCancelTotal: promauto.NewCounterVec(prometheus.CounterOpts{
			Namespace:   namespace,
			Name:        "orders_cancelled_total",
			Help:        "Total number of orders cancelled",
			ConstLabels: labels,
		}, []string{"side", "reason"}),

		FillsTotal: promauto.NewCounterVec(prometheus.CounterOpts{
			Namespace:   namespace,
			Name:        "fills_total",
			Help:        "Total number of fills",
			ConstLabels: labels,
		}, []string{"side"}),

		FillVolume: promauto.NewCounterVec(prometheus.CounterOpts{
			Namespace:   namespace,
			Name:        "fill_volume_total",
			Help:        "Total fill volume (base asset)",
			ConstLabels: labels,
		}, []string{"side"}),

		FillNotional: promauto.NewCounterVec(prometheus.CounterOpts{
			Namespace:   namespace,
			Name:        "fill_notional_usd_total",
			Help:        "Total fill notional in USD",
			ConstLabels: labels,
		}, []string{"side"}),

		LastFillPrice: promauto.NewGaugeVec(prometheus.GaugeOpts{
			Namespace:   namespace,
			Name:        "last_fill_price",
			Help:        "Last fill price by side",
			ConstLabels: labels,
		}, []string{"side"}),

		TimeToFillSecs: promauto.NewHistogram(prometheus.HistogramOpts{
			Namespace:   namespace,
			Name:        "time_to_fill_seconds",
			Help:        "Time from order placement to fill",
			ConstLabels: labels,
			Buckets:     []float64{0.1, 0.5, 1, 5, 10, 30, 60, 300, 600},
		}),

		MidPrice: promauto.NewGauge(prometheus.GaugeOpts{
			Namespace:   namespace,
			Name:        "mid_price",
			Help:        "Current mid price",
			ConstLabels: labels,
		}),

		BestBid: promauto.NewGauge(prometheus.GaugeOpts{
			Namespace:   namespace,
			Name:        "best_bid",
			Help:        "Current best bid price",
			ConstLabels: labels,
		}),

		BestAsk: promauto.NewGauge(prometheus.GaugeOpts{
			Namespace:   namespace,
			Name:        "best_ask",
			Help:        "Current best ask price",
			ConstLabels: labels,
		}),

		SpreadBps: promauto.NewGauge(prometheus.GaugeOpts{
			Namespace:   namespace,
			Name:        "spread_bps",
			Help:        "Current spread in basis points",
			ConstLabels: labels,
		}),

		ErrorsTotal: promauto.NewCounterVec(prometheus.CounterOpts{
			Namespace:   namespace,
			Name:        "errors_total",
			Help:        "Total number of errors",
			ConstLabels: labels,
		}, []string{"type"}),

		TickDurationSecs: promauto.NewHistogram(prometheus.HistogramOpts{
			Namespace:   namespace,
			Name:        "tick_duration_seconds",
			Help:        "Duration of each tick cycle",
			ConstLabels: labels,
			Buckets:     []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5},
		}),

		APILatencySecs: promauto.NewHistogramVec(prometheus.HistogramOpts{
			Namespace:   namespace,
			Name:        "api_latency_seconds",
			Help:        "API call latency",
			ConstLabels: labels,
			Buckets:     []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10},
		}, []string{"method"}),
	}

	// Set bot info
	m.Info.WithLabelValues("1.0.0", "simple-maker").Set(1)

	return m
}

// RecordFill records a fill event
func (m *Metrics) RecordFill(side string, price, qty float64) {
	m.FillsTotal.WithLabelValues(side).Inc()
	m.FillVolume.WithLabelValues(side).Add(qty)
	m.FillNotional.WithLabelValues(side).Add(price * qty)
	m.LastFillPrice.WithLabelValues(side).Set(price)
}

// RecordOrderPlaced records an order placement
func (m *Metrics) RecordOrderPlaced(side string) {
	m.OrdersPlacedTotal.WithLabelValues(side).Inc()
}

// RecordOrderCancelled records an order cancellation
func (m *Metrics) RecordOrderCancelled(side, reason string) {
	m.OrdersCancelTotal.WithLabelValues(side, reason).Inc()
}

// RecordError records an error
func (m *Metrics) RecordError(errType string) {
	m.ErrorsTotal.WithLabelValues(errType).Inc()
}

// UpdateMarketData updates market data metrics
func (m *Metrics) UpdateMarketData(mid, bid, ask float64) {
	m.MidPrice.Set(mid)
	m.BestBid.Set(bid)
	m.BestAsk.Set(ask)
	if mid > 0 {
		spreadBps := (ask - bid) / mid * 10000
		m.SpreadBps.Set(spreadBps)
	}
}

// UpdateNAV updates NAV-related metrics
func (m *Metrics) UpdateNAV(nav, peakNAV, drawdownPct, inventoryPct float64) {
	m.NAV.Set(nav)
	m.PeakNAV.Set(peakNAV)
	m.DrawdownPct.Set(drawdownPct * 100) // Convert to percentage
	m.InventoryPct.Set(inventoryPct * 100)
}

// UpdateLiveOrders updates live order counts
func (m *Metrics) UpdateLiveOrders(bidCount, askCount int) {
	m.LiveOrdersTotal.WithLabelValues("BUY").Set(float64(bidCount))
	m.LiveOrdersTotal.WithLabelValues("SELL").Set(float64(askCount))
}
