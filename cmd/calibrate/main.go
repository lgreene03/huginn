// Calibrate runs a parameter sweep over one of huginn's bundled strategies
// against a historical JSONL feature stream and emits a CSV summarising
// Sharpe, MDD, hit rate, turnover, average hold time, and realized PnL per
// parameter combination.
//
// IMPORTANT — this is a sanity sweep, not a research tool. Real strategy
// research belongs in muninn-py (Python + polars + the existing notebook
// helpers). Use the output to spot egregiously bad parameter regions before
// committing a strategy to a long backtest or live paper run; do not use it
// to hill-climb on noise.
//
// # Usage
//
//	muninn-calibrate \
//	    --strategy obi \
//	    --data data/historical.jsonl \
//	    --grid threshold=0.5,0.6,0.7,0.8 \
//	    --grid order_size=0.005,0.01,0.02 \
//	    --workers 4 \
//	    --out data/calibration/obi-<timestamp>.csv
//
// Strategies and their supported grid keys:
//
//	obi             — threshold, order_size
//	vpin            — threshold, order_size, cooldown_ms
//	vwap_deviation  — threshold_pct, order_size
//	ema_crossover   — fast_period, slow_period, order_size
//
// Unknown grid keys for a given strategy abort with a non-zero exit.
package main

import (
	"bufio"
	"encoding/csv"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/lgreene03/huginn/internal/config"
	"github.com/lgreene03/huginn/internal/executor"
	"github.com/lgreene03/huginn/internal/metrics"
	"github.com/lgreene03/huginn/internal/model"
	"github.com/lgreene03/huginn/internal/portfolio"
	"github.com/lgreene03/huginn/internal/risk"
	"github.com/lgreene03/huginn/internal/strategy"
)

// stringSliceFlag collects repeated `--grid key=v1,v2,...` flags.
type stringSliceFlag []string

func (s *stringSliceFlag) String() string     { return strings.Join(*s, ";") }
func (s *stringSliceFlag) Set(v string) error { *s = append(*s, v); return nil }

func main() {
	var (
		strategyName = flag.String("strategy", "", "Strategy to calibrate: obi | vpin | vwap_deviation | ema_crossover")
		dataPath     = flag.String("data", "", "Path to historical FeatureEvent JSONL")
		out          = flag.String("out", "", "Output CSV path (default: data/calibration/<strategy>-<unixsec>.csv)")
		workers      = flag.Int("workers", 4, "Parallel goroutines")
		initialCash  = flag.Float64("cash", 100_000, "Starting paper-trading cash")
		walkForward  = flag.Int("walk-forward", 0,
			"Walk-forward fold count. 0 = single full-sample sweep (default). "+
				"N≥1 splits events chronologically into N+1 equal windows; "+
				"for each of N folds the grid is evaluated on the train prefix, "+
				"the best Sharpe is selected, then re-run on the next window "+
				"(out-of-sample). Output CSV reports per-fold winning params "+
				"and their test metrics.")
		grids stringSliceFlag
	)
	flag.Var(&grids, "grid", "Repeatable: key=v1,v2,v3 — defines one dimension of the sweep")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	if *strategyName == "" || *dataPath == "" {
		slog.Error("--strategy and --data are required")
		flag.Usage()
		os.Exit(2)
	}

	grid, err := parseGrid(grids)
	if err != nil {
		slog.Error("Invalid --grid flag", "error", err)
		os.Exit(2)
	}
	combos := cartesian(grid)
	if len(combos) == 0 {
		slog.Error("Empty parameter grid — nothing to sweep")
		os.Exit(2)
	}

	events, err := loadEvents(*dataPath)
	if err != nil {
		slog.Error("Failed to load events", "error", err, "path", *dataPath)
		os.Exit(1)
	}
	slog.Info("Loaded events", "count", len(events), "path", *dataPath)

	resultPath := *out
	if resultPath == "" {
		ts := time.Now().UTC().Unix()
		resultPath = filepath.Join("data", "calibration",
			fmt.Sprintf("%s-%d.csv", *strategyName, ts))
	}
	if err := os.MkdirAll(filepath.Dir(resultPath), 0o750); err != nil {
		slog.Error("Failed to create output dir", "error", err)
		os.Exit(1)
	}

	if *walkForward > 0 {
		folds, err := runWalkForward(*strategyName, combos, events, *walkForward, *initialCash, *workers)
		if err != nil {
			slog.Error("Walk-forward failed", "error", err)
			os.Exit(1)
		}
		if err := writeWalkForwardCSV(resultPath, *strategyName, folds); err != nil {
			slog.Error("Failed to write walk-forward CSV", "error", err)
			os.Exit(1)
		}
		slog.Info("Walk-forward calibration complete",
			"folds", len(folds), "combos_per_fold", len(combos), "out", resultPath,
		)
		return
	}

	results := runSweep(*strategyName, combos, events, *initialCash, *workers)
	if err := writeCSV(resultPath, *strategyName, results); err != nil {
		slog.Error("Failed to write CSV", "error", err)
		os.Exit(1)
	}
	slog.Info("Calibration complete",
		"combos", len(combos),
		"out", resultPath,
	)
}

