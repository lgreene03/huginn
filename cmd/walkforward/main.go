package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"math"
	"os"
	"strconv"
	"strings"

	"github.com/lgreene03/huginn/internal/config"
	"github.com/lgreene03/huginn/internal/research"
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
//
// The walk-forward compute itself lives in internal/research so heavy backtests
// can also run out of the live process (cmd/research). This command owns only
// CLI parsing, console presentation, and the artifact write.

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

	events, err := research.LoadEvents(*dataPath)
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

	grid := research.BuildGrid(cfg, parseFloatsOr(*thresholdsFlag, cfg.Strategy.Threshold), parseFloatsOr(*orderSizesFlag, cfg.Strategy.OrderSize))

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

	res, err := research.Run(research.Options{
		Config:  cfg,
		Events:  events,
		Folds:   *folds,
		TestPct: *testPct,
		Grid:    grid,
	})
	if err != nil {
		slog.Error("Walk-forward run failed", "error", err)
		os.Exit(1)
	}

	for _, r := range res.Folds {
		oosSign := "-"
		if r.TestPnL > 0 {
			oosSign = "+"
		}

		fmt.Printf("\nFold %d/%d\n", r.Fold, *folds)
		fmt.Printf("  Selected:  threshold=%.4f order_size=%.4f  (best of %d on train)\n",
			r.BestThreshold, r.BestOrderSize, r.CombosSearched)
		fmt.Printf("  Train: %s → %s  (%d events, %d fills, IS Sharpe %.4f, PnL: %.4f)\n",
			r.TrainStart, r.TrainEnd, r.TrainEventCount, r.TrainFills, r.TrainSharpe, r.TrainPnL)
		fmt.Printf("  Test:  %s → %s  (%d events, %d fills, PnL: %s%.4f)\n",
			r.TestStart, r.TestEnd, r.TestEventCount, r.TestFills, oosSign, math.Abs(r.TestPnL))
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
		fmt.Printf("  Deflated Sharpe (deflated by %d trials): %s\n", res.GridSize, dsrStr)
	}

	printSummary(res)

	resultsPath := os.Getenv("WALKFORWARD_RESULTS_PATH")
	if resultsPath == "" {
		resultsPath = "data/walkforward_results.json"
	}
	jsonOut, err := json.MarshalIndent(res.Folds, "", "  ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "error marshaling walk-forward results: %v\n", err)
		os.Exit(1)
	}
	if err := os.WriteFile(resultsPath, jsonOut, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "error writing %s: %v\n", resultsPath, err)
		os.Exit(1)
	}
	fmt.Printf("Results written to %s\n", resultsPath)
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

// buildGrid expands the threshold/order-size flag lists into the cartesian
// product of params to search. Each empty flag falls back to the single config
// value, so the default (no flags) yields exactly one combo — i.e. no real
// selection, which main warns about. Retained as a thin wrapper over
// research.BuildGrid so the existing cmd/walkforward tests keep exercising the
// CLI's grid construction.
func buildGrid(cfg *config.Config, thresholdsCSV, orderSizesCSV string) []research.Params {
	return research.BuildGrid(cfg, parseFloatsOr(thresholdsCSV, cfg.Strategy.Threshold), parseFloatsOr(orderSizesCSV, cfg.Strategy.OrderSize))
}

// sharpeDecayRatio is retained as a thin wrapper over research.SharpeDecayRatio
// so the existing cmd/walkforward test keeps exercising the same behavior.
func sharpeDecayRatio(isSharpe, oosSharpe float64) float64 {
	return research.SharpeDecayRatio(isSharpe, oosSharpe)
}

// printSummary emits a confidence-aware walk-forward summary. It deliberately
// avoids a binary PASS/FAIL verdict: with numCombos parameters searched per
// fold, the best in-sample pick is upward-biased (multiple testing), so the
// honest output is the OOS distribution and IS→OOS decay, leaving the call to
// the reader.
func printSummary(res research.Result) {
	results := res.Folds
	totalOOS := res.TotalOOSPnL
	oosWins := res.OOSFoldsProfitable
	numCombos := res.GridSize

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

	pbo := res.PBO
	fmt.Println("─── Probability of Backtest Overfitting (CSCV) ───")
	if math.IsNaN(pbo) {
		nConfigs := 0
		if len(res.OOSMatrix) > 0 {
			nConfigs = len(res.OOSMatrix[0])
		}
		fmt.Printf("PBO: n/a (need ≥2 folds and ≥2 configs; have %d folds, %d configs)\n",
			len(res.OOSMatrix), nConfigs)
	} else {
		fmt.Printf("PBO: %.4f  (fraction of folds where the IS-best config was OOS bottom-half)\n", pbo)
		// Honesty caveat: on a short fixture the OOS Sharpes across configs are
		// often near-tied (near-zero cross-fold dispersion). When that happens a
		// PBO pinned at 1.00 (or 0.00) is a degenerate/conservative tie-break of
		// a rank statistic, NOT a finely-measured overfitting probability — the
		// rank flips on noise. The load-bearing evidence in that regime is the
		// OOS-folds-profitable count above, not the PBO value itself.
		if (pbo >= 0.99 || pbo <= 0.01) && stdSharpe < 1e-9 {
			fmt.Println("  ⚠ Caveat: OOS Sharpes are near-tied across configs on this short " +
				"fixture, so this extreme PBO is a conservative tie-break, not a precise " +
				"probability — read the OOS-folds-profitable count as the real signal.")
		}
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
