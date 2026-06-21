package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"math"
	"os"
	"strconv"
	"strings"
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
// windows. For each fold, the strategy's threshold (and optionally order size)
// is SELECTED on the training window by a grid search that maximises in-sample
// Sharpe, and only the chosen parameters are then evaluated on the
// out-of-sample test window. Because parameter choice happens on train and
// scoring happens on never-before-seen test data, an edge that survives is
// evidence the strategy generalises rather than overfitting a single period.
//
// Window layout (anchored walk-forward):
//
//   Fold 1: [====TRAIN====][==TEST==]
//   Fold 2: [=======TRAIN=======][==TEST==]
//   Fold 3: [==========TRAIN==========][==TEST==]
//
// The training window grows (anchored to start) while the test window
// slides forward. This is the standard approach used by quant firms.
//
// Parameter grid: pass --thresholds / --order-sizes as comma-separated lists.
// The cartesian product is searched on each fold's train window. When neither
// flag is given the grid degenerates to the single config value, in which case
// there is NO real selection — see the runtime warning. The number of combos
// searched is reported so the best in-sample pick can be read with the
// appropriate multiple-testing scepticism.

type foldResult struct {
	Fold           int     `json:"fold"`
	TrainStart     string  `json:"train_start"`
	TrainEnd       string  `json:"train_end"`
	TestStart      string  `json:"test_start"`
	TestEnd        string  `json:"test_end"`
	BestThreshold  float64 `json:"best_threshold"`
	BestOrderSize  float64 `json:"best_order_size"`
	CombosSearched int     `json:"combos_searched"`
	TrainPnL       float64 `json:"train_pnl"`
	TestPnL        float64 `json:"test_pnl"`
	TrainFills     int     `json:"train_fills"`
	TestFills      int     `json:"test_fills"`
	TrainSharpe    float64 `json:"train_sharpe"`
	Sharpe         float64 `json:"sharpe"` // OOS (test) Sharpe
	SharpeDecay    float64 `json:"sharpe_decay_ratio"`
	MaxDD          float64 `json:"max_dd"`
	HitRate        float64 `json:"hit_rate"`
	// DeflatedSharpe is the selected config's OOS Deflated Sharpe Ratio,
	// deflating the per-observation OOS Sharpe by CombosSearched trials (Bailey &
	// López de Prado). PTrueSharpeNonPositive = 1 − DeflatedSharpe. Both are NaN
	// when the OOS window has too few return observations to define them.
	DeflatedSharpe         float64 `json:"deflated_sharpe"`
	PTrueSharpeNonPositive float64 `json:"p_true_sharpe_non_positive"`
}

