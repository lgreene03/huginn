// Package research holds the reusable walk-forward validation core extracted
// from cmd/walkforward. It runs an anchored walk-forward (expanding train
// window, sliding test window), selecting parameters in-sample on each fold's
// train window and scoring only the chosen params out-of-sample, and derives
// the overfitting-aware statistics (PBO, Deflated Sharpe) so heavy backtests
// can run OUT of the live trading process — both the CLI (cmd/walkforward) and
// the research gateway service (cmd/research) call Run.
//
// Behavior is identical to the routine that previously lived inline in
// cmd/walkforward/main.go: same window layout, same in-sample selection, same
// OOS scoring, same *float64-null-for-undefined-DSR handling — so the CLI's
// console output and the data/walkforward_results.json artifact it writes are
// byte-for-byte unchanged.
package research

import (
	"bufio"
	"encoding/json"
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

// FoldResult is one per-fold record. Field names + JSON tags are kept identical
// to the struct that previously lived in cmd/walkforward so the persisted
// artifact is byte-for-byte unchanged.
type FoldResult struct {
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

	// TrainEventCount / TestEventCount are the window sizes for this fold. They
	// are NOT persisted (json:"-") so the data/walkforward_results.json artifact
	// is byte-for-byte identical to the pre-refactor output; they exist only so
	// the CLI can reproduce its per-fold console line.
	TrainEventCount int `json:"-"`
	TestEventCount  int `json:"-"`
}

// nullIfNaN returns a *float64 that JSON-encodes as the value, or as null when
// the value is NaN/±Inf (those IEEE values are not representable in JSON).
func nullIfNaN(f float64) *float64 {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return nil
	}
	return &f
}

// MarshalJSON renders the NaN-capable metric fields (Sharpe family, decay,
// drawdown, hit-rate, deflated Sharpe) as JSON null instead of letting NaN/±Inf
// fail the whole encode. Without this, a single NaN (e.g. a deflated Sharpe that
// is undefined on a too-short OOS window) made encoding/json error, the error
// was discarded, and a 0-byte results file was written. The float64 struct
// fields are kept as-is so all compute/printf code is unaffected; the outer
// *float64 fields here shadow the embedded ones for serialization only.
func (r FoldResult) MarshalJSON() ([]byte, error) {
	type alias FoldResult
	return json.Marshal(struct {
		alias
		TrainSharpe            *float64 `json:"train_sharpe"`
		Sharpe                 *float64 `json:"sharpe"`
		SharpeDecay            *float64 `json:"sharpe_decay_ratio"`
		MaxDD                  *float64 `json:"max_dd"`
		HitRate                *float64 `json:"hit_rate"`
		DeflatedSharpe         *float64 `json:"deflated_sharpe"`
		PTrueSharpeNonPositive *float64 `json:"p_true_sharpe_non_positive"`
	}{
		alias:                  alias(r),
		TrainSharpe:            nullIfNaN(r.TrainSharpe),
		Sharpe:                 nullIfNaN(r.Sharpe),
		SharpeDecay:            nullIfNaN(r.SharpeDecay),
		MaxDD:                  nullIfNaN(r.MaxDD),
		HitRate:                nullIfNaN(r.HitRate),
		DeflatedSharpe:         nullIfNaN(r.DeflatedSharpe),
		PTrueSharpeNonPositive: nullIfNaN(r.PTrueSharpeNonPositive),
	})
}

// Options configures one walk-forward run.
type Options struct {
	Config  *config.Config       // strategy/capital/executor/risk config
	Events  []model.FeatureEvent // historical feature events, time-ordered
	Folds   int                  // number of walk-forward folds
	TestPct float64              // fraction of data per test window
	Grid    []Params             // parameter grid searched on each train window
}

