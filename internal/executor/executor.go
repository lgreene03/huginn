// Package executor wires together the strategy, portfolio, and simulated
// fill engine to process incoming feature events end-to-end.
package executor

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"time"

	"go.opentelemetry.io/otel/attribute"

	"github.com/lgreene03/huginn/internal/journal"
	"github.com/lgreene03/huginn/internal/metrics"
	"github.com/lgreene03/huginn/internal/model"
	"github.com/lgreene03/huginn/internal/portfolio"
	"github.com/lgreene03/huginn/internal/risk"
	"github.com/lgreene03/huginn/internal/strategy"
	"github.com/lgreene03/huginn/internal/tracing"
)

// IntentPublisher defines a gateway client capable of transmitting order requests.
type IntentPublisher interface {
	PublishIntent(ctx context.Context, order model.Order, orderID string) error
}

// Config holds execution simulation parameters.
type Config struct {
	// TransactionCostBps is the simulated fee in basis points per fill.
	TransactionCostBps float64

	// SlippageBps is the simulated slippage in basis points per fill. This is
	// the base (size-independent) component of the slippage model.
	SlippageBps float64

	// SlippageImpactK is the coefficient of the optional square-root market
	// impact term. The effective slippage in bps is:
	//
	//	effective_slip_bps = SlippageBps + SlippageImpactK * sqrt(qty / SlippageImpactScale)
	//
	// Zero (default) disables the impact term entirely, so the model collapses
	// to the original flat-constant SlippageBps behaviour. Positive values make
	// slippage grow with order size, modelling the price impact of consuming
	// successively worse levels of the book. Exposed in config so calibrate can
	// sweep it.
	SlippageImpactK float64

	// SlippageImpactScale normalises order quantity inside the square-root
	// impact term (the order size at which the impact term contributes exactly
	// SlippageImpactK bps). Only consulted when SlippageImpactK > 0. Must be
	// > 0 when the impact term is active; a non-positive value falls back to 1.0.
	SlippageImpactScale float64

	// FillLatencyMs defers the fill timestamp by this many milliseconds in
	// paper-trading mode. Zero (default) uses the raw event timestamp.
	// Positive values model realistic signal-to-fill delays in backtests.
	FillLatencyMs int64

	// Sizing is the OPT-IN equity-aware position-sizing mode (quant-4). The
	// zero value (strategy.SizingFixed) preserves the historical behaviour —
	// every order ships at the strategy's own OrderSize, unchanged. The kelly /
	// inverse-vol modes rescale each emitted order's quantity from account
	// equity *after* the strategy returns it, so strategies stay untouched.
	Sizing strategy.SizingMode

	// SizingKellyFraction is the Kelly fraction of equity to allocate when
	// Sizing == SizingKelly. Static here (no live win-rate tracking yet); a
	// caller can derive it via strategy.KellyFraction(winRate, avgWin, avgLoss)
	// from offline stats. Zero falls back to the strategy's OrderSize.
	SizingKellyFraction float64

	// SizingVolTarget is the per-position volatility budget for inverse-vol
	// sizing (target_notional = VolTarget/volatility * equity). Only consulted
	// when Sizing == SizingInverseVol and the event carries a volatility
	// feature. Zero disables the mode (falls back to OrderSize).
	SizingVolTarget float64

	// SizingMaxNotionalFraction caps any sized order at this fraction of equity
	// (e.g. 0.25 = never above 25% of equity in one order). Zero disables the cap.
	SizingMaxNotionalFraction float64

	// MakerFeeBps / TakerFeeBps are the per-fill fees (basis points) for maker
	// (passive, rests at the touch) and taker (aggressive, crosses the spread)
	// liquidity respectively (quant-alpha-2). A maker fee may be negative to
	// model a rebate. Both default to TransactionCostBps, so unless they are
	// explicitly configured the cost of any fill is exactly the legacy
	// TransactionCostBps regardless of liquidity — preserving existing numbers.
	//
	// See feeBps for the resolution rule (zero ⇒ fall back to
	// TransactionCostBps; an explicit negative maker rebate is honoured).
	MakerFeeBps float64
	TakerFeeBps float64
}