// parseGrid converts ["threshold=0.5,0.6", "order_size=0.01"] into
// {"threshold": ["0.5","0.6"], "order_size": ["0.01"]}.
func parseGrid(in []string) (map[string][]string, error) {
	out := map[string][]string{}
	for _, g := range in {
		k, v, ok := strings.Cut(g, "=")
		if !ok || k == "" || v == "" {
			return nil, fmt.Errorf("malformed grid %q (want key=v1,v2,...)", g)
		}
		out[k] = strings.Split(v, ",")
	}
	return out, nil
}

// cartesian expands the grid into the full set of parameter combinations.
// Determinism: keys are visited in sorted order, so the output ordering is
// reproducible across runs (helps with diffing CSVs).
func cartesian(g map[string][]string) []map[string]string {
	keys := make([]string, 0, len(g))
	for k := range g {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	result := []map[string]string{{}}
	for _, k := range keys {
		var next []map[string]string
		for _, base := range result {
			for _, v := range g[k] {
				c := make(map[string]string, len(base)+1)
				for kk, vv := range base {
					c[kk] = vv
				}
				c[k] = v
				next = append(next, c)
			}
		}
		result = next
	}
	return result
}

func loadEvents(path string) ([]model.FeatureEvent, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	var events []model.FeatureEvent
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		var ev model.FeatureEvent
		if err := json.Unmarshal(scanner.Bytes(), &ev); err != nil {
			return nil, fmt.Errorf("malformed event line: %w", err)
		}
		events = append(events, ev)
	}
	return events, scanner.Err()
}

// result captures the per-combination summary metrics.
type result struct {
	Params      map[string]string
	Sharpe      float64
	MaxDrawdown float64
	Fills       int
	RealizedPnL float64
	HitRate     float64
	Turnover    float64
	AvgHoldSec  float64
}

func runSweep(stratName string, combos []map[string]string, events []model.FeatureEvent, initialCash float64, workers int) []result {
	if workers < 1 {
		workers = 1
	}
	results := make([]result, len(combos))
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup

	for i, combo := range combos {
		i, combo := i, combo
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			r, err := runOne(stratName, combo, events, initialCash)
			if err != nil {
				slog.Warn("Combo failed", "combo", combo, "error", err)
				results[i] = result{Params: combo}
				return
			}
			results[i] = r
		}()
	}
	wg.Wait()
	return results
}

// runOne runs a single parameter combination through the existing executor +
// portfolio pipeline. No journal IO (paper-mode handles nil), permissive risk
// (we want the strategy to expose its own behaviour, not be gated by the
// sweep's risk config).
func runOne(stratName string, params map[string]string, events []model.FeatureEvent, initialCash float64) (result, error) {
	strategy, err := buildStrategy(stratName, params)
	if err != nil {
		return result{}, err
	}

	port := portfolio.New(initialCash)
	riskCfg := config.RiskConfig{
		MaxDrawdownPct:    0.99, // effectively disabled for the sweep
		DailyLossLimit:    1e18, // disabled
		PositionLimitHard: 1e18, // disabled
	}
	rm := risk.NewManager(riskCfg, initialCash)
	exec := executor.New(strategy, port, nil, rm, executor.Config{
		TransactionCostBps: 5,
		SlippageBps:        2,
	}, false, nil, "" /* no state persistence */)

	// Replay daily equity for Sharpe/MDD.
	var equity []float64
	var lastBucket string
	for _, ev := range events {
		exec.OnFeature(ev)
		bucket := ev.EventTime.UTC().Format("2006-01-02")
		if bucket != lastBucket && lastBucket != "" {
			equity = append(equity, port.Snapshot().TotalValue)
		}
		lastBucket = bucket
	}
	snap := port.Snapshot()
	equity = append(equity, snap.TotalValue)

	fills := port.Fills()
	return result{
		Params:      params,
		Sharpe:      metrics.CalculateSharpeRatio(equity, 0),
		MaxDrawdown: metrics.CalculateMaxDrawdown(equity),
		Fills:       len(fills),
		RealizedPnL: snap.RealizedPnL,
		HitRate:     metrics.HitRate(fills),
		Turnover:    metrics.Turnover(fills),
		AvgHoldSec:  metrics.AvgHoldTimeSeconds(fills),
	}, nil
}

