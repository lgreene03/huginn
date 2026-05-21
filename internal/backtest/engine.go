package backtest

import (
	"bufio"
	"encoding/json"
	"log/slog"
	"os"
	"time"

	"github.com/lgreene03/huginn/internal/executor"
	"github.com/lgreene03/huginn/internal/journal"
	"github.com/lgreene03/huginn/internal/model"
	"github.com/lgreene03/huginn/internal/portfolio"
	"github.com/lgreene03/huginn/internal/risk"
)

// Engine is a high-performance historical replayer.
//
// Multi-strategy support: create the engine with NewEngine for a single
// strategy, then call AddExecutor for each additional strategy. All executors
// share the portfolio and risk manager that were passed to NewEngine, so every
// strategy's fills are applied to the same book and counted against the same
// risk limits.
type Engine struct {
	execs   []*executor.Executor
	port    *portfolio.Portfolio
	journal journal.Writer
	riskMgr *risk.Manager
	equity  []float64
}

// NewEngine initializes the backtest engine with all required execution components.
// To run multiple strategies simultaneously, call AddExecutor after construction.
func NewEngine(exec *executor.Executor, port *portfolio.Portfolio, jw journal.Writer, rm *risk.Manager) *Engine {
	return &Engine{
		execs:   []*executor.Executor{exec},
		port:    port,
		journal: jw,
		riskMgr: rm,
		equity:  make([]float64, 0),
	}
}

// AddExecutor registers an additional strategy executor that will receive every
// feature event alongside the primary executor. The added executor must share
// the same portfolio and risk manager that were passed to NewEngine.
func (e *Engine) AddExecutor(exec *executor.Executor) {
	e.execs = append(e.execs, exec)
}

// Run executes the backtest by sequentially processing a JSONL file of FeatureEvents.
func (e *Engine) Run(dataPath string) error {
	f, err := os.Open(dataPath)
	if err != nil {
		return err
	}
	defer f.Close()

	slog.Info("Starting backtest", "data", dataPath, "strategies", len(e.execs))
	start := time.Now()
	var eventsProcessed int

	var lastDay int = -1

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var event model.FeatureEvent
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			slog.Warn("Failed to parse event", "error", err)
			continue
		}

		// Dispatch to every registered executor in registration order.
		// All executors share the same portfolio and risk manager, so
		// later strategies see any position/cash changes from earlier ones
		// within the same event tick.
		for _, exec := range e.execs {
			exec.OnFeature(event)
		}
		eventsProcessed++

		// Track daily equity curve for advanced metrics (Sharpe/Sortino).
		// Use year*1000+YearDay rather than YearDay alone: YearDay wraps
		// at year boundaries (Jan 1 2024 and Jan 1 2025 both return 1),
		// collapsing multi-year backtests into a single equity bucket.
		day := event.EventTime.Year()*1000 + event.EventTime.YearDay()
		if day != lastDay {
			if lastDay != -1 {
				snap := e.port.Snapshot()
				e.equity = append(e.equity, snap.TotalValue)
			}
			lastDay = day
		}
	}

	// Record final equity state
	snap := e.port.Snapshot()
	e.equity = append(e.equity, snap.TotalValue)

	duration := time.Since(start)
	slog.Info("Backtest completed",
		"events", eventsProcessed,
		"duration", duration.String(),
		"eps", float64(eventsProcessed)/duration.Seconds(),
	)

	return scanner.Err()
}

// EquityCurve returns the tracked daily portfolio values.
func (e *Engine) EquityCurve() []float64 {
	return e.equity
}

// FinalSnapshot returns the terminal state of the portfolio.
func (e *Engine) FinalSnapshot() portfolio.Snapshot {
	return e.port.Snapshot()
}
