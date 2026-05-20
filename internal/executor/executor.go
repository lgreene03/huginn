// Package executor wires together the strategy, portfolio, and simulated
// fill engine to process incoming feature events end-to-end.
package executor

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/lgreene03/huginn/internal/journal"
	"github.com/lgreene03/huginn/internal/metrics"
	"github.com/lgreene03/huginn/internal/model"
	"github.com/lgreene03/huginn/internal/portfolio"
	"github.com/lgreene03/huginn/internal/risk"
	"github.com/lgreene03/huginn/internal/strategy"
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
	strategyKey   string // stable identifier for state persistence (e.g. "obi")
	portfolio     *portfolio.Portfolio
	journalWriter journal.Writer
	riskManager   *risk.Manager
	config        Config
	fillCount     int
	liveMode      bool
	publisher     IntentPublisher
	dedup         *dedupCache // drops duplicate Sleipnir fills by ExecutionID
}

// New creates an executor wiring a strategy to a portfolio.
//
// strategyKey is a stable identifier (typically the config strategy name like
// "obi" / "vpin") used to key state persistence. Pass an empty string to
// disable state persistence — useful in tests.
func New(strat strategy.Strategy, port *portfolio.Portfolio, jw journal.Writer, rm *risk.Manager, cfg Config, liveMode bool, pub IntentPublisher, strategyKey string) *Executor {
	return &Executor{
		strategy:      strat,
		strategyKey:   strategyKey,
		portfolio:     port,
		journalWriter: jw,
		riskManager:   rm,
		config:        cfg,
		liveMode:      liveMode,
		publisher:     pub,
		dedup:         newDedupCache(10_000),
	}
}

// PersistStrategyState marshals the strategy's current state (if Stateful) and
// writes it via the journal. Errors are logged and counted but do not
// propagate — a journal hiccup must not crash the engine.
//
// Safe to call concurrently; the strategy holds its own mutex internally and
// the journal writer is itself thread-safe.
func (e *Executor) PersistStrategyState() {
	if e.journalWriter == nil {
		return
	}
	if e.strategyKey != "" {
		if sf, ok := e.strategy.(strategy.Stateful); ok {
			blob, err := sf.MarshalState()
			if err != nil {
				slog.Error("Failed to marshal strategy state",
					"strategy", e.strategy.Name(), "error", err)
			} else if err := e.journalWriter.AppendStrategyState(e.strategyKey, blob); err != nil {
				slog.Error("Failed to persist strategy state",
					"strategy_key", e.strategyKey, "error", err)
			}
		}
	}
	// Risk-manager state (peakValue, dayStart, lastFeatureEvent) rides on
	// the same persist cycle under a fixed "_risk" key. Co-located so a
	// single ticker covers both; co-resolved on boot in cmd/huginn/main.go.
	if e.riskManager != nil {
		blob, err := e.riskManager.MarshalState()
		if err != nil {
			slog.Error("Failed to marshal risk state", "error", err)
		} else if err := e.journalWriter.AppendStrategyState(riskStateKey, blob); err != nil {
			slog.Error("Failed to persist risk state", "error", err)
		}
	}
}

// riskStateKey is the fixed journal key for the risk manager's persisted
// state. Reserved — strategies must not use a name starting with underscore.
const riskStateKey = "_risk"

// RiskStateKey is exported for the boot path to load risk state by the same
// key the executor writes it under.
func RiskStateKey() string { return riskStateKey }

// RunStatePersister runs a coalescing ticker that calls PersistStrategyState
// every interval. EMA-style strategies mutate continuously between fills and
// would otherwise lose their accumulator on a crash. Cancel via ctx.
//
// Fires even with an empty strategyKey because the risk manager's state
// (peakValue, lastFeatureEventTime) also needs the ticker — it changes on
// every event, not only on fills.
func (e *Executor) RunStatePersister(ctx context.Context, interval time.Duration) {
	if e.journalWriter == nil {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			// One last persist on graceful shutdown — best effort.
			e.PersistStrategyState()
			return
		case <-ticker.C:
			e.PersistStrategyState()
		}
	}
}

