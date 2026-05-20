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
)