func main() {
	configPath := flag.String("config", "configs/default.yaml", "YAML config")
	dataPath := flag.String("data", "", "Historical FeatureEvent JSONL file")
	folds := flag.Int("folds", 5, "Number of walk-forward folds")
	testPct := flag.Float64("test-pct", 0.2, "Fraction of data per test window")
	thresholdsFlag := flag.String("thresholds", "",
		"Comma-separated strategy thresholds to grid-search on each train window "+
			"(e.g. 0.5,0.6,0.7). Empty = use the single config value (no real selection).")
	orderSizesFlag := flag.String("order-sizes", "",
		"Comma-separated order sizes to grid-search on each train window. "+
			"Empty = use the single config value.")
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

	grid := buildGrid(cfg, *thresholdsFlag, *orderSizesFlag)

	fmt.Println("\n═══ Walk-Forward Validation ═══")
	fmt.Printf("Strategy:     %s\n", cfg.Strategy.Name)
	fmt.Printf("Total events: %d\n", len(events))
	fmt.Printf("Folds:        %d\n", *folds)
	fmt.Printf("Test window:  %d events (%.0f%%)\n", testSize, *testPct*100)
	fmt.Printf("Grid combos:  %d (searched on each train window)\n", len(grid))
	fmt.Println("═══════════════════════════════")

	if len(grid) == 1 {
		slog.Warn("Grid has a single combo — no real parameter selection happens; " +
			"train and test run identical fixed params. Pass --thresholds/--order-sizes " +
			"for genuine in-sample selection.")
	}

	results := make([]foldResult, 0, *folds)
	var totalOOS float64
	var oosWins int
	// oosMatrix[fold][config] = that config's out-of-sample Sharpe on the fold's
	// test window. Feeds the Probability of Backtest Overfitting (CSCV) estimator.
	var oosMatrix [][]float64

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

		// Select parameters on the TRAIN window only: evaluate every grid combo
		// on train, keep the one with the highest in-sample Sharpe.
		best, bestParams := selectBestOnTrain(cfg, trainEvents, grid)

		// Apply ONLY the selected params to the out-of-sample TEST window.
		testResult := runFold(cfg, testEvents, bestParams)

		// Score EVERY grid combo out-of-sample on this test window for the PBO
		// matrix (CSCV needs all configs' OOS performance, not just the winner).
		oosRow := make([]float64, len(grid))
		for i, p := range grid {
			m := runFold(cfg, testEvents, p)
			s := m.sharpe
			if math.IsNaN(s) {
				s = math.Inf(-1) // higher-is-better ranking; degenerate combo sinks
			}
			oosRow[i] = s
		}
		oosMatrix = append(oosMatrix, oosRow)

		// Deflated Sharpe of the selected config out-of-sample, deflated by the
		// number of grid combos searched in-sample (the trial count).
		dsr := math.NaN()
		pNonPos := math.NaN()
		if mm := testResult.moments; !math.IsNaN(mm.PerObsSharpe) && mm.NObs >= 2 {
			dsr = metrics.DeflatedSharpeRatio(mm.PerObsSharpe, len(grid), mm.Skew, mm.Kurtosis, mm.NObs)
			pNonPos = metrics.ImpliedPTrueSharpeNonPositive(dsr)
		}

		r := foldResult{
			Fold:                   fold + 1,
			TrainStart:             trainEvents[0].EventTime.Format("2006-01-02T15:04"),
			TrainEnd:               trainEvents[len(trainEvents)-1].EventTime.Format("2006-01-02T15:04"),
			TestStart:              testEvents[0].EventTime.Format("2006-01-02T15:04"),
			TestEnd:                testEvents[len(testEvents)-1].EventTime.Format("2006-01-02T15:04"),
			BestThreshold:          bestParams.threshold,
			BestOrderSize:          bestParams.orderSize,
			CombosSearched:         len(grid),
			TrainPnL:               best.pnl,
			TestPnL:                testResult.pnl,
			TrainFills:             best.fills,
			TestFills:              testResult.fills,
			TrainSharpe:            best.sharpe,
			Sharpe:                 testResult.sharpe,
			SharpeDecay:            sharpeDecayRatio(best.sharpe, testResult.sharpe),
			MaxDD:                  testResult.maxDD,
			HitRate:                testResult.hitRate,
			DeflatedSharpe:         dsr,
			PTrueSharpeNonPositive: pNonPos,
		}
		results = append(results, r)
		totalOOS += testResult.pnl

		oosSign := "-"
		if testResult.pnl > 0 {
			oosSign = "+"
			oosWins++
		}

		fmt.Printf("\nFold %d/%d\n", fold+1, *folds)
		fmt.Printf("  Selected:  threshold=%.4f order_size=%.4f  (best of %d on train)\n",
			r.BestThreshold, r.BestOrderSize, r.CombosSearched)
		fmt.Printf("  Train: %s → %s  (%d events, %d fills, IS Sharpe %.4f, PnL: %.4f)\n",
			r.TrainStart, r.TrainEnd, len(trainEvents), r.TrainFills, r.TrainSharpe, r.TrainPnL)
		fmt.Printf("  Test:  %s → %s  (%d events, %d fills, PnL: %s%.4f)\n",
			r.TestStart, r.TestEnd, len(testEvents), r.TestFills, oosSign, math.Abs(r.TestPnL))
		decayStr := "n/a (IS≤0)"
		if !math.IsNaN(r.SharpeDecay) {
			decayStr = fmt.Sprintf("%.2f", r.SharpeDecay)
		}
		fmt.Printf("  OOS Sharpe: %.4f  IS→OOS decay: %s  MaxDD: %.2f%%  Hit: %.1f%%\n",
			r.Sharpe, decayStr, r.MaxDD*100, r.HitRate*100)
		dsrStr := "n/a (insufficient OOS obs)"
		if !math.IsNaN(r.DeflatedSharpe) {
			dsrStr = fmt.Sprintf("%.4f  P(true Sharpe ≤ 0): %.4f", r.DeflatedSharpe, r.PTrueSharpeNonPositive)
		}
		fmt.Printf("  Deflated Sharpe (deflated by %d trials): %s\n", len(grid), dsrStr)
	}

	printSummary(results, totalOOS, oosWins, len(grid), oosMatrix)

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
	// moments carries the per-observation return statistics of this fold's
	// equity curve so the summary can compute a Deflated Sharpe Ratio for the
	// selected config. The sharpe field above stays the annualized headline.
	moments metrics.ReturnMoments
}