// Result carries the per-fold records plus the derived aggregates the callers
// need: the CLI uses them for its summary, the service surfaces the
// console-contract subset (oosFoldsProfitable / totalOOSPnL / pbo /
// deflatedSharpe).
type Result struct {
	Folds []FoldResult
	// TotalOOSPnL is the summed out-of-sample PnL across folds.
	TotalOOSPnL float64
	// OOSFoldsProfitable is the count of folds with positive OOS PnL.
	OOSFoldsProfitable int
	// GridSize is the number of grid combos searched per fold (the trial count).
	GridSize int
	// OOSMatrix[fold][config] is each config's OOS Sharpe on that fold's test
	// window — feeds the CSCV Probability of Backtest Overfitting estimator.
	OOSMatrix [][]float64
	// PBO is the Probability of Backtest Overfitting (CSCV). NaN when there are
	// too few folds/configs to define it.
	PBO float64
	// DeflatedSharpe is the mean OOS Deflated Sharpe over the folds where it is
	// defined, or nil when no fold had enough OOS observations (mirrors the
	// *float64-null-for-undefined-DSR handling in internal/server/http.go's
	// validationHandler — never a misleading 0).
	DeflatedSharpe *float64
}

// Params is one point in the walk-forward grid: the threshold and order size to
// instantiate the configured strategy with. EMA crossover ignores threshold (it
// is driven by fast/slow periods from config) but still honours order size.
type Params struct {
	Threshold float64
	OrderSize float64
}

// foldMetrics carries the per-fold compute outputs.
type foldMetrics struct {
	pnl     float64
	fills   int
	sharpe  float64
	maxDD   float64
	hitRate float64
	// moments carries the per-observation return statistics of this fold's
	// equity curve so the caller can compute a Deflated Sharpe Ratio for the
	// selected config. The sharpe field above stays the annualized headline.
	moments metrics.ReturnMoments
}

// Run executes the anchored walk-forward and returns the per-fold records plus
// derived aggregates. It does no console I/O and writes no files — callers own
// presentation/persistence. The grid must be non-empty (use BuildGrid).
func Run(opts Options) (Result, error) {
	events := opts.Events
	cfg := opts.Config
	folds := opts.Folds
	grid := opts.Grid

	testSize := int(float64(len(events)) * opts.TestPct)
	if testSize < 10 {
		testSize = 10
	}

	results := make([]FoldResult, 0, folds)
	var totalOOS float64
	var oosWins int
	// oosMatrix[fold][config] = that config's out-of-sample Sharpe on the fold's
	// test window. Feeds the Probability of Backtest Overfitting (CSCV) estimator.
	var oosMatrix [][]float64

	step := (len(events) - testSize) / folds
	if step < testSize {
		step = testSize
	}

	for fold := 0; fold < folds; fold++ {
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

		r := FoldResult{
			Fold:                   fold + 1,
			TrainStart:             trainEvents[0].EventTime.Format("2006-01-02T15:04"),
			TrainEnd:               trainEvents[len(trainEvents)-1].EventTime.Format("2006-01-02T15:04"),
			TestStart:              testEvents[0].EventTime.Format("2006-01-02T15:04"),
			TestEnd:                testEvents[len(testEvents)-1].EventTime.Format("2006-01-02T15:04"),
			BestThreshold:          bestParams.Threshold,
			BestOrderSize:          bestParams.OrderSize,
			CombosSearched:         len(grid),
			TrainPnL:               best.pnl,
			TestPnL:                testResult.pnl,
			TrainFills:             best.fills,
			TestFills:              testResult.fills,
			TrainSharpe:            best.sharpe,
			Sharpe:                 testResult.sharpe,
			SharpeDecay:            SharpeDecayRatio(best.sharpe, testResult.sharpe),
			MaxDD:                  testResult.maxDD,
			HitRate:                testResult.hitRate,
			DeflatedSharpe:         dsr,
			PTrueSharpeNonPositive: pNonPos,
			TrainEventCount:        len(trainEvents),
			TestEventCount:         len(testEvents),
		}
		results = append(results, r)
		totalOOS += testResult.pnl
		if testResult.pnl > 0 {
			oosWins++
		}
	}

	// Mean Deflated Sharpe across folds where it is defined; nil when none are,
	// rather than a misleading 0 (mirrors validationHandler).
	var dsrSum float64
	var dsrN int
	for _, r := range results {
		if !math.IsNaN(r.DeflatedSharpe) {
			dsrSum += r.DeflatedSharpe
			dsrN++
		}
	}
	var deflated *float64
	if dsrN > 0 {
		avg := dsrSum / float64(dsrN)
		deflated = &avg
	}

	return Result{
		Folds:              results,
		TotalOOSPnL:        totalOOS,
		OOSFoldsProfitable: oosWins,
		GridSize:           len(grid),
		OOSMatrix:          oosMatrix,
		PBO:                metrics.ProbabilityBacktestOverfitting(oosMatrix),
		DeflatedSharpe:     deflated,
	}, nil
}