// feeBps returns the transaction-cost basis points to charge for a fill of the
// given liquidity. The per-liquidity MakerFeeBps/TakerFeeBps override
// TransactionCostBps only when set; a zero value falls back to
// TransactionCostBps so the legacy single-fee behaviour is unchanged unless the
// maker/taker fees are configured.
//
// A negative MakerFeeBps is a deliberate rebate and is honoured (it is not
// treated as "unset"); only an exactly-zero value falls back.
func (c Config) feeBps(liq model.Liquidity) float64 {
	switch liq {
	case model.Maker:
		if c.MakerFeeBps != 0 {
			return c.MakerFeeBps
		}
	default: // Taker
		if c.TakerFeeBps != 0 {
			return c.TakerFeeBps
		}
	}
	return c.TransactionCostBps
}

// effectiveSlippageBps returns the slippage in basis points for an order of the
// given quantity. With SlippageImpactK == 0 (the default) this is exactly the
// flat config.SlippageBps — preserving the original constant-slippage model.
// With a positive impact coefficient it adds a square-root market-impact term
// k*sqrt(qty/scale), so larger orders incur more slippage.
func (c Config) effectiveSlippageBps(qty float64) float64 {
	if c.SlippageImpactK <= 0 {
		return c.SlippageBps
	}
	scale := c.SlippageImpactScale
	if scale <= 0 {
		scale = 1.0
	}
	return c.SlippageBps + c.SlippageImpactK*math.Sqrt(math.Abs(qty)/scale)
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

	// restingMakers holds maker orders parked at the touch in paper mode,
	// awaiting a subsequent through-trade (quant-alpha-2). Each new feature
	// event is checked against these before the strategy runs; a maker fills
	// only if the new mid trades through its resting price (adverse-selection
	// model). Always empty unless a strategy emits Maker-liquidity orders, so
	// the default taker path never touches this slice.
	restingMakers []restingMaker

	// makerFills / takerFills count applied paper fills by liquidity, driving
	// the maker-fill-rate gauge. Both start at zero; takerFills tracks the
	// existing taker path so the rate is well-defined from the first fill.
	makerFills int64
	takerFills int64
}

// restingMaker is a maker order parked at the touch awaiting a through-trade.
// restPrice is the passive level (bid for a buy, ask for a sell). The order
// fills on a later event whose mid trades through restPrice in the order's
// favour direction.
type restingMaker struct {
	order     model.Order
	restPrice float64
}

