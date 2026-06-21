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
// RISK — by DEFAULT the sweep is risk-unconstrained: drawdown, daily-loss, and
// position limits are set so high they never trip, so each combo reveals the
// strategy's raw behaviour rather than being clipped by the production risk
// gate. Pass --apply-risk (with --config) to evaluate the grid under the same
// RiskConfig live trading would face; combos may then be throttled or halted.
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
//	obi             — threshold, order_size, stop_loss_pct, take_profit_pct,
//	                  max_hold_ms, cooldown_ms, max_notional, cost_hurdle_k,
//	                  obi_edge_bps_per_unit
//	                  (cost_hurdle_k>0 enables the net-of-cost signal gate;
//	                   sweep it to find the K that maximises NET realized PnL)
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

// Sweep fill-cost constants. These are the simulated per-fill costs every combo
// is evaluated under; they're named so the net-of-cost gate (quant-alpha-1) can
// reuse the identical numbers — the gate's cost estimate must match the cost the
// simulated fills actually incur, or a swept cost_hurdle_k would be miscalibrated.
const (
	sweepTxCostBps   = 5.0
	sweepSlippageBps = 2.0
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
		applyRisk    = flag.Bool("apply-risk", false,
			"Apply the production RiskConfig (from --config) during the sweep. "+
				"DEFAULT false: the sweep runs risk-unconstrained (drawdown/daily-loss/"+
				"position limits effectively disabled) so each combo exposes the raw "+
				"strategy behaviour. Set true to see how the grid behaves once the "+
				"production risk gate is applied — combos may then be throttled or halted.")
		configPath = flag.String("config", "configs/default.yaml",
			"YAML config supplying the production RiskConfig; only consulted when --apply-risk is set.")
		walkForward = flag.Int("walk-forward", 0,
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

	// Risk policy for the sweep. By default the sweep is risk-unconstrained
	// (limits effectively disabled) so each combo exposes the strategy's raw
	// behaviour; --apply-risk swaps in the production RiskConfig from --config
	// so the grid is evaluated under the same gate live trading would face.
	riskCfg := unconstrainedRiskConfig()
	if *applyRisk {
		cfg, cerr := config.Load(*configPath)
		if cerr != nil {
			slog.Error("Failed to load config for --apply-risk", "error", cerr, "path", *configPath)
			os.Exit(1)
		}
		riskCfg = cfg.Risk
		slog.Info("Applying production RiskConfig to sweep", "config", *configPath,
			"max_drawdown_pct", riskCfg.MaxDrawdownPct,
			"daily_loss_limit", riskCfg.DailyLossLimit,
			"position_limit_hard", riskCfg.PositionLimitHard)
	} else {
		slog.Info("Sweep is risk-unconstrained (default); pass --apply-risk to apply production RiskConfig")
	}

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
		folds, oosMatrix, err := runWalkForward(*strategyName, combos, events, *walkForward, *initialCash, *workers, riskCfg)
		if err != nil {
			slog.Error("Walk-forward failed", "error", err)
			os.Exit(1)
		}
		if err := writeWalkForwardCSV(resultPath, *strategyName, folds); err != nil {
			slog.Error("Failed to write walk-forward CSV", "error", err)
			os.Exit(1)
		}
		printWalkForwardSummary(folds, len(combos), oosMatrix)
		slog.Info("Walk-forward calibration complete",
			"folds", len(folds), "combos_per_fold", len(combos), "out", resultPath,
		)
		return
	}

	results := runSweep(*strategyName, combos, events, *initialCash, *workers, riskCfg)
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
	// Moments carries the per-observation return statistics of this combo's
	// equity curve (per-obs Sharpe, skew, kurtosis, nObs) so the walk-forward
	// path can compute a Deflated Sharpe Ratio for the selected config without
	// re-deriving them. Populated by runOne; the annualized Sharpe field above
	// remains the headline figure.
	Moments metrics.ReturnMoments
}

