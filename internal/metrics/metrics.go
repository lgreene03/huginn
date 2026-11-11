// Package metrics defines the Prometheus instrumentation huginn exposes
// on /metrics. The Phase 3 of docs/ROADMAP.md added the operational
// gauges/histograms below (orders rejected by reason, feature event age,
// signal-to-fill latency, halt status). Counter/gauge nomenclature
// follows Prometheus convention: *_total for counters, no suffix for
// gauges, *_seconds for histograms with time units.
package metrics

import (
	"time"

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

	// OrdersCostSuppressedTotal counts candidate entries blocked by the
	// net-of-cost signal gate (quant-alpha-1) because their expected edge did
	// not clear K * round-trip cost. Labelled by strategy and side. Stays at 0
	// while COST_HURDLE_K == 0 (the inert default).
	OrdersCostSuppressedTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "huginn_orders_cost_suppressed_total",
			Help: "Total candidate entries suppressed by the net-of-cost signal gate",
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
	// FeatureEventTimeInFutureTotal counts feature events whose own timestamp is
	// ahead of huginn's clock. Such an event is excluded from the latency and
	// age histograms below, where a negative value would corrupt _sum (and so
	// every mean) while hiding inside the lowest bucket. A rising count means a
	// producer is mis-stamping events; it does not affect the staleness
	// watchdog, which measures arrival time.
	FeatureEventTimeInFutureTotal = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "huginn_feature_event_time_in_future_total",
			Help: "Feature events whose own timestamp is ahead of huginn's clock",
		},
	)

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

	// ─── Resilience / data-ops additions ────────────────────────────────

	// ConsumerPanicsTotal counts panics recovered in a consumer's per-message
	// handler dispatch, labeled by which consumer loop caught it. A non-zero
	// value means a handler threw but the consumer goroutine survived (instead
	// of dying and causing a silent total trading outage). Any increment is an
	// alertable bug in the handler.
	ConsumerPanicsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "huginn_consumer_panics_total",
			Help: "Panics recovered in consumer handler dispatch (consumer survived)",
		},
		[]string{"consumer"}, // feature | fills | price
	)

	// DeserializeFailedTotal counts messages dropped because their payload
	// failed to deserialize, labeled by consumer. A spike means an upstream
	// producer changed its wire format or is emitting corrupt frames; the
	// message is skipped (offset still advances) rather than silently lost
	// without a trace.
	DeserializeFailedTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "huginn_deserialize_failed_total",
			Help: "Messages dropped because their payload failed to deserialize",
		},
		[]string{"consumer"}, // feature | fills | price
	)

	// ─── Maker/taker execution additions (quant-alpha-2) ────────────────

	// MakerTakerFillsTotal counts simulated paper fills by their liquidity
	// classification. A maker fill rested at the touch and paid the maker
	// fee/rebate; a taker fill crossed the spread and paid the taker fee.
	// Divide maker by the sum to get the realized maker-fill rate. Stays
	// entirely on the "taker" series while no order requests Maker liquidity.
	MakerTakerFillsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "huginn_maker_taker_fills_total",
			Help: "Simulated fills by liquidity classification (maker rested, taker crossed)",
		},
		[]string{"liquidity"}, // maker | taker
	)

	// MakerFillRate is the running fraction of simulated fills that were maker
	// (passive) fills, in [0,1]. 0 means every fill crossed the spread (the
	// default when no maker orders are requested); higher means more spread
	// captured. Computed in-process from the maker/taker counters so a single
	// gauge is dashboard-ready without a PromQL ratio.
	MakerFillRate = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "huginn_maker_fill_rate",
			Help: "Fraction of simulated fills that were maker (passive) fills, in [0,1]",
		},
	)

	// ─── Pluggable alpha framework additions (quant-alpha-3) ─────────────

	// AlphaContribution is the most recent weighted contribution (weight *
	// value, optionally * confidence) of each alpha to a CompositeStrategy's
	// combined score, labelled by the composite strategy name and the alpha
	// name. This makes every alpha's pull on the blended signal observable, so
	// an operator can see which signal is driving (or dragging) the composite.
	// Set on every feature event the composite processes; stays absent until a
	// composite strategy runs.
	AlphaContribution = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "huginn_alpha_contribution",
			Help: "Most recent weighted contribution of each alpha to the composite score",
		},
		[]string{"strategy", "alpha"},
	)

	// CompositeScore is the most recent combined score a CompositeStrategy
	// produced (after weighting/blending and clamping to [-1,1]), labelled by
	// the composite strategy name. Pair with AlphaContribution to attribute the
	// blended signal to its parts.
	CompositeScore = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "huginn_composite_score",
			Help: "Most recent combined score produced by a composite strategy, in [-1,1]",
		},
		[]string{"strategy"},
	)

	// ─── Idleness instrumentation (G0.8) ────────────────────────────────

	// EntriesBlockedTotal counts entries stopped before they became an order
	// intent, labelled by the FIRST gate that stopped them. The label set is
	// closed: it is exactly BlockReasons, and every series is created at zero
	// on start so a dashboard can tell "never fired" from "not exported".
	// Without it a strategy that has stopped trading is indistinguishable from
	// one that has nothing to say. See BlockReason for each reason's meaning.
	//
	// The strategy-level reasons are recorded by CompositeStrategy, the
	// strategy norse-stack runs. The executor records risk_rejected for any
	// strategy; at that point it cannot tell an entry from an exit, which only
	// matters for strategies that emit exits (the composite does not).
	EntriesBlockedTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "huginn_entries_blocked_total",
			Help: "Entries stopped before becoming an order intent, by the first gate that stopped them (below_threshold means no candidate)",
		},
		[]string{"reason"},
	)

	// LastOrderIntentTimestampSeconds is the wall-clock Unix time at which
	// huginn last issued an order intent: published to the gateway in live
	// mode, or past the risk check (or parked as a resting maker) in paper
	// mode. It is 0 until the first intent after the process starts, so 0
	// means "none since start", never "just now"; compare it with
	// process_start_time_seconds.
	LastOrderIntentTimestampSeconds = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "huginn_last_order_intent_timestamp_seconds",
			Help: "Unix time of the last order intent huginn issued; 0 if none since the process started",
		},
	)
)