// New creates an executor wiring a strategy to a portfolio.
//
// strategyKey is a stable identifier (typically the config strategy name like
// "obi" / "vpin") used to key state persistence. Pass an empty string to
// disable state persistence — useful in tests.
func New(s strategy.Strategy, port *portfolio.Portfolio, jw journal.Writer, rm *risk.Manager, cfg Config, liveMode bool, pub IntentPublisher, strategyKey string) *Executor {
	return &Executor{
		strategy:      s,
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

// Strategy returns the active strategy. It is read-only and exists so the HTTP
// server can expose live strategy introspection (e.g. /api/alphas) without the
// server package reaching into executor internals. The returned value shares
// the executor's strategy instance; callers must only invoke its own
// concurrency-safe methods (Name, AlphaSnapshot), never mutate it.
func (e *Executor) Strategy() strategy.Strategy {
	return e.strategy
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
				metrics.StrategyStatePersistErrorsTotal.WithLabelValues("marshal").Inc()
			} else if err := e.journalWriter.AppendStrategyState(e.strategyKey, blob); err != nil {
				slog.Error("Failed to persist strategy state",
					"strategy_key", e.strategyKey, "error", err)
				metrics.StrategyStatePersistErrorsTotal.WithLabelValues("write").Inc()
			} else {
				metrics.StrategyStatePersistedTotal.Inc()
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
			metrics.StrategyStatePersistErrorsTotal.WithLabelValues("marshal").Inc()
		} else if err := e.journalWriter.AppendStrategyState(riskStateKey, blob); err != nil {
			slog.Error("Failed to persist risk state", "error", err)
			metrics.StrategyStatePersistErrorsTotal.WithLabelValues("write").Inc()
		} else {
			metrics.StrategyStatePersistedTotal.Inc()
		}

		// Daily PnL snapshot (Postgres only): upsert today's closing numbers as a
		// human-readable fallback for risk recovery if the _risk blob is lost.
		if pw, ok := e.journalWriter.(*journal.PostgresWriter); ok {
			snap := e.portfolio.Snapshot()
			if err := pw.AppendDailyPnLSnapshot(
				snap.RealizedPnL,
				snap.TotalValue,
				e.riskManager.PeakValue(),
				e.riskManager.DayStartRealizedPnL(),
			); err != nil {
				slog.Error("Failed to persist daily PnL snapshot", "error", err)
			}
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
	// Root span for the per-event lifecycle. If live mode publishes an
	// intent, `PublishIntent` injects this span's TraceContext into the
	// Kafka headers — sleipnir's gateway extracts it and the response leg
	// continues here via `OnExecutionFill`.
	ctx, span := tracing.StartSpan(context.Background(), "executor.on_feature",
		attribute.String("event_id", event.EventID),
		attribute.String("feature", event.FeatureName),
		attribute.String("instrument", event.Instrument),
	)
	defer span.End()

	// Observability: wall-clock age of this feature event at the moment it
	// reaches huginn. Histogram in `huginn_feature_event_age_seconds`.
	if !event.EventTime.IsZero() {
		metrics.FeatureEventAgeSeconds.Observe(time.Since(event.EventTime).Seconds())
	}

	// End-to-end latency: bridge signal creation → huginn receipt
	if event.SignalTimeMs > 0 {
		bridgeToHuginnMs := time.Now().UnixMilli() - event.SignalTimeMs
		metrics.SignalToDecisionMs.Observe(float64(bridgeToHuginnMs))
	}

	// Notify the risk manager that a fresh event arrived — feeds the
	// staleness watchdog and the auto-resume-from-staleness path. The market
	// volatility feature (when present) drives the vol-scaled position limit
	// (quant-14); absent it, the manager falls back to fill-price dispersion.
	if e.riskManager != nil {
		e.riskManager.OnFeatureSeen(event.EventTime, event.Values["volatility"])
	}

	// Maker/taker (quant-alpha-2): before the strategy runs, test any resting
	// maker orders against this event's mid. A maker fills only when the market
	// trades through its passive level (adverse-selection model). No-op while
	// no maker orders are outstanding, so the taker default is untouched.
	if !e.liveMode && len(e.restingMakers) > 0 {
		e.tryFillRestingMakers(event)
	}

	orders := e.strategy.OnFeature(event)

	// Opt-in equity-aware sizing (quant-4). Default SizingFixed is a no-op, so
	// orders keep the strategy's own OrderSize unless a sizing mode is configured.
	e.applySizing(orders, event)

	for _, order := range orders {
		metrics.OrdersGeneratedTotal.WithLabelValues(e.strategy.Name(), order.Side.String()).Inc()
	}

	// Signal arrived at this point. We record fill latency where each branch
	// applies the fill (paper) / publishes the intent (live).
	signalTime := time.Now()

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
				// Publish order intent to Kafka for Sleipnir. Use the OnFeature
				// span's ctx (not context.Background) so the producer's
				// InjectKafkaHeaders writes the trace parent into the message.
				publishCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
				if err := e.publisher.PublishIntent(publishCtx, order, orderID); err != nil {
					slog.Error("Failed to publish live order intent to gateway", "error", err, "order_id", orderID)
				}
				cancel()
				// Latency for live mode covers the signal → intent-publish leg.
				// The signal → fill leg is observed separately on the inbound
				// path via OnExecutionFill.
				metrics.SignalToFillLatencySeconds.WithLabelValues("live").Observe(time.Since(signalTime).Seconds())
			} else {
				slog.Warn("Live mode enabled but no intent publisher is configured", "order_id", orderID)
			}

		} else {
			// Simulated paper-trading mode.
			//
			// Maker liquidity (quant-alpha-2): park the order at the touch and
			// wait for a subsequent through-trade rather than crossing now. The
			// taker default (order.Liquidity == model.Taker) keeps the original
			// cross-the-spread path below, so existing numbers are unchanged.
			if order.Liquidity == model.Maker {
				if rest, ok := e.restPriceForMaker(order, event); ok {
					e.restingMakers = append(e.restingMakers, restingMaker{order: order, restPrice: rest})
					continue
				}
				// No book/mid price to rest against: degrade to a taker fill so
				// the order is never silently dropped.
			}

			fill := e.simulateFill(order, event)

			if e.riskManager != nil && !e.riskManager.Evaluate(fill, e.portfolio.Snapshot()) {
				slog.Warn("Fill rejected by risk manager", "order_id", fill.OrderID)
				continue
			}

			e.applyPaperFill(fill)
			metrics.SignalToFillLatencySeconds.WithLabelValues("paper").Observe(time.Since(signalTime).Seconds())

			slog.Debug("Paper trade executed",
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
//
// The ctx parameter carries the trace context sleipnir threaded through on
// the fills topic — every operation here (dedup → journal → portfolio
// apply) attaches to the same span tree the original intent began on.
func (e *Executor) OnExecutionFill(ctx context.Context, fill model.Fill) {
	// ineffassign suppression: the returned ctx isn't passed to anything
	// downstream right now (journal.Append + portfolio.ApplyFill are both
	// ctx-free), but we keep the span open so its attributes + duration get
	// captured. Discarding with _ keeps the linter happy.
	_, span := tracing.StartSpan(ctx, "executor.on_execution_fill",
		attribute.String("order_id", fill.OrderID),
		attribute.String("execution_id", fill.ExecutionID),
		attribute.Float64("quantity", fill.Quantity),
	)
	defer span.End()

	if e.dedup.Seen(fill.ExecutionID) {
		slog.Warn("Dropping duplicate execution fill",
			"order_id", fill.OrderID,
			"execution_id", fill.ExecutionID,
			"qty", fill.Quantity,
		)
		span.SetAttributes(attribute.Bool("dedup_hit", true))
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

// applySizing rescales each order's quantity in place according to the
// configured sizing mode (quant-4). With Config.Sizing == strategy.SizingFixed
// (the default) it is a no-op, so the strategy's own OrderSize is preserved.
//
// Equity comes from the live portfolio snapshot; the reference price is the
// order's limit price when set, else the event's estimated price. Any
// degenerate input makes SizeOrder fall back to the original quantity, so this
// can never zero out or invert an order.
func (e *Executor) applySizing(orders []model.Order, event model.FeatureEvent) {
	if e.config.Sizing == strategy.SizingFixed || len(orders) == 0 {
		return
	}
	equity := e.portfolio.Snapshot().TotalValue
	vol := event.Values["volatility"]
	for i := range orders {
		price := orders[i].LimitPrice
		if price <= 0 {
			price = e.estimatePrice(event)
		}
		newQty := strategy.SizeOrder(e.config.Sizing, strategy.SizingParams{
			BaseQty:             orders[i].Quantity,
			Equity:              equity,
			Price:               price,
			Volatility:          vol,
			KellyFraction:       e.config.SizingKellyFraction,
			VolTarget:           e.config.SizingVolTarget,
			MaxNotionalFraction: e.config.SizingMaxNotionalFraction,
		})
		orders[i].Quantity = newQty
	}
}

// simulateFill models a TAKER execution: cross the spread now, with
// configurable slippage, transaction costs, an optional order-book-aware fill
// price, and an optional latency offset. Maker orders are not filled here —
// they rest at the touch and fill later via tryFillRestingMakers.
func (e *Executor) simulateFill(order model.Order, event model.FeatureEvent) model.Fill {
	e.fillCount++

	// Size-dependent slippage: base bps plus an optional square-root market
	// impact term (disabled by default; see Config.effectiveSlippageBps).
	effectiveSlipBps := e.config.effectiveSlippageBps(order.Quantity)
	slippageMultiplier := effectiveSlipBps / 10_000.0

	// Order-book-aware pricing: when the feature event carries bid/ask from
	// features.book.v1, fill buys at the ask touch and sells at the bid touch.
	// Falls back to estimatePrice (microPrice / VWAP value) when not present.
	var fillPrice float64
	switch order.Side {
	case model.Buy:
		if ask, ok := event.Values["askPrice"]; ok && ask > 0 {
			fillPrice = ask * (1 + slippageMultiplier)
		} else {
			fillPrice = e.estimatePrice(event) * (1 + slippageMultiplier)
		}
	case model.Sell:
		if bid, ok := event.Values["bidPrice"]; ok && bid > 0 {
			fillPrice = bid * (1 - slippageMultiplier)
		} else {
			fillPrice = e.estimatePrice(event) * (1 - slippageMultiplier)
		}
	}

	txCost := fillPrice * order.Quantity * (e.config.feeBps(model.Taker) / 10_000.0)

	// Latency model: defers the fill timestamp to simulate signal-to-fill delay.
	// Zero means use the raw event timestamp (original behaviour).
	fillTime := event.EventTime
	if e.config.FillLatencyMs > 0 {
		fillTime = fillTime.Add(time.Duration(e.config.FillLatencyMs) * time.Millisecond)
	}

	return model.Fill{
		OrderID:         fmt.Sprintf("huginn-fill-%d", e.fillCount),
		Instrument:      order.Instrument,
		Side:            order.Side,
		Quantity:        order.Quantity,
		FillPrice:       fillPrice,
		TransactionCost: txCost,
		SlippageBps:     effectiveSlipBps,
		Timestamp:       fillTime,
		Liquidity:       model.Taker,
	}
}

// restPriceForMaker returns the passive resting price for a maker order on the
// given event: the bid touch for a buy, the ask touch for a sell. It falls back
// to the estimated mid price when the book quote is absent, and returns
// (0, false) when no price can be derived (so the order is treated as a taker).
func (e *Executor) restPriceForMaker(order model.Order, event model.FeatureEvent) (float64, bool) {
	switch order.Side {
	case model.Buy:
		if bid, ok := event.Values["bidPrice"]; ok && bid > 0 {
			return bid, true
		}
	case model.Sell:
		if ask, ok := event.Values["askPrice"]; ok && ask > 0 {
			return ask, true
		}
	}
	if mid := e.estimatePrice(event); mid > 0 {
		return mid, true
	}
	return 0, false
}

// eventMid returns the mid price used to test whether a resting maker order was
// traded through. Prefers an explicit midPrice, then the bid/ask average, then
// the estimated price. Returns (0, false) when no price is available.
func eventMid(e *Executor, event model.FeatureEvent) (float64, bool) {
	if mid, ok := event.Values["midPrice"]; ok && mid > 0 {
		return mid, true
	}
	bid, hasBid := event.Values["bidPrice"]
	ask, hasAsk := event.Values["askPrice"]
	if hasBid && hasAsk && bid > 0 && ask > 0 {
		return (bid + ask) / 2, true
	}
	if mid := e.estimatePrice(event); mid > 0 {
		return mid, true
	}
	return 0, false
}

// makerFill builds the Fill for a resting maker order that was traded through.
// It fills exactly at the passive resting price (no spread crossed, no
// slippage) and pays the maker fee/rebate. The fill is timestamped on the
// through-trade event (plus any configured latency).
func (e *Executor) makerFill(rm restingMaker, event model.FeatureEvent) model.Fill {
	e.fillCount++
	txCost := rm.restPrice * rm.order.Quantity * (e.config.feeBps(model.Maker) / 10_000.0)

	fillTime := event.EventTime
	if e.config.FillLatencyMs > 0 {
		fillTime = fillTime.Add(time.Duration(e.config.FillLatencyMs) * time.Millisecond)
	}

	return model.Fill{
		OrderID:         fmt.Sprintf("huginn-fill-%d", e.fillCount),
		Instrument:      rm.order.Instrument,
		Side:            rm.order.Side,
		Quantity:        rm.order.Quantity,
		FillPrice:       rm.restPrice,
		TransactionCost: txCost,
		SlippageBps:     0, // maker rests at the touch; no spread crossed
		Timestamp:       fillTime,
		Liquidity:       model.Maker,
	}
}

// recordLiquidity bumps the maker/taker counters and recomputes the maker
// fill-rate gauge. Called once per applied paper fill.
func (e *Executor) recordLiquidity(liq model.Liquidity) {
	if liq == model.Maker {
		e.makerFills++
	} else {
		e.takerFills++
	}
	metrics.MakerTakerFillsTotal.WithLabelValues(liqLabel(liq)).Inc()
	if total := e.makerFills + e.takerFills; total > 0 {
		metrics.MakerFillRate.Set(float64(e.makerFills) / float64(total))
	}
}

func liqLabel(liq model.Liquidity) string {
	if liq == model.Maker {
		return "maker"
	}
	return "taker"
}

// applyPaperFill journals, applies, and instruments a single paper-mode fill.
// Shared by the immediate taker path and the deferred resting-maker path so
// both record liquidity, persist state, and refresh the portfolio gauges
// identically.
func (e *Executor) applyPaperFill(fill model.Fill) {
	if e.journalWriter != nil {
		if err := e.journalWriter.Append(fill); err != nil {
			slog.Error("Failed to journal fill", "error", err, "order_id", fill.OrderID)
		}
	}

	e.portfolio.ApplyFill(fill)
	metrics.FillsExecutedTotal.WithLabelValues(fill.Side.String()).Inc()
	e.recordLiquidity(fill.Liquidity)

	// Persist strategy state alongside the fill: position-bearing state
	// (OBI/VPIN/VWAP.netPosition, VPIN.lastTrade) only changes on fills.
	e.PersistStrategyState()

	snap := e.portfolio.Snapshot()
	metrics.PortfolioCash.Set(snap.Cash)
	metrics.PortfolioRealizedPnL.Set(snap.RealizedPnL)
	metrics.PortfolioUnrealizedPnL.Set(snap.UnrealizedPnL)
	metrics.PortfolioTotalValue.Set(snap.TotalValue)
}

// tryFillRestingMakers tests every parked maker order against the current
// event's mid and fills those the market traded through (quant-alpha-2). A buy
// resting at the bid fills once the mid drops to or below its level (the ask
// came down to it); a sell resting at the ask fills once the mid rises to or
// above its level. Orders not yet traded through stay parked for the next event.
//
// This models adverse selection: a passive order only executes when price moves
// against the resting side, which is exactly when a taker would have done
// better — the price the maker pays for the spread capture / rebate.
func (e *Executor) tryFillRestingMakers(event model.FeatureEvent) {
	mid, ok := eventMid(e, event)
	if !ok {
		return // no price to test through; keep everything parked
	}

	remaining := e.restingMakers[:0]
	for _, rm := range e.restingMakers {
		tradedThrough := false
		switch rm.order.Side {
		case model.Buy:
			tradedThrough = mid <= rm.restPrice
		case model.Sell:
			tradedThrough = mid >= rm.restPrice
		}
		if !tradedThrough {
			remaining = append(remaining, rm)
			continue
		}

		fill := e.makerFill(rm, event)
		if e.riskManager != nil && !e.riskManager.Evaluate(fill, e.portfolio.Snapshot()) {
			slog.Warn("Maker fill rejected by risk manager", "order_id", fill.OrderID)
			continue // drop it; do not re-park a risk-rejected order
		}
		e.applyPaperFill(fill)
		slog.Debug("Maker order filled on through-trade",
			"strategy", e.strategy.Name(),
			"order_side", rm.order.Side.String(),
			"rest_price", fmt.Sprintf("%.2f", rm.restPrice),
			"mid", fmt.Sprintf("%.2f", mid),
		)
	}
	e.restingMakers = remaining
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