func runSweep(stratName string, combos []map[string]string, events []model.FeatureEvent, initialCash float64, workers int, riskCfg config.RiskConfig) []result {
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
			r, err := runOne(stratName, combo, events, initialCash, riskCfg)
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
// portfolio pipeline. No journal IO (paper-mode handles nil). The risk policy
// is supplied by the caller: by default it is unconstrained (see
// unconstrainedRiskConfig) so the strategy exposes its own behaviour, but
// --apply-risk substitutes the production RiskConfig so the sweep is gated
// exactly as live trading would be.
func runOne(stratName string, params map[string]string, events []model.FeatureEvent, initialCash float64, riskCfg config.RiskConfig) (result, error) {
	strat, err := buildStrategy(stratName, params)
	if err != nil {
		return result{}, err
	}

	port := portfolio.New(initialCash)
	rm := risk.NewManager(riskCfg, initialCash)
	exec := executor.New(strat, port, nil, rm, executor.Config{
		TransactionCostBps: sweepTxCostBps,
		SlippageBps:        sweepSlippageBps,
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
		Moments:     metrics.EquityReturnMoments(equity),
	}, nil
}

// unconstrainedRiskConfig is the default sweep risk policy: drawdown, daily
// loss, and position limits are set so high they never trip. This is
// deliberate — the default calibrate sweep is RISK-UNCONSTRAINED so each combo
// reveals the strategy's raw behaviour rather than being clipped by the
// production risk gate. Pass --apply-risk to swap in the production RiskConfig.
func unconstrainedRiskConfig() config.RiskConfig {
	return config.RiskConfig{
		MaxDrawdownPct:    0.99, // effectively disabled for the sweep
		DailyLossLimit:    1e18, // disabled
		PositionLimitHard: 1e18, // disabled
	}
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
		// Exit/throttle grid keys (quant-12). Defaults equal the historical
		// hardcoded values via DefaultOBIParams, so omitting them reproduces
		// the pre-config sweep behaviour exactly.
		def := strategy.DefaultOBIParams()
		stopLoss, err := mustFloat(p, "stop_loss_pct", def.StopLossPct)
		if err != nil {
			return nil, err
		}
		takeProfit, err := mustFloat(p, "take_profit_pct", def.TakeProfitPct)
		if err != nil {
			return nil, err
		}
		holdMs, err := mustInt(p, "max_hold_ms", int(def.MaxHoldTime/time.Millisecond))
		if err != nil {
			return nil, err
		}
		cooldownMs, err := mustInt(p, "cooldown_ms", int(def.Cooldown/time.Millisecond))
		if err != nil {
			return nil, err
		}
		maxNotional, err := mustFloat(p, "max_notional", def.MaxNotional)
		if err != nil {
			return nil, err
		}
		// Net-of-cost gate sweep keys (quant-alpha-1). cost_hurdle_k DEFAULTS to
		// 0 ⇒ inert, so a grid that omits it reproduces the pre-gate sweep
		// exactly. obi_edge_bps_per_unit defaults to 0 ⇒ the strategy's
		// DefaultEdgeBpsPerUnit. Sweeping cost_hurdle_k finds the K that
		// maximises NET realized PnL.
		costHurdleK, err := mustFloat(p, "cost_hurdle_k", 0)
		if err != nil {
			return nil, err
		}
		edgeBpsPerUnit, err := mustFloat(p, "obi_edge_bps_per_unit", 0)
		if err != nil {
			return nil, err
		}
		if err := rejectUnknown(p, "threshold", "order_size",
			"stop_loss_pct", "take_profit_pct", "max_hold_ms", "cooldown_ms", "max_notional",
			"cost_hurdle_k", "obi_edge_bps_per_unit"); err != nil {
			return nil, err
		}
		obiStrat := strategy.NewOBIThresholdWithParams(th, size, size*10, strategy.OBIParams{
			StopLossPct:   stopLoss,
			TakeProfitPct: takeProfit,
			MaxHoldTime:   time.Duration(holdMs) * time.Millisecond,
			Cooldown:      time.Duration(cooldownMs) * time.Millisecond,
			MaxNotional:   maxNotional,
		})
		if costHurdleK > 0 {
			// Cost primitives match the sweep's executor.Config so the gate's
			// cost estimate agrees with the fills the sweep actually simulates.
			obiStrat.SetCostHurdle(&strategy.CostHurdle{
				K:                  costHurdleK,
				TransactionCostBps: sweepTxCostBps,
				SlippageBps:        sweepSlippageBps,
				Edge:               strategy.OBIEdgeModel{BpsPerUnit: edgeBpsPerUnit},
			})
		}
		return obiStrat, nil

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
	// TestMoments holds the selected config's out-of-sample per-observation
	// return statistics, used to compute the Deflated Sharpe Ratio.
	TestMoments metrics.ReturnMoments
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
func runWalkForward(stratName string, combos []map[string]string, events []model.FeatureEvent, windows int, initialCash float64, workers int, riskCfg config.RiskConfig) ([]foldResult, [][]float64, error) {
	n := len(events)
	if windows < 1 {
		return nil, nil, fmt.Errorf("--walk-forward must be ≥ 1, got %d", windows)
	}
	winSize := n / (windows + 1)
	if winSize < 2 {
		return nil, nil, fmt.Errorf("dataset too small (%d events) for %d-fold walk-forward (need ≥ %d events)",
			n, windows, 2*(windows+1))
	}

	folds := make([]foldResult, windows)
	// oosMatrix is the [fold][config] out-of-sample Sharpe matrix that feeds the
	// Probability of Backtest Overfitting (CSCV) estimator. Each row is one
	// fold's test slice scored across EVERY combo (not just the selected one),
	// so PBO can ask whether the in-sample winner generalises.
	oosMatrix := make([][]float64, windows)
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

		trainResults := runSweep(stratName, combos, train, initialCash, workers, riskCfg)
		best, err := pickBestSharpe(trainResults)
		if err != nil {
			return nil, nil, fmt.Errorf("fold %d: %w", k, err)
		}

		// Score every combo out-of-sample on this fold's test slice for the PBO
		// matrix. The selected config's OOS metrics are read back from this same
		// sweep so we don't run it twice.
		testResults := runSweep(stratName, combos, test, initialCash, workers, riskCfg)
		oosRow := make([]float64, len(testResults))
		for i, tr := range testResults {
			s := tr.Sharpe
			if math.IsNaN(s) {
				// CSCV ranks higher-is-better; a failed/degenerate combo gets the
				// worst possible score so it never inflates a rank.
				s = math.Inf(-1)
			}
			oosRow[i] = s
		}
		oosMatrix[k] = oosRow

		// Locate the selected (best-IS) combo within the OOS sweep to read its
		// out-of-sample metrics + moments.
		testR := testResults[indexOfParams(testResults, best.Params)]
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
			TestMoments:  testR.Moments,
		}
	}
	return folds, oosMatrix, nil
}

// indexOfParams returns the index of the result whose Params map equals target.
// combos preserve order across sweeps of the same combo list, so the selected
// in-sample winner maps to the same slot in the out-of-sample sweep; this scan
// is a defensive lookup that also tolerates any future reordering. Returns 0 if
// no exact match is found (the matrices are built from the same combos, so a
// miss is not expected).
func indexOfParams(rs []result, target map[string]string) int {
	for i, r := range rs {
		if sameParams(r.Params, target) {
			return i
		}
	}
	return 0
}

func sameParams(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
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

// sharpeDecayRatio is OOS Sharpe / IS Sharpe — the fraction of in-sample edge
// that survived out of sample. ~1.0 means the edge held; <<1 (or negative)
// signals overfit. Returns NaN when the IS Sharpe is non-positive (the ratio is
// undefined: there was no in-sample edge to decay from).
func sharpeDecayRatio(isSharpe, oosSharpe float64) float64 {
	if isSharpe <= 0 {
		return math.NaN()
	}
	return oosSharpe / isSharpe
}

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

// printWalkForwardSummary emits a confidence-aware walk-forward summary to
// stdout. It deliberately does NOT collapse the run into a binary PASS/FAIL:
// with numCombos parameter combinations searched per fold, the best in-sample
// Sharpe is subject to multiple-testing selection bias, so the honest output is
// the IS→OOS decay per fold plus the distribution of OOS metrics across folds,
// leaving the judgement call to the reader.
func printWalkForwardSummary(folds []foldResult, numCombos int, oosMatrix [][]float64) {
	if len(folds) == 0 {
		return
	}

	oosSharpes := make([]float64, 0, len(folds))
	oosPnLs := make([]float64, 0, len(folds))
	for _, f := range folds {
		oosSharpes = append(oosSharpes, f.TestSharpe)
		oosPnLs = append(oosPnLs, f.TestPnL)
	}
	meanOOS, stdOOS := meanStd(oosSharpes)
	meanPnL, stdPnL := meanStd(oosPnLs)

	fmt.Println("\n═══ Walk-Forward Summary ═══")
	fmt.Printf("Folds:            %d\n", len(folds))
	fmt.Printf("Combos/fold:      %d  (best-of-%d selected in-sample → multiple-testing bias)\n",
		numCombos, numCombos)
	fmt.Println("─── Per-fold IS→OOS Sharpe decay ───")
	for _, f := range folds {
		decay := sharpeDecayRatio(f.TrainSharpe, f.TestSharpe)
		decayStr := "n/a (IS≤0)"
		if !math.IsNaN(decay) {
			decayStr = fmt.Sprintf("%.2f", decay)
		}
		fmt.Printf("  Fold %d: IS Sharpe %+.4f  OOS Sharpe %+.4f  decay %s\n",
			f.Idx, f.TrainSharpe, f.TestSharpe, decayStr)
	}
	fmt.Println("─── OOS distribution across folds ───")
	fmt.Printf("OOS Sharpe:  mean %+.4f  std %.4f\n", meanOOS, stdOOS)
	fmt.Printf("OOS PnL:     mean %+.4f  std %.4f\n", meanPnL, stdPnL)

	// Confidence-aware read: how many folds held a positive OOS Sharpe, and
	// whether the mean OOS Sharpe clears its own cross-fold dispersion (a rough
	// signal-to-noise check). No verdict — context for the reader.
	positive := 0
	for _, s := range oosSharpes {
		if s > 0 {
			positive++
		}
	}
	snr := math.NaN()
	if stdOOS > 0 {
		snr = meanOOS / stdOOS
	}
	fmt.Println("─── Confidence ───")
	fmt.Printf("OOS folds with positive Sharpe: %d/%d\n", positive, len(folds))
	if math.IsNaN(snr) {
		fmt.Println("Mean/std (OOS Sharpe SNR):      n/a (zero cross-fold dispersion)")
	} else {
		fmt.Printf("Mean/std (OOS Sharpe SNR):      %+.2f  (higher = more consistent edge)\n", snr)
	}
	fmt.Printf("Reminder: %d combos searched/fold — treat the best as upward-biased.\n", numCombos)

	// ─── Overfitting-aware statistics (Bailey & López de Prado) ───
	// Deflated Sharpe Ratio per fold: the selected config's OOS Sharpe, deflated
	// by the number of trials (numCombos) searched in-sample, reported with the
	// implied P(true Sharpe ≤ 0). The Sharpe is converted from the engine's
	// annualized figure back to per-observation units via the OOS moments.
	fmt.Println("─── Deflated Sharpe (selected config, deflated by trials) ───")
	fmt.Printf("Trials (combos) per fold: %d\n", numCombos)
	for _, f := range folds {
		m := f.TestMoments
		if math.IsNaN(m.PerObsSharpe) || m.NObs < 2 {
			fmt.Printf("  Fold %d: DSR n/a (insufficient OOS return observations)\n", f.Idx)
			continue
		}
		dsr := metrics.DeflatedSharpeRatio(m.PerObsSharpe, numCombos, m.Skew, m.Kurtosis, m.NObs)
		if math.IsNaN(dsr) {
			fmt.Printf("  Fold %d: DSR n/a (undefined: degenerate higher moments)\n", f.Idx)
			continue
		}
		pNonPos := metrics.ImpliedPTrueSharpeNonPositive(dsr)
		fmt.Printf("  Fold %d: DSR %.4f  P(true Sharpe ≤ 0) %.4f  (OOS obs %.0f)\n",
			f.Idx, dsr, pNonPos, m.NObs)
	}

	// Probability of Backtest Overfitting across all folds, from the OOS Sharpe
	// matrix (CSCV). High PBO ⇒ the in-sample winner tends to be a coin-flip (or
	// worse) out of sample.
	pbo := metrics.ProbabilityBacktestOverfitting(oosMatrix)
	fmt.Println("─── Probability of Backtest Overfitting (CSCV) ───")
	if math.IsNaN(pbo) {
		fmt.Printf("PBO: n/a (need ≥2 folds and ≥2 configs; have %d folds, %d configs)\n",
			len(oosMatrix), oosConfigCount(oosMatrix))
	} else {
		fmt.Printf("PBO: %.4f  (fraction of folds where the IS-best config was OOS bottom-half)\n", pbo)
	}
	fmt.Println("════════════════════════════")
}

// oosConfigCount returns the config count (row width) of the OOS matrix, or 0
// when the matrix is empty — used only for the PBO "n/a" diagnostic line.
func oosConfigCount(m [][]float64) int {
	if len(m) == 0 {
		return 0
	}
	return len(m[0])
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
		"train_sharpe", "test_sharpe", "sharpe_decay_ratio", "test_max_drawdown",
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
			strconv.FormatFloat(sharpeDecayRatio(fold.TrainSharpe, fold.TestSharpe), 'f', 4, 64),
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
