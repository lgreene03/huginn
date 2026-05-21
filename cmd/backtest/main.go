package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/lgreene03/huginn/internal/backtest"
	"github.com/lgreene03/huginn/internal/config"
	"github.com/lgreene03/huginn/internal/executor"
	"github.com/lgreene03/huginn/internal/journal"
	"github.com/lgreene03/huginn/internal/metrics"
	"github.com/lgreene03/huginn/internal/portfolio"
	"github.com/lgreene03/huginn/internal/risk"
	"github.com/lgreene03/huginn/internal/strategy"
)

const journalPath = "data/backtest_trades.jsonl"

func main() {
	configPath := flag.String("config", "configs/default.yaml", "Path to YAML config file")
	dataPath := flag.String("data", "", "Path to historical FeatureEvent JSONL data file")
	reportPath := flag.String("report", "", "Optional path for self-contained HTML report (e.g. report.html)")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	if *dataPath == "" {
		slog.Error("Missing --data flag. A historical JSONL file is required for backtesting.")
		os.Exit(1)
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		slog.Error("Failed to load configuration", "error", err)
		os.Exit(1)
	}

	// Initialize fresh portfolio
	port := portfolio.New(cfg.Capital.InitialCash)

	// Select strategy
	var activeStrategy strategy.Strategy
	switch cfg.Strategy.Name {
	case "obi":
		activeStrategy = strategy.NewOBIThreshold(cfg.Strategy.Threshold, cfg.Strategy.OrderSize, cfg.Strategy.OrderSize*10)
	case "vpin":
		activeStrategy = strategy.NewVPINBreakout(cfg.Strategy.Threshold, cfg.Strategy.OrderSize, time.Minute)
	case "vwap_deviation":
		activeStrategy = strategy.NewVWAPDeviation(cfg.Strategy.Threshold, cfg.Strategy.OrderSize, cfg.Strategy.OrderSize*10)
	case "ema_crossover":
		activeStrategy = strategy.NewEMACrossover(cfg.Strategy.FastPeriod, cfg.Strategy.SlowPeriod, cfg.Strategy.OrderSize, cfg.Strategy.OrderSize*10)
	default:
		slog.Error("Unknown strategy", "strategy", cfg.Strategy.Name)
		os.Exit(1)
	}

	// Initialize journal writer for backtest output
	jWriter, err := journal.NewJSONLWriter(journalPath)
	if err != nil {
		slog.Error("Failed to initialize journal writer", "error", err)
		os.Exit(1)
	}
	defer jWriter.Close()

	// Initialize risk manager
	riskManager := risk.NewManager(cfg.Risk, cfg.Capital.InitialCash)

	// Initialize executor
	exec := executor.New(activeStrategy, port, jWriter, riskManager, executor.Config{
		TransactionCostBps: cfg.Executor.TransactionCostBps,
		SlippageBps:        cfg.Executor.SlippageBps,
		FillLatencyMs:      cfg.Executor.FillLatencyMs,
	}, false, nil, "") // empty key — backtest doesn't persist strategy state

	// Initialize and run backtest engine
	engine := backtest.NewEngine(exec, port, jWriter, riskManager)
	if err := engine.Run(*dataPath); err != nil {
		slog.Error("Backtest failed", "error", err)
		os.Exit(1)
	}

	// Collect results
	snap := engine.FinalSnapshot()
	equity := engine.EquityCurve()
	sharpe := metrics.CalculateSharpeRatio(equity, 0.0)
	mdd := metrics.CalculateMaxDrawdown(equity)

	// Read fills from the journal for extended metrics and the optional report.
	fills, err := journal.ReadFills(journalPath)
	if err != nil {
		slog.Warn("Could not read fills from journal for extended metrics", "error", err)
	}

	hitRate := metrics.HitRate(fills)
	turnover := metrics.Turnover(fills)
	avgHold := metrics.AvgHoldTimeSeconds(fills)

	// Print terminal summary
	fmt.Println("\n═══ Backtest Summary ═══")
	fmt.Printf("Strategy:        %s\n", activeStrategy.Name())
	fmt.Printf("Initial Cash:    %.2f\n", cfg.Capital.InitialCash)
	fmt.Printf("Final Value:     %.2f\n", snap.TotalValue)
	fmt.Printf("Realized PnL:    %.4f\n", snap.RealizedPnL)
	fmt.Printf("Total Fills:     %d\n", snap.TotalFills)
	fmt.Printf("Max Drawdown:    %.2f%%\n", mdd*100)
	fmt.Printf("Sharpe Ratio:    %.4f\n", sharpe)
	fmt.Printf("Hit Rate:        %.1f%%\n", hitRate*100)
	fmt.Printf("Turnover:        %.2fx\n", turnover)
	fmt.Printf("Avg Hold:        %.0fs\n", avgHold)
	fmt.Println("════════════════════════")

	// Optionally generate an HTML report.
	if *reportPath != "" {
		params := backtest.ReportParams{
			Strategy:    activeStrategy.Name(),
			ConfigPath:  *configPath,
			DataPath:    *dataPath,
			InitialCash: cfg.Capital.InitialCash,
			FinalValue:  snap.TotalValue,
			RealizedPnL: snap.RealizedPnL,
			TotalFills:  snap.TotalFills,
			MaxDrawdown: mdd,
			Sharpe:      sharpe,
			HitRate:     hitRate,
			Turnover:    turnover,
			AvgHoldSec:  avgHold,
			Equity:      equity,
			Fills:       fills,
			GeneratedAt: time.Now().UTC(),
		}
		if err := backtest.GenerateHTMLReport(params, *reportPath); err != nil {
			slog.Error("Failed to write HTML report", "error", err)
			os.Exit(1)
		}
		fmt.Printf("Report written to %s\n", *reportPath)
	}
}