// BuildGrid expands the threshold/order-size lists into the cartesian product
// of params to search. Each empty list falls back to the single config value,
// so the default (no flags) yields exactly one combo — i.e. no real selection.
func BuildGrid(cfg *config.Config, thresholds, orderSizes []float64) []Params {
	if len(thresholds) == 0 {
		thresholds = []float64{cfg.Strategy.Threshold}
	}
	if len(orderSizes) == 0 {
		orderSizes = []float64{cfg.Strategy.OrderSize}
	}
	grid := make([]Params, 0, len(thresholds)*len(orderSizes))
	for _, th := range thresholds {
		for _, os := range orderSizes {
			grid = append(grid, Params{Threshold: th, OrderSize: os})
		}
	}
	return grid
}

// selectBestOnTrain evaluates every grid combo on the train window and returns
// the metrics + params of the highest in-sample Sharpe. NaN Sharpe is treated
// as -Inf so a degenerate combo never wins. The grid is guaranteed non-empty.
func selectBestOnTrain(cfg *config.Config, trainEvents []model.FeatureEvent, grid []Params) (foldMetrics, Params) {
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

// SharpeDecayRatio is OOS Sharpe / IS Sharpe — the fraction of the in-sample
// edge that survived out of sample. NaN when IS Sharpe ≤ 0 (no edge to decay).
func SharpeDecayRatio(isSharpe, oosSharpe float64) float64 {
	if isSharpe <= 0 {
		return math.NaN()
	}
	return oosSharpe / isSharpe
}

func runFold(cfg *config.Config, events []model.FeatureEvent, p Params) foldMetrics {
	port := portfolio.New(cfg.Capital.InitialCash)

	var s strategy.Strategy
	switch cfg.Strategy.Name {
	case "obi":
		s = strategy.NewOBIThreshold(p.Threshold, p.OrderSize, p.OrderSize*10)
	case "vpin":
		s = strategy.NewVPINBreakout(p.Threshold, p.OrderSize, time.Minute)
	case "vwap_deviation":
		s = strategy.NewVWAPDeviation(p.Threshold, p.OrderSize, p.OrderSize*10)
	case "ema_crossover":
		s = strategy.NewEMACrossover(cfg.Strategy.FastPeriod, cfg.Strategy.SlowPeriod, p.OrderSize, p.OrderSize*10)
	case "ou":
		// OU mean-reversion: the swept "threshold" is the |z| entry band; the
		// rolling OLS window is cfg.Strategy.SlowPeriod (matches cmd/huginn).
		s = strategy.NewOUReversion(cfg.Strategy.SlowPeriod, p.Threshold, p.OrderSize, p.OrderSize*10)
	case "composite":
		// Pluggable-alpha composite: same default weighted alpha set as
		// cmd/huginn. Threshold is the |combined score| entry band.
		s = strategy.NewCompositeStrategy(strategy.DefaultCompositeConfig(p.Threshold, p.OrderSize))
	default:
		s = strategy.NewOBIThreshold(p.Threshold, p.OrderSize, p.OrderSize*10)
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

// LoadEvents reads a JSONL file of FeatureEvents, skipping unparseable lines.
func LoadEvents(path string) ([]model.FeatureEvent, error) {
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