// BlockReason is the closed set of reasons an entry can be stopped. Using a
// named type keeps the reason label bounded: only these constants reach
// EntriesBlockedTotal.
type BlockReason string

// The reasons, in the order the gates run. The first five are decided inside
// the strategy; risk_rejected is decided by the executor afterwards.
const (
	// BlockBelowThreshold: the signal did not clear the entry band, so there
	// was no candidate entry at all. Counted so that "no signal" can be told
	// apart from "a signal that was refused".
	BlockBelowThreshold BlockReason = "below_threshold"
	// BlockPositionOpen: the instrument already holds the one position the
	// strategy allows per instrument.
	BlockPositionOpen BlockReason = "position_open"
	// BlockMaxPosition: the signed net-position cap is already reached in the
	// signal's direction.
	BlockMaxPosition BlockReason = "max_position"
	// BlockZeroSize: the computed order quantity was not positive.
	BlockZeroSize BlockReason = "zero_size"
	// BlockCostHurdle: the net-of-cost gate judged the expected edge too small
	// for the round-trip cost.
	BlockCostHurdle BlockReason = "cost_hurdle"
	// BlockRiskRejected: the executor's risk manager refused the prospective
	// fill. huginn_orders_rejected_total carries the risk manager's own reason.
	BlockRiskRejected BlockReason = "risk_rejected"
)

// BlockReasons lists every BlockReason in gate order. It is the complete
// label set of EntriesBlockedTotal.
var BlockReasons = []BlockReason{
	BlockBelowThreshold,
	BlockPositionOpen,
	BlockMaxPosition,
	BlockZeroSize,
	BlockCostHurdle,
	BlockRiskRejected,
}

func init() {
	// Create every reason's series at zero so the full, bounded label set is
	// exported from the first scrape.
	for _, r := range BlockReasons {
		EntriesBlockedTotal.WithLabelValues(string(r))
	}
}

// EntryBlocked records one entry stopped for the given reason.
func EntryBlocked(r BlockReason) {
	EntriesBlockedTotal.WithLabelValues(string(r)).Inc()
}

// OrderIntentIssued records the wall-clock time of an order intent.
func OrderIntentIssued(at time.Time) {
	LastOrderIntentTimestampSeconds.Set(float64(at.Unix()) + float64(at.Nanosecond())/1e9)
}
