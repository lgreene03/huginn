package backtest

import (
	"bufio"
	"encoding/json"
	"log/slog"
	"os"
	"time"

	"github.com/lgreene/huginn/internal/executor"
	"github.com/lgreene/huginn/internal/journal"
	"github.com/lgreene/huginn/internal/model"
	"github.com/lgreene/huginn/internal/portfolio"
	"github.com/lgreene/huginn/internal/risk"
)

// Engine is a high-performance historical replayer.
type Engine struct {
	exec    *executor.Executor
	port    *portfolio.Portfolio
	journal journal.Writer
	riskMgr *risk.Manager
	equity  []float64
}

// NewEngine initializes the backtest engine with all required execution components.
func NewEngine(exec *executor.Executor, port *portfolio.Portfolio, jw journal.Writer, rm *risk.Manager) *Engine {
	return &Engine{
		exec:    exec,
		port:    port,
		journal: jw,
		riskMgr: rm,
		equity:  make([]float64, 0),
	}
}

// Run executes the backtest by sequentially processing a JSONL file of FeatureEvents.
func (e *Engine) Run(dataPath string) error {
	f, err := os.Open(dataPath)
	if err != nil {
		return err
	}
	defer f.Close()

	slog.Info("Starting backtest", "data", dataPath)
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

		// Dispatch directly to the executor, bypassing Kafka entirely
		e.exec.OnFeature(event)
		eventsProcessed++

		// Track daily equity curve for advanced metrics (Sharpe/Sortino)
		day := event.EventTime.YearDay()
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
