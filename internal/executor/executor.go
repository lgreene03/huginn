// Package executor wires together the strategy, portfolio, and simulated
// fill engine to process incoming feature events end-to-end.
package executor

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/lgreene/huginn/internal/journal"
	"github.com/lgreene/huginn/internal/metrics"
	"github.com/lgreene/huginn/internal/model"
	"github.com/lgreene/huginn/internal/portfolio"
	"github.com/lgreene/huginn/internal/risk"
	"github.com/lgreene/huginn/internal/strategy"
)

// IntentPublisher defines a gateway client capable of transmitting order requests.
type IntentPublisher interface {
	PublishIntent(ctx context.Context, order model.Order, orderID string) error
}

// Config holds execution simulation parameters.
type Config struct {
	// TransactionCostBps is the simulated fee in basis points per fill.
	TransactionCostBps float64

	// SlippageBps is the simulated slippage in basis points per fill.
	SlippageBps float64
}

// Executor receives feature events, delegates to a strategy, simulates fills,
// and applies them to the portfolio.
type Executor struct {
	strategy      strategy.Strategy
	portfolio     *portfolio.Portfolio
	journalWriter journal.Writer
	riskManager   *risk.Manager
	config        Config
	fillCount     int
	liveMode      bool
	publisher     IntentPublisher
}

// New creates an executor wiring a strategy to a portfolio.
func New(strat strategy.Strategy, port *portfolio.Portfolio, jw journal.Writer, rm *risk.Manager, cfg Config, liveMode bool, pub IntentPublisher) *Executor {
	return &Executor{
		strategy:      strat,
		portfolio:     port,
		journalWriter: jw,
		riskManager:   rm,
		config:        cfg,
		liveMode:      liveMode,
		publisher:     pub,
	}
}

// OnFeature is the main event handler. It is called by the Kafka consumer
// for each deserialized FeatureEvent.
func (e *Executor) OnFeature(event model.FeatureEvent) {
	orders := e.strategy.OnFeature(event)
	for _, order := range orders {
		metrics.OrdersGeneratedTotal.WithLabelValues(e.strategy.Name(), order.Side.String()).Inc()
	}

	for _, order := range orders {
		if e.liveMode {
			e.fillCount++
			orderID := fmt.Sprintf("huginn-live-order-%d-%d", time.Now().UnixNano(), e.fillCount)

			// Risk check: evaluate risk limits using a prospective fill
			prospectiveFill := model.Fill{
				OrderID:         orderID,
				Instrument:      order.Instrument,
				Side:            order.Side,
				Quantity:        order.Quantity,
				FillPrice:       e.estimatePrice(event),
				TransactionCost: 0.0,
				SlippageBps:     0.0,
				Timestamp:       event.EventTime,
			}

			if e.riskManager != nil && !e.riskManager.Evaluate(prospectiveFill, e.portfolio.Snapshot()) {
				slog.Warn("Outbound live order intent rejected by pre-trade risk manager", "order_id", orderID, "instrument", order.Instrument)
				continue
			}

			if e.publisher != nil {
				// Publish order intent to Kafka for Sleipnir
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				if err := e.publisher.PublishIntent(ctx, order, orderID); err != nil {
					slog.Error("Failed to publish live order intent to gateway", "error", err, "order_id", orderID)
				}
				cancel()
			} else {
				slog.Warn("Live mode enabled but no intent publisher is configured", "order_id", orderID)
			}

		} else {
			// Simulated paper-trading mode
			fill := e.simulateFill(order, event)

			if e.riskManager != nil && !e.riskManager.Evaluate(fill, e.portfolio.Snapshot()) {
				slog.Warn("Fill rejected by risk manager", "order_id", fill.OrderID)
				continue
			}
			
			if e.journalWriter != nil {
				if err := e.journalWriter.Append(fill); err != nil {
					slog.Error("Failed to journal fill", "error", err, "order_id", fill.OrderID)
				}
			}

			e.portfolio.ApplyFill(fill)
			metrics.FillsExecutedTotal.WithLabelValues(fill.Side.String()).Inc()

			// Update portfolio gauges
			snap := e.portfolio.Snapshot()
			metrics.PortfolioCash.Set(snap.Cash)
			metrics.PortfolioRealizedPnL.Set(snap.RealizedPnL)
			metrics.PortfolioUnrealizedPnL.Set(snap.UnrealizedPnL)
			metrics.PortfolioTotalValue.Set(snap.TotalValue)

			slog.Info("Paper trade executed",
				"strategy", e.strategy.Name(),
				"order_side", order.Side.String(),
				"order_qty", fmt.Sprintf("%.8f", order.Quantity),
				"fill_price", fmt.Sprintf("%.2f", fill.FillPrice),
				"slippage_bps", fmt.Sprintf("%.2f", fill.SlippageBps),
				"tx_cost", fmt.Sprintf("%.4f", fill.TransactionCost),
				"reason", order.Reason,
			)
		}
	}

	if len(orders) > 0 && !e.liveMode {
		snap := e.portfolio.Snapshot()
		metrics.PortfolioCash.Set(snap.Cash)
		metrics.PortfolioRealizedPnL.Set(snap.RealizedPnL)
		metrics.PortfolioTotalValue.Set(snap.TotalValue)
	}
}