func buildStrategy(name string, p map[string]string) (strategy.Strategy, error) {
	switch name {
	case "obi":
		th, err := mustFloat(p, "threshold", 0.7)
		if err != nil {
			return nil, err
		}
		size, err := mustFloat(p, "order_size", 0.01)
		if err != nil {
			return nil, err
		}
		if err := rejectUnknown(p, "threshold", "order_size"); err != nil {
			return nil, err
		}
		return strategy.NewOBIThreshold(th, size, size*10), nil

	case "vpin":
		th, err := mustFloat(p, "threshold", 0.5)
		if err != nil {
			return nil, err
		}
		size, err := mustFloat(p, "order_size", 0.01)
		if err != nil {
			return nil, err
		}
		coolMs, err := mustInt(p, "cooldown_ms", int(time.Minute/time.Millisecond))
		if err != nil {
			return nil, err
		}
		if err := rejectUnknown(p, "threshold", "order_size", "cooldown_ms"); err != nil {
			return nil, err
		}
		return strategy.NewVPINBreakout(th, size, time.Duration(coolMs)*time.Millisecond), nil

	case "vwap_deviation":
		th, err := mustFloat(p, "threshold_pct", 0.001)
		if err != nil {
			return nil, err
		}
		size, err := mustFloat(p, "order_size", 0.01)
		if err != nil {
			return nil, err
		}
		if err := rejectUnknown(p, "threshold_pct", "order_size"); err != nil {
			return nil, err
		}
		return strategy.NewVWAPDeviation(th, size, size*10), nil

	case "ema_crossover":
		fast, err := mustInt(p, "fast_period", 10)
		if err != nil {
			return nil, err
		}
		slow, err := mustInt(p, "slow_period", 30)
		if err != nil {
			return nil, err
		}
		size, err := mustFloat(p, "order_size", 0.01)
		if err != nil {
			return nil, err
		}
		if err := rejectUnknown(p, "fast_period", "slow_period", "order_size"); err != nil {
			return nil, err
		}
		if fast >= slow {
			return nil, fmt.Errorf("ema_crossover: fast_period=%d must be < slow_period=%d", fast, slow)
		}
		return strategy.NewEMACrossover(fast, slow, size, size*10), nil
	}
	return nil, fmt.Errorf("unknown strategy %q", name)
}

func mustFloat(p map[string]string, key string, dflt float64) (float64, error) {
	v, ok := p[key]
	if !ok {
		return dflt, nil
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid %s=%q: %w", key, v, err)
	}
	return f, nil
}

func mustInt(p map[string]string, key string, dflt int) (int, error) {
	v, ok := p[key]
	if !ok {
		return dflt, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("invalid %s=%q: %w", key, v, err)
	}
	return n, nil
}

func rejectUnknown(p map[string]string, allowed ...string) error {
	set := make(map[string]struct{}, len(allowed))
	for _, k := range allowed {
		set[k] = struct{}{}
	}
	for k := range p {
		if _, ok := set[k]; !ok {
			return fmt.Errorf("strategy does not accept grid key %q (allowed: %v)", k, allowed)
		}
	}
	return nil
}

// foldResult captures the per-fold winning combination + its out-of-sample
// performance. The CSV ultimately written by writeWalkForwardCSV records
// one row per fold.
type foldResult struct {
	Idx          int
	BestParams   map[string]string
	TrainSharpe  float64
	TestSharpe   float64
	TestMDD      float64
	TestFills    int
	TestPnL      float64
	TestHitRate  float64
	TestTurnover float64
	TestHoldSec  float64
}

// runWalkForward divides the event stream into N+1 chronological windows of
// equal size. For each of the N folds:
//
//  1. Evaluate every combo on the train prefix (events[0 .. (k+1)*winSize]).
//  2. Pick the combo with the highest Sharpe.
//  3. Re-run that combo on the next window (the test slice) and record its
//     out-of-sample metrics.
//
// This is the canonical guard against in-sample overfit: parameter choice
// happens on train, evaluation happens on never-before-seen test data.
func runWalkForward(stratName string, combos []map[string]string, events []model.FeatureEvent, windows int, initialCash float64, workers int) ([]foldResult, error) {
	n := len(events)
	if windows < 1 {
		return nil, fmt.Errorf("--walk-forward must be ≥ 1, got %d", windows)
	}
	winSize := n / (windows + 1)
	if winSize < 2 {
		return nil, fmt.Errorf("dataset too small (%d events) for %d-fold walk-forward (need ≥ %d events)",
			n, windows, 2*(windows+1))
	}

	folds := make([]foldResult, windows)
	for k := 0; k < windows; k++ {
		trainEnd := (k + 1) * winSize
		testEnd := (k + 2) * winSize
		if testEnd > n {
			testEnd = n
		}
		train := events[:trainEnd]
		test := events[trainEnd:testEnd]

		slog.Info("Walk-forward fold",
			"fold", k, "train_events", len(train), "test_events", len(test),
		)

		trainResults := runSweep(stratName, combos, train, initialCash, workers)
		best, err := pickBestSharpe(trainResults)
		if err != nil {
			return nil, fmt.Errorf("fold %d: %w", k, err)
		}

		testR, err := runOne(stratName, best.Params, test, initialCash)
		if err != nil {
			return nil, fmt.Errorf("fold %d test eval: %w", k, err)
		}
		folds[k] = foldResult{
			Idx:          k,
			BestParams:   best.Params,
			TrainSharpe:  best.Sharpe,
			TestSharpe:   testR.Sharpe,
			TestMDD:      testR.MaxDrawdown,
			TestFills:    testR.Fills,
			TestPnL:      testR.RealizedPnL,
			TestHitRate:  testR.HitRate,
			TestTurnover: testR.Turnover,
			TestHoldSec:  testR.AvgHoldSec,
		}
	}
	return folds, nil
}

