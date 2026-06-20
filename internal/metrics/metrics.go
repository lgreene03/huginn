// Package metrics defines the Prometheus instrumentation huginn exposes
// on /metrics. The Phase 3 of docs/ROADMAP.md added the operational
// gauges/histograms below (orders rejected by reason, feature event age,
// signal-to-fill latency, halt status). Counter/gauge nomenclature
// follows Prometheus convention: *_total for counters, no suffix for
// gauges, *_seconds for histograms with time units.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	// FeaturesConsumedTotal counts total number of feature events ingested from Kafka.
	FeaturesConsumedTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "huginn_features_consumed_total",
			Help: "Total number of feature events consumed from Muninn",
		},
		[]string{"feature"},
	)

	// OrdersGeneratedTotal counts total generated orders by strategy and side.
	OrdersGeneratedTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "huginn_orders_generated_total",
			Help: "Total number of paper orders generated",
		},
		[]string{"strategy", "side"},
	)

	// FillsExecutedTotal counts total simulated/live fills applied.
	FillsExecutedTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "huginn_fills_executed_total",
			Help: "Total number of fills executed",
		},
		[]string{"side"},
	)

	// PortfolioCash tracks the current cash balance.
	PortfolioCash = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "huginn_portfolio_cash",
			Help: "Current cash balance of the portfolio",
		},
	)

	// PortfolioRealizedPnL tracks cumulative realized profit and loss.
	PortfolioRealizedPnL = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "huginn_portfolio_realized_pnl",
			Help: "Cumulative realized PnL",
		},
	)

	// PortfolioUnrealizedPnL tracks current unrealized profit and loss.
	PortfolioUnrealizedPnL = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "huginn_portfolio_unrealized_pnl",
			Help: "Current unrealized PnL based on last fill prices",
		},
	)

	// PortfolioTotalValue tracks cash + unrealized PnL.
	PortfolioTotalValue = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "huginn_portfolio_total_value",
			Help: "Total portfolio value (cash + unrealized PnL)",
		},
	)

	// ─── Phase 3 additions ──────────────────────────────────────────────

	// OrdersRejectedTotal counts prospective fills rejected by the risk
	// manager, labeled by the typed reason. Use this to spot a runaway
	// strategy: a spike in `position_limit` rejections is the canary
	// before the trailing stop trips.
	OrdersRejectedTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "huginn_orders_rejected_total",
			Help: "Total prospective fills rejected by the risk manager",
		},
		[]string{"reason"}, // halted / drawdown / daily_loss / position_limit / staleness
	)

	// FeatureEventAgeSeconds is the wall-clock delay between the feature
	// event's EventTime and when huginn dispatched it. A creeping p95 here
	// means Muninn → Kafka → huginn is falling behind; a step change means
	// the feed broke or huginn is throttled.
	FeatureEventAgeSeconds = promauto.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "huginn_feature_event_age_seconds",
			Help:    "Wall-clock delay between feature event time and dispatch",
			Buckets: prometheus.ExponentialBuckets(0.001, 4, 9), // 1ms … 65s
		},
	)

	// SignalToDecisionMs tracks end-to-end latency from bridge signal
	// creation to huginn strategy dispatch, in milliseconds.
	SignalToDecisionMs = promauto.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "huginn_signal_to_decision_ms",
			Help:    "Bridge signal creation to huginn receipt latency (ms)",
			Buckets: prometheus.ExponentialBuckets(1, 2, 12), // 1ms … 4s
		},
	)

	// SignalToFillLatencySeconds is the wall-clock delay between strategy
	// signal and fill application. For paper mode this is small (in-process);
	// for live mode it captures the round-trip to sleipnir.
	SignalToFillLatencySeconds = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "huginn_signal_to_fill_latency_seconds",
			Help:    "Wall-clock delay from strategy signal to fill application",
			Buckets: prometheus.ExponentialBuckets(0.0001, 4, 9), // 100µs … 6.5s
		},
		[]string{"mode"}, // paper | live
	)

	// StrategyStatePersistedTotal counts successful state-journal writes.
	// Pair with StrategyStatePersistErrorsTotal to compute the success ratio.
	StrategyStatePersistedTotal = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "huginn_strategy_state_persisted_total",
			Help: "Successful strategy/risk state persists via the journal",
		},
	)

	// StrategyStatePersistErrorsTotal counts failed state-journal writes.
	StrategyStatePersistErrorsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "huginn_strategy_state_persist_errors_total",
			Help: "Failed strategy/risk state persists via the journal",
		},
		[]string{"kind"}, // marshal | write
	)

	// RiskHaltActive is 1 when the risk manager has trading halted (any
	// reason), 0 otherwise. Combine with RiskHaltReason via PromQL `info`
	// joins to render the reason on dashboards.
	RiskHaltActive = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "huginn_risk_halt_active",
			Help: "1 when trading is halted by the risk manager, else 0",
		},
	)

	// RiskHaltReason emits an info-style gauge with the reason label.
	// Always exactly one series is non-zero (or none, if not halted).
	RiskHaltReason = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "huginn_risk_halt_reason",
			Help: "Info-style: which halt reason is currently active (manual|drawdown|feature_staleness)",
		},
		[]string{"reason"},
	)

	// ─── Phase 8 additions (SSE feature stream) ─────────────────────────

	// FeatureStreamConnected is 1 while the SSE feed source holds an open
	// connection to muninn's /api/v1/features/stream, 0 while disconnected
	// (including during backoff between reconnect attempts). Mirrors
	// sleipnir's sleipnir_ws_connected gauge. Only meaningful when the
	// feed source is "stream"; stays 0 in the default Kafka path.
	FeatureStreamConnected = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "huginn_feature_stream_connected",
			Help: "1 while the SSE feature stream is connected, else 0",
		},
	)

	// FeatureStreamReconnectsTotal counts how many times the SSE feed source
	// re-established its connection after a drop. A climbing value means the
	// stream is flapping (proxy timeout, muninn restart, network); pair with
	// FeatureStreamConnected to distinguish "flapping" from "down".
	FeatureStreamReconnectsTotal = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "huginn_feature_stream_reconnects_total",
			Help: "Total successful SSE feature-stream reconnections after a drop",
		},
	)
)
