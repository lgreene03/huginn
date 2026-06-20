package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"math"
	"os"
	"time"

	"github.com/lgreene03/huginn/internal/config"
	"github.com/lgreene03/huginn/internal/executor"
	"github.com/lgreene03/huginn/internal/journal"
	"github.com/lgreene03/huginn/internal/metrics"
	"github.com/lgreene03/huginn/internal/model"
	"github.com/lgreene03/huginn/internal/portfolio"
	"github.com/lgreene03/huginn/internal/risk"
	"github.com/lgreene03/huginn/internal/strategy"
)

// Walk-forward validation: splits historical data into rolling train/test
// windows. For each fold, the strategy is trained/calibrated on the training
// window and evaluated on the out-of-sample test window. This proves the
// strategy generalises and isn't overfit to a specific period.
//
// Window layout (anchored walk-forward):
//
//   Fold 1: [====TRAIN====][==TEST==]
//   Fold 2: [=======TRAIN=======][==TEST==]
//   Fold 3: [==========TRAIN==========][==TEST==]
//
// The training window grows (anchored to start) while the test window
// slides forward. This is the standard approach used by quant firms.

type foldResult struct {
	Fold       int     `json:"fold"`
	TrainStart string  `json:"train_start"`
	TrainEnd   string  `json:"train_end"`
	TestStart  string  `json:"test_start"`
	TestEnd    string  `json:"test_end"`
	TrainPnL   float64 `json:"train_pnl"`
	TestPnL    float64 `json:"test_pnl"`
	TrainFills int     `json:"train_fills"`
	TestFills  int     `json:"test_fills"`
	Sharpe     float64 `json:"sharpe"`
	MaxDD      float64 `json:"max_dd"`
	HitRate    float64 `json:"hit_rate"`
}

func main() {
	configPath := flag.String("config", "configs/default.yaml", "YAML config")
	dataPath := flag.String("data", "", "Historical FeatureEvent JSONL file")
	folds := flag.Int("folds", 5, "Number of walk-forward folds")
	testPct := flag.Float64("test-pct", 0.2, "Fraction of data per test window")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	if *dataPath == "" {
		slog.Error("Missing --data flag")
		os.Exit(1)
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		slog.Error("Config load failed", "error", err)
		os.Exit(1)
	}

	events, err := loadEvents(*dataPath)
	if err != nil {
		slog.Error("Failed to load events", "error", err)
		os.Exit(1)
	}

	if len(events) < 100 {
		slog.Error("Not enough events for walk-forward", "count", len(events))
		os.Exit(1)
	}

	testSize := int(float64(len(events)) * *testPct)
	if testSize < 10 {
		testSize = 10
	}

	fmt.Println("\n═══ Walk-Forward Validation ═══")
	fmt.Printf("Strategy:     %s\n", cfg.Strategy.Name)
	fmt.Printf("Total events: %d\n", len(events))
	fmt.Printf("Folds:        %d\n", *folds)
	fmt.Printf("Test window:  %d events (%.0f%%)\n", testSize, *testPct*100)
	fmt.Println("═══════════════════════════════")

	results := make([]foldResult, 0, *folds)
	var totalOOS float64
	var oosWins int

	step := (len(events) - testSize) / *folds
	if step < testSize {
		step = testSize
	}

	for fold := 0; fold < *folds; fold++ {
		trainEnd := testSize + step*fold
		if trainEnd > len(events)-testSize {
			trainEnd = len(events) - testSize
		}
		testEnd := trainEnd + testSize
		if testEnd > len(events) {
			testEnd = len(events)
		}

		trainEvents := events[:trainEnd]
		testEvents := events[trainEnd:testEnd]

		if len(testEvents) < 5 {
			break
		}

		trainResult := runFold(cfg, trainEvents)
		testResult := runFold(cfg, testEvents)

		r := foldResult{
			Fold:       fold + 1,
			TrainStart: trainEvents[0].EventTime.Format("2006-01-02T15:04"),
			TrainEnd:   trainEvents[len(trainEvents)-1].EventTime.Format("2006-01-02T15:04"),
			TestStart:  testEvents[0].EventTime.Format("2006-01-02T15:04"),
			TestEnd:    testEvents[len(testEvents)-1].EventTime.Format("2006-01-02T15:04"),
			TrainPnL:   trainResult.pnl,
			TestPnL:    testResult.pnl,
			TrainFills: trainResult.fills,
			TestFills:  testResult.fills,
			Sharpe:     testResult.sharpe,
			MaxDD:      testResult.maxDD,
			HitRate:    testResult.hitRate,
		}
		results = append(results, r)
		totalOOS += testResult.pnl

		oosSign := "-"
		if testResult.pnl > 0 {
			oosSign = "+"
			oosWins++
		}

		fmt.Printf("\nFold %d/%d\n", fold+1, *folds)
		fmt.Printf("  Train: %s → %s  (%d events, %d fills, PnL: %.4f)\n",
			r.TrainStart, r.TrainEnd, len(trainEvents), r.TrainFills, r.TrainPnL)
		fmt.Printf("  Test:  %s → %s  (%d events, %d fills, PnL: %s%.4f)\n",
			r.TestStart, r.TestEnd, len(testEvents), r.TestFills, oosSign, math.Abs(r.TestPnL))
		fmt.Printf("  OOS Sharpe: %.4f  MaxDD: %.2f%%  Hit: %.1f%%\n",
			r.Sharpe, r.MaxDD*100, r.HitRate*100)
	}

	fmt.Println("\n═══ Walk-Forward Summary ═══")
	fmt.Printf("OOS folds profitable: %d/%d (%.0f%%)\n",
		oosWins, len(results), float64(oosWins)/float64(len(results))*100)
	fmt.Printf("Total OOS PnL:       %.4f\n", totalOOS)
	fmt.Printf("Avg OOS PnL/fold:    %.4f\n", totalOOS/float64(len(results)))

	avgSharpe := 0.0
	for _, r := range results {
		avgSharpe += r.Sharpe
	}
	avgSharpe /= float64(len(results))
	fmt.Printf("Avg OOS Sharpe:      %.4f\n", avgSharpe)

	if float64(oosWins)/float64(len(results)) >= 0.6 && avgSharpe > 0 {
		fmt.Println("Verdict:             PASS — strategy generalises out-of-sample")
	} else {
		fmt.Println("Verdict:             FAIL — likely overfit to training data")
	}
	fmt.Println("════════════════════════════")

	jsonOut, _ := json.MarshalIndent(results, "", "  ")
	_ = os.WriteFile("data/walkforward_results.json", jsonOut, 0644)
	fmt.Println("Results written to data/walkforward_results.json")
}