// buildGrid expands the threshold/order-size flag lists into the cartesian
// product of params to search. Each empty flag falls back to the single config
// value, so the default (no flags) yields exactly one combo — i.e. no real
// selection, which main warns about.
func buildGrid(cfg *config.Config, thresholdsCSV, orderSizesCSV string) []params {
	thresholds := parseFloatsOr(thresholdsCSV, cfg.Strategy.Threshold)
	orderSizes := parseFloatsOr(orderSizesCSV, cfg.Strategy.OrderSize)

	grid := make([]params, 0, len(thresholds)*len(orderSizes))
	for _, th := range thresholds {
		for _, os := range orderSizes {
			grid = append(grid, params{threshold: th, orderSize: os})
		}
	}
	return grid
}

// parseFloatsOr parses a comma-separated float list, falling back to [dflt]
// when the input is empty. Unparseable entries are skipped with a warning; if
// nothing parses, it falls back to [dflt] so the grid is never empty.
func parseFloatsOr(csv string, dflt float64) []float64 {
	csv = strings.TrimSpace(csv)
	if csv == "" {
		return []float64{dflt}
	}
	var out []float64
	for _, part := range strings.Split(csv, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		v, err := strconv.ParseFloat(part, 64)
		if err != nil {
			slog.Warn("Skipping unparseable grid value", "value", part, "error", err)
			continue
		}
		out = append(out, v)
	}
	if len(out) == 0 {
		return []float64{dflt}
	}
	return out
}

// selectBestOnTrain evaluates every grid combo on the train window and returns
// the metrics + params of the highest in-sample Sharpe. NaN Sharpe is treated
// as -Inf so a degenerate combo never wins. The grid is guaranteed non-empty.
func selectBestOnTrain(cfg *config.Config, trainEvents []model.FeatureEvent, grid []params) (foldMetrics, params) {
	bestSharpe := math.Inf(-1)
	var bestMetrics foldMetrics
	bestParams := grid[0]
	for _, p := range grid {
		m := runFold(cfg, trainEvents, p)
		s := m.sharpe
		if math.IsNaN(s) {
			s = math.Inf(-1)
		}
		if s > bestSharpe {
			bestSharpe = s
			bestMetrics = m
			bestParams = p
		}
	}
	return bestMetrics, bestParams
}

// sharpeDecayRatio is OOS Sharpe / IS Sharpe — the fraction of the in-sample
// edge that survived out of sample. NaN when IS Sharpe ≤ 0 (no edge to decay).
func sharpeDecayRatio(isSharpe, oosSharpe float64) float64 {
	if isSharpe <= 0 {
		return math.NaN()
	}
	return oosSharpe / isSharpe
}