// pickBestSharpe returns the result with the highest Sharpe. NaN Sharpe
// (the failure path) is treated as -Inf so it never wins. If every combo
// failed, the function errors.
func pickBestSharpe(rs []result) (result, error) {
	if len(rs) == 0 {
		return result{}, fmt.Errorf("no results to choose from")
	}
	bestIdx := -1
	bestSharpe := math.Inf(-1)
	for i, r := range rs {
		s := r.Sharpe
		if math.IsNaN(s) {
			s = math.Inf(-1)
		}
		if s > bestSharpe {
			bestSharpe = s
			bestIdx = i
		}
	}
	if bestIdx < 0 {
		return result{}, fmt.Errorf("every fold combo produced NaN Sharpe")
	}
	return rs[bestIdx], nil
}

func writeWalkForwardCSV(path, stratName string, folds []foldResult) error {
	// Stable header: union of param keys across all folds.
	keySet := map[string]struct{}{}
	for _, f := range folds {
		for k := range f.BestParams {
			keySet[k] = struct{}{}
		}
	}
	keys := make([]string, 0, len(keySet))
	for k := range keySet {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	w := csv.NewWriter(f)
	defer w.Flush()

	header := []string{"strategy", "fold"}
	header = append(header, keys...)
	header = append(header,
		"train_sharpe", "test_sharpe", "test_max_drawdown",
		"test_fills", "test_realized_pnl",
		"test_hit_rate", "test_turnover", "test_avg_hold_seconds",
	)
	if err := w.Write(header); err != nil {
		return err
	}

	for _, fold := range folds {
		row := []string{stratName, strconv.Itoa(fold.Idx)}
		for _, k := range keys {
			row = append(row, fold.BestParams[k])
		}
		row = append(row,
			strconv.FormatFloat(fold.TrainSharpe, 'f', 4, 64),
			strconv.FormatFloat(fold.TestSharpe, 'f', 4, 64),
			strconv.FormatFloat(fold.TestMDD, 'f', 6, 64),
			strconv.Itoa(fold.TestFills),
			strconv.FormatFloat(fold.TestPnL, 'f', 4, 64),
			strconv.FormatFloat(fold.TestHitRate, 'f', 4, 64),
			strconv.FormatFloat(fold.TestTurnover, 'f', 4, 64),
			strconv.FormatFloat(fold.TestHoldSec, 'f', 1, 64),
		)
		if err := w.Write(row); err != nil {
			return err
		}
	}
	return nil
}

func writeCSV(path, stratName string, results []result) error {
	// Collect all parameter keys (union across combos) for stable header.
	keySet := map[string]struct{}{}
	for _, r := range results {
		for k := range r.Params {
			keySet[k] = struct{}{}
		}
	}
	keys := make([]string, 0, len(keySet))
	for k := range keySet {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	w := csv.NewWriter(f)
	defer w.Flush()

	header := append([]string{"strategy"}, keys...)
	header = append(header,
		"sharpe", "max_drawdown", "fills", "realized_pnl",
		"hit_rate", "turnover", "avg_hold_seconds",
	)
	if err := w.Write(header); err != nil {
		return err
	}

	for _, r := range results {
		row := []string{stratName}
		for _, k := range keys {
			row = append(row, r.Params[k])
		}
		row = append(row,
			strconv.FormatFloat(r.Sharpe, 'f', 4, 64),
			strconv.FormatFloat(r.MaxDrawdown, 'f', 6, 64),
			strconv.Itoa(r.Fills),
			strconv.FormatFloat(r.RealizedPnL, 'f', 4, 64),
			strconv.FormatFloat(r.HitRate, 'f', 4, 64),
			strconv.FormatFloat(r.Turnover, 'f', 4, 64),
			strconv.FormatFloat(r.AvgHoldSec, 'f', 1, 64),
		)
		if err := w.Write(row); err != nil {
			return err
		}
	}
	return nil
}