// OnExecutionFill handles live fills received asynchronously from Sleipnir.
func (e *Executor) OnExecutionFill(fill model.Fill) {
	if e.journalWriter != nil {
		if err := e.journalWriter.Append(fill); err != nil {
			slog.Error("Failed to journal execution fill", "error", err, "order_id", fill.OrderID)
		}
	}

	e.portfolio.ApplyFill(fill)
	metrics.FillsExecutedTotal.WithLabelValues(fill.Side.String()).Inc()

	slog.Info("Live execution fill applied to portfolio",
		"order_id", fill.OrderID,
		"instrument", fill.Instrument,
		"side", fill.Side.String(),
		"qty", fmt.Sprintf("%.8f", fill.Quantity),
		"price", fmt.Sprintf("%.2f", fill.FillPrice),
		"tx_cost", fmt.Sprintf("%.4f", fill.TransactionCost),
	)

	snap := e.portfolio.Snapshot()
	metrics.PortfolioCash.Set(snap.Cash)
	metrics.PortfolioRealizedPnL.Set(snap.RealizedPnL)
	metrics.PortfolioTotalValue.Set(snap.TotalValue)
}

// simulateFill models execution with configurable slippage and transaction costs.
func (e *Executor) simulateFill(order model.Order, event model.FeatureEvent) model.Fill {
	e.fillCount++

	// Use microPrice if available for more realistic fill simulation,
	// otherwise fall back to mid-price estimation from OBI.
	basePrice := e.estimatePrice(event)

	// Apply slippage: buys fill higher, sells fill lower
	slippageMultiplier := e.config.SlippageBps / 10_000.0
	var fillPrice float64
	switch order.Side {
	case model.Buy:
		fillPrice = basePrice * (1 + slippageMultiplier)
	case model.Sell:
		fillPrice = basePrice * (1 - slippageMultiplier)
	}

	txCost := fillPrice * order.Quantity * (e.config.TransactionCostBps / 10_000.0)

	return model.Fill{
		OrderID:         fmt.Sprintf("huginn-fill-%d", e.fillCount),
		Instrument:      order.Instrument,
		Side:            order.Side,
		Quantity:        order.Quantity,
		FillPrice:       fillPrice,
		TransactionCost: txCost,
		SlippageBps:     e.config.SlippageBps,
		Timestamp:       event.EventTime,
	}
}

// estimatePrice extracts the best available price from the feature event.
func (e *Executor) estimatePrice(event model.FeatureEvent) float64 {
	if mp, ok := event.Values["microPrice"]; ok && mp > 0 {
		return mp
	}
	if vwap, ok := event.Values["value"]; ok && vwap > 0 {
		return vwap
	}
	// Fallback: use a sentinel that will be obviously wrong in logs
	return 0.0
}

// PrintSummary logs a final portfolio summary.
func (e *Executor) PrintSummary() {
	snap := e.portfolio.Snapshot()
	slog.Info("═══ Huginn Session Summary ═══",
		"strategy", e.strategy.Name(),
		"total_fills", snap.TotalFills,
		"total_tx_costs", fmt.Sprintf("%.4f", snap.TotalCosts),
		"realized_pnl", fmt.Sprintf("%.4f", snap.RealizedPnL),
		"unrealized_pnl", fmt.Sprintf("%.4f", snap.UnrealizedPnL),
		"cash", fmt.Sprintf("%.2f", snap.Cash),
		"total_value", fmt.Sprintf("%.2f", snap.TotalValue),
	)

	for inst, pos := range snap.Positions {
		slog.Info("Position",
			"instrument", inst,
			"quantity", fmt.Sprintf("%.8f", pos.Quantity),
			"avg_cost", fmt.Sprintf("%.2f", pos.AverageCost),
			"unrealized_pnl", fmt.Sprintf("%.4f", pos.UnrealizedPnL),
		)
	}
}