type foldMetrics struct {
	pnl     float64
	fills   int
	sharpe  float64
	maxDD   float64
	hitRate float64
}

func runFold(cfg *config.Config, events []model.FeatureEvent) foldMetrics {
	port := portfolio.New(cfg.Capital.InitialCash)

	var s strategy.Strategy
	switch cfg.Strategy.Name {
	case "obi":
		s = strategy.NewOBIThreshold(cfg.Strategy.Threshold, cfg.Strategy.OrderSize, cfg.Strategy.OrderSize*10)
	case "vpin":
		s = strategy.NewVPINBreakout(cfg.Strategy.Threshold, cfg.Strategy.OrderSize, time.Minute)
	case "vwap_deviation":
		s = strategy.NewVWAPDeviation(cfg.Strategy.Threshold, cfg.Strategy.OrderSize, cfg.Strategy.OrderSize*10)
	case "ema_crossover":
		s = strategy.NewEMACrossover(cfg.Strategy.FastPeriod, cfg.Strategy.SlowPeriod, cfg.Strategy.OrderSize, cfg.Strategy.OrderSize*10)
	default:
		s = strategy.NewOBIThreshold(cfg.Strategy.Threshold, cfg.Strategy.OrderSize, cfg.Strategy.OrderSize*10)
	}

	jw := journal.NewNullWriter()
	rm := risk.NewManager(cfg.Risk, cfg.Capital.InitialCash)
	exec := executor.New(s, port, jw, rm, executor.Config{
		TransactionCostBps: cfg.Executor.TransactionCostBps,
		SlippageBps:        cfg.Executor.SlippageBps,
		FillLatencyMs:      cfg.Executor.FillLatencyMs,
	}, false, nil, "")

	var equity []float64
	lastDay := -1

	for _, event := range events {
		exec.OnFeature(event)

		day := event.EventTime.Year()*1000 + event.EventTime.YearDay()
		if day != lastDay {
			if lastDay != -1 {
				snap := port.Snapshot()
				equity = append(equity, snap.TotalValue)
			}
			lastDay = day
		}
	}

	snap := port.Snapshot()
	equity = append(equity, snap.TotalValue)

	sharpe := metrics.CalculateSharpeRatio(equity, 0.0)
	maxDD := metrics.CalculateMaxDrawdown(equity)

	return foldMetrics{
		pnl:     snap.RealizedPnL,
		fills:   snap.TotalFills,
		sharpe:  sharpe,
		maxDD:   maxDD,
		hitRate: 0,
	}
}

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
		var event model.FeatureEvent
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			continue
		}
		events = append(events, event)
	}
	return events, scanner.Err()
}
