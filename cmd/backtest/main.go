package main

import (
	"bufio"
	"encoding/json"
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
	"github.com/lgreene03/huginn/internal/model"
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
	defer func() { _ = jWriter.Close() }()

	// Initialize risk manager
	riskManager := risk.NewManager(cfg.Risk, cfg.Capital.InitialCash)

	// Initialize executor
	exec := executor.New(activeStrategy, port, jWriter, riskManager, executor.Config{
		TransactionCostBps:  cfg.Executor.TransactionCostBps,
		SlippageBps:         cfg.Executor.SlippageBps,
		SlippageImpactK:     cfg.Executor.SlippageImpactK,
		SlippageImpactScale: cfg.Executor.SlippageImpactScale,
		FillLatencyMs:       cfg.Executor.FillLatencyMs,
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

	// Cost-aware reporting: gross vs net. NetPnL = GrossPnL − fees − slippage is
	// the true objective — a strategy can have a real gross edge yet bleed it all
	// to costs by over-trading. NetSharpe is the Sharpe of the (net-of-cost)
	// equity curve the engine sampled.
	cost := metrics.ComputeCostBreakdown(fills)
	netSharpe := metrics.NetSharpe(equity, 0.0)

	// Buy-and-hold benchmark over the same window. Re-load the event stream
	// (cheap second pass) so we can mark an equal-notional basket of every
	// priceable instrument to market and compare the strategy against simply
	// buying and holding. A load failure must not fail the backtest itself, so
	// we degrade to "no benchmark" on error.
	strategyReturn := backtest.StrategyTotalReturn(snap.TotalValue, cfg.Capital.InitialCash)
	var bench backtest.Benchmark
	var benchValid bool
	if benchEvents, berr := loadEvents(*dataPath); berr != nil {
		slog.Warn("Could not load events for buy-and-hold benchmark", "error", berr)
	} else {
		bench = backtest.BenchmarkBuyHold(benchEvents, cfg.Capital.InitialCash)
		benchValid = bench.Instruments > 0
	}
	excess := strategyReturn - bench.TotalReturn
	infoRatio := backtest.InformationRatio(equity, bench)

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
	fmt.Println("─── vs Buy-and-Hold ───")
	if benchValid {
		fmt.Printf("Strategy Return: %+.2f%%\n", strategyReturn*100)
		fmt.Printf("Buy-Hold Return: %+.2f%%  (%d instrument(s))\n", bench.TotalReturn*100, bench.Instruments)
		fmt.Printf("Excess Return:   %+.2f%%\n", excess*100)
		fmt.Printf("Info Ratio:      %.4f\n", infoRatio)
	} else {
		fmt.Println("Buy-Hold Return: n/a (no priceable instruments in stream)")
	}
	fmt.Println("─── Net of Costs ───")
	fmt.Printf("Gross PnL:       %+.4f\n", cost.GrossPnL)
	fmt.Printf("Fees:            %.4f\n", cost.Fees)
	fmt.Printf("Slippage:        %.4f\n", cost.Slippage)
	fmt.Printf("Net PnL:         %+.4f  (gross − fees − slippage)\n", cost.NetPnL)
	fmt.Printf("Net Sharpe:      %.4f\n", netSharpe)
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

			BenchmarkValid:       benchValid,
			BenchmarkInstruments: bench.Instruments,
			StrategyTotalReturn:  strategyReturn,
			BenchmarkTotalReturn: bench.TotalReturn,
			ExcessReturn:         excess,
			InformationRatio:     infoRatio,
		}
		if err := backtest.GenerateHTMLReport(params, *reportPath); err != nil {
			slog.Error("Failed to write HTML report", "error", err)
			os.Exit(1)
		}
		fmt.Printf("Report written to %s\n", *reportPath)
	}
}

// loadEvents reads a FeatureEvent JSONL file into memory for the buy-and-hold
// benchmark pass. Malformed lines are skipped (matching the engine's lenient
// replay), so a single bad row never aborts the benchmark.
func loadEvents(path string) ([]model.FeatureEvent, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	var events []model.FeatureEvent
	scanner := bufio.NewScanner(f)
	buf := make([]byte, 0, 1024*1024)
	scanner.Buffer(buf, 10*1024*1024)
	for scanner.Scan() {
		var ev model.FeatureEvent
		if err := json.Unmarshal(scanner.Bytes(), &ev); err != nil {
			continue
		}
		events = append(events, ev)
	}
	return events, scanner.Err()
}