// OnFeature is the main event handler. It is called by the Kafka consumer
// for each deserialized FeatureEvent.
func (e *Executor) OnFeature(event model.FeatureEvent) {
	// Notify the risk manager that a fresh event arrived — feeds the
	// staleness watchdog and the auto-resume-from-staleness path.
	if e.riskManager != nil {
		e.riskManager.OnFeatureSeen(event.EventTime)
	}

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

			// Persist strategy state alongside the fill: position-bearing state
			// (OBI/VPIN/VWAP.netPosition, VPIN.lastTrade) only changes on fills.
			e.PersistStrategyState()

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
//
// Sleipnir's boot reconciliation can re-emit a fill the live WebSocket path
// already delivered; without dedup we would double-count quantity, cash, and
// realized PnL. The bounded LRU keyed on Fill.ExecutionID drops those repeats.
// Empty ExecutionID (paper mode, or pre-Phase-5 fills) is never deduplicated.
func (e *Executor) OnExecutionFill(fill model.Fill) {
	if e.dedup.Seen(fill.ExecutionID) {
		slog.Warn("Dropping duplicate execution fill",
			"order_id", fill.OrderID,
			"execution_id", fill.ExecutionID,
			"qty", fill.Quantity,
		)
		return
	}
	e.dedup.Mark(fill.ExecutionID)

	if e.journalWriter != nil {
		if err := e.journalWriter.Append(fill); err != nil {
			slog.Error("Failed to journal execution fill", "error", err, "order_id", fill.OrderID)
		}
	}

	e.portfolio.ApplyFill(fill)
	metrics.FillsExecutedTotal.WithLabelValues(fill.Side.String()).Inc()

	// Persist strategy state on every live fill, same as in paper mode.
	e.PersistStrategyState()

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

// SystemConfig represents the parameters of the active strategy and risk manager.
type SystemConfig struct {
	StrategyName      string  `json:"strategy_name"`
	Threshold         float64 `json:"threshold"`
	OrderSize         float64 `json:"order_size"`
	FastPeriod        int     `json:"fast_period"`
	SlowPeriod        int     `json:"slow_period"`
	PositionLimitHard float64 `json:"position_limit_hard"`
}

// GetConfig retrieves the current active parameter configuration.
func (e *Executor) GetConfig() SystemConfig {
	sc := SystemConfig{
		StrategyName: e.strategy.Name(),
	}

	// Dynamic strategy casting to read values
	switch s := e.strategy.(type) {
	case *strategy.OBIThreshold:
		sc.Threshold = s.Threshold
		sc.OrderSize = s.OrderSize
	case *strategy.VPINBreakout:
		sc.Threshold = s.Threshold
		sc.OrderSize = s.OrderSize
	case *strategy.VWAPDeviation:
		sc.Threshold = s.ThresholdPct
		sc.OrderSize = s.OrderSize
	case *strategy.EMACrossover:
		sc.OrderSize = s.OrderSize
		sc.FastPeriod = s.FastPeriod
		sc.SlowPeriod = s.SlowPeriod
	}

	if e.riskManager != nil {
		sc.PositionLimitHard = e.riskManager.GetPositionLimitHard()
	}

	return sc
}

// UpdateConfig updates the strategy and risk parameters on the fly.
func (e *Executor) UpdateConfig(sc SystemConfig) {
	switch s := e.strategy.(type) {
	case *strategy.OBIThreshold:
		if sc.Threshold > 0 {
			s.Threshold = sc.Threshold
		}
		if sc.OrderSize > 0 {
			s.OrderSize = sc.OrderSize
		}
	case *strategy.VPINBreakout:
		if sc.Threshold > 0 {
			s.Threshold = sc.Threshold
		}
		if sc.OrderSize > 0 {
			s.OrderSize = sc.OrderSize
		}
	case *strategy.VWAPDeviation:
		if sc.Threshold > 0 {
			s.ThresholdPct = sc.Threshold
		}
		if sc.OrderSize > 0 {
			s.OrderSize = sc.OrderSize
		}
	case *strategy.EMACrossover:
		if sc.OrderSize > 0 {
			s.OrderSize = sc.OrderSize
		}
		if sc.FastPeriod > 0 {
			s.FastPeriod = sc.FastPeriod
		}
		if sc.SlowPeriod > 0 {
			s.SlowPeriod = sc.SlowPeriod
		}
	}

	if e.riskManager != nil && sc.PositionLimitHard > 0 {
		e.riskManager.UpdateLimits(sc.PositionLimitHard)
	}

	slog.Info("System parameters updated dynamically",
		"strategy", e.strategy.Name(),
		"threshold", sc.Threshold,
		"order_size", sc.OrderSize,
		"fast_period", sc.FastPeriod,
		"slow_period", sc.SlowPeriod,
		"position_limit", sc.PositionLimitHard,
	)
}