// printSummary emits a confidence-aware walk-forward summary. It deliberately
// avoids a binary PASS/FAIL verdict: with numCombos parameters searched per
// fold, the best in-sample pick is upward-biased (multiple testing), so the
// honest output is the OOS distribution and IS→OOS decay, leaving the call to
// the reader.
func printSummary(results []foldResult, totalOOS float64, oosWins, numCombos int, oosMatrix [][]float64) {
	fmt.Println("\n═══ Walk-Forward Summary ═══")
	if len(results) == 0 {
		fmt.Println("No folds produced — dataset too small for the requested split.")
		fmt.Println("════════════════════════════")
		return
	}

	oosSharpes := make([]float64, 0, len(results))
	for _, r := range results {
		oosSharpes = append(oosSharpes, r.Sharpe)
	}
	meanSharpe, stdSharpe := meanStd(oosSharpes)

	fmt.Printf("Folds:                %d\n", len(results))
	fmt.Printf("Combos searched/fold: %d  (best-of-%d in-sample → multiple-testing bias)\n",
		numCombos, numCombos)
	fmt.Printf("OOS folds profitable: %d/%d (%.0f%%)\n",
		oosWins, len(results), float64(oosWins)/float64(len(results))*100)
	fmt.Printf("Total OOS PnL:        %.4f\n", totalOOS)
	fmt.Printf("Avg OOS PnL/fold:     %.4f\n", totalOOS/float64(len(results)))
	fmt.Printf("OOS Sharpe:           mean %+.4f  std %.4f\n", meanSharpe, stdSharpe)

	snr := math.NaN()
	if stdSharpe > 0 {
		snr = meanSharpe / stdSharpe
	}
	fmt.Println("─── Confidence ───")
	if math.IsNaN(snr) {
		fmt.Println("Mean/std (OOS Sharpe SNR): n/a (zero cross-fold dispersion)")
	} else {
		fmt.Printf("Mean/std (OOS Sharpe SNR): %+.2f  (higher = more consistent edge)\n", snr)
	}
	fmt.Printf("Reminder: %d combos searched/fold — read the best as upward-biased.\n", numCombos)

	// ─── Overfitting-aware statistics (Bailey & López de Prado) ───
	// Mean Deflated Sharpe across folds (only over folds where it is defined),
	// plus the Probability of Backtest Overfitting from the OOS Sharpe matrix.
	var dsrSum float64
	var dsrN int
	for _, r := range results {
		if !math.IsNaN(r.DeflatedSharpe) {
			dsrSum += r.DeflatedSharpe
			dsrN++
		}
	}
	fmt.Println("─── Deflated Sharpe (selected config, deflated by trials) ───")
	fmt.Printf("Trials (combos) per fold: %d\n", numCombos)
	if dsrN == 0 {
		fmt.Println("Mean OOS Deflated Sharpe: n/a (no fold had enough OOS observations)")
	} else {
		meanDSR := dsrSum / float64(dsrN)
		fmt.Printf("Mean OOS Deflated Sharpe: %.4f over %d/%d folds  (implied mean P(true Sharpe ≤ 0): %.4f)\n",
			meanDSR, dsrN, len(results), 1.0-meanDSR)
	}

	pbo := metrics.ProbabilityBacktestOverfitting(oosMatrix)
	fmt.Println("─── Probability of Backtest Overfitting (CSCV) ───")
	if math.IsNaN(pbo) {
		nConfigs := 0
		if len(oosMatrix) > 0 {
			nConfigs = len(oosMatrix[0])
		}
		fmt.Printf("PBO: n/a (need ≥2 folds and ≥2 configs; have %d folds, %d configs)\n",
			len(oosMatrix), nConfigs)
	} else {
		fmt.Printf("PBO: %.4f  (fraction of folds where the IS-best config was OOS bottom-half)\n", pbo)
	}
	fmt.Println("════════════════════════════")
}

// meanStd returns the mean and population standard deviation of xs.
func meanStd(xs []float64) (mean, std float64) {
	if len(xs) == 0 {
		return 0, 0
	}
	var sum float64
	for _, x := range xs {
		sum += x
	}
	mean = sum / float64(len(xs))
	var variance float64
	for _, x := range xs {
		variance += (x - mean) * (x - mean)
	}
	std = math.Sqrt(variance / float64(len(xs)))
	return mean, std
}

// params is one point in the walk-forward grid: the threshold and order size
// to instantiate the configured strategy with. EMA crossover ignores threshold
// (it is driven by fast/slow periods from config) but still honours order size.
type params struct {
	threshold float64
	orderSize float64
}

func runFold(cfg *config.Config, events []model.FeatureEvent, p params) foldMetrics {
	port := portfolio.New(cfg.Capital.InitialCash)

	var s strategy.Strategy
	switch cfg.Strategy.Name {
	case "obi":
		s = strategy.NewOBIThreshold(p.threshold, p.orderSize, p.orderSize*10)
	case "vpin":
		s = strategy.NewVPINBreakout(p.threshold, p.orderSize, time.Minute)
	case "vwap_deviation":
		s = strategy.NewVWAPDeviation(p.threshold, p.orderSize, p.orderSize*10)
	case "ema_crossover":
		s = strategy.NewEMACrossover(cfg.Strategy.FastPeriod, cfg.Strategy.SlowPeriod, p.orderSize, p.orderSize*10)
	default:
		s = strategy.NewOBIThreshold(p.threshold, p.orderSize, p.orderSize*10)
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
		moments: metrics.EquityReturnMoments(equity),
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
