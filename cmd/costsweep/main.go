// Command costsweep runs a transaction-cost sensitivity sweep over a backtest.
//
// Given a FeatureEvent JSONL stream and a strategy config, it re-runs the
// backtest engine across a grid of transaction-cost-bps values (and, for the
// OBI strategy, optionally across cost_hurdle_k values), recording NET Sharpe
// (metrics.NetSharpe), net PnL, and turnover at each grid point. From the
// aggregate it identifies:
//
//   - the break-even cost: the transaction-cost-bps at which net PnL crosses
//     zero (linearly interpolated between the bracketing grid points), and
//   - the cost_hurdle_k that maximises NET Sharpe.
//
// It then renders a dependency-free, committable SVG with two panels: a
// net-Sharpe-vs-cost curve and a turnover-vs-net-Sharpe frontier.
//
// This is a standalone main package — it reuses internal/backtest,
// internal/executor and internal/metrics exactly as cmd/backtest does, and
// edits no shared files, so it cannot break other builds.
package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
	"sort"
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

// journalPath is the per-run JSONL sink. The sweep runs the engine many times;
// each run truncates and rewrites this file (NewJSONLWriter opens O_TRUNC) so
// fills never bleed across grid points.
const journalPath = "data/costsweep_trades.jsonl"

// SweepPoint is one grid cell: the inputs that were varied plus the cost-aware
// outputs the backtest produced at that point.
type SweepPoint struct {
	TxCostBps float64
	HurdleK   float64
	NetSharpe float64
	NetPnL    float64
	GrossPnL  float64
	Turnover  float64
	Fills     int
}

// SweepResult is the aggregate of every grid point plus the two headline
// conclusions: the cost at which the strategy stops making money, and the
// cost_hurdle_k that maximises risk-adjusted net return.
type SweepResult struct {
	Points []SweepPoint

	// BreakEvenCost is the transaction-cost-bps at which net PnL crosses zero,
	// linearly interpolated between the two bracketing grid points along the
	// baseline (lowest-hurdle) cost axis. BreakEvenFound is false when net PnL
	// never crosses zero across the swept range (always positive or always
	// non-positive).
	BreakEvenCost  float64
	BreakEvenFound bool

	// BestHurdleK is the cost_hurdle_k that achieved the highest NetSharpe over
	// the whole grid; BestHurdleNetSharpe is that Sharpe. When no hurdle axis is
	// swept these still report the single (K=0) column's best point.
	BestHurdleK         float64
	BestHurdleNetSharpe float64
}

func main() {
	configPath := flag.String("config", "configs/default.yaml", "Path to YAML config file")
	dataPath := flag.String("data", "", "Path to historical FeatureEvent JSONL data file")
	costMin := flag.Float64("cost-min", 0, "Minimum transaction-cost-bps to sweep")
	costMax := flag.Float64("cost-max", 20, "Maximum transaction-cost-bps to sweep")
	costSteps := flag.Int("cost-steps", 11, "Number of transaction-cost-bps grid points (>=2)")
	hurdleList := flag.String("hurdle-k", "", "Optional comma-separated cost_hurdle_k values to sweep (OBI only), e.g. 0,1,2,3")
	outPath := flag.String("out", "docs/costsweep.svg", "Output path for the committable SVG")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	if *dataPath == "" {
		slog.Error("Missing --data flag. A historical JSONL file is required for the cost sweep.")
		os.Exit(1)
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		slog.Error("Failed to load configuration", "error", err)
		os.Exit(1)
	}

	costs := linspace(*costMin, *costMax, *costSteps)
	if len(costs) < 2 {
		slog.Error("Need at least 2 cost grid points", "cost-steps", *costSteps)
		os.Exit(1)
	}

	hurdles, err := parseHurdles(*hurdleList)
	if err != nil {
		slog.Error("Invalid --hurdle-k list", "error", err)
		os.Exit(1)
	}
	if cfg.Strategy.Name != "obi" && len(hurdles) > 1 {
		// Only the OBI strategy carries a cost-hurdle gate; for others the K
		// axis is inert, so collapse it to a single column to avoid running
		// redundant identical backtests.
		slog.Warn("cost_hurdle_k only affects the obi strategy; collapsing hurdle axis", "strategy", cfg.Strategy.Name)
		hurdles = []float64{0}
	}

	var points []SweepPoint
	for _, k := range hurdles {
		for _, c := range costs {
			pt, rerr := runPoint(cfg, *dataPath, c, k)
			if rerr != nil {
				slog.Error("Sweep point failed", "tx_cost_bps", c, "hurdle_k", k, "error", rerr)
				os.Exit(1)
			}
			points = append(points, pt)
		}
	}

	result := Aggregate(points)

	// Terminal summary.
	fmt.Println("\n═══ Cost Sweep ═══")
	fmt.Printf("Strategy:   %s\n", cfg.Strategy.Name)
	fmt.Printf("Cost grid:  %.2f … %.2f bps  (%d steps)\n", *costMin, *costMax, len(costs))
	fmt.Printf("Hurdle K:   %v\n", hurdles)
	fmt.Println("─── points (tx_bps, K) → net_sharpe / net_pnl / turnover ───")
	for _, p := range result.Points {
		fmt.Printf("  (%6.2f, %4.2f)  netSharpe=%8.4f  netPnL=%+12.4f  turnover=%6.2fx  fills=%d\n",
			p.TxCostBps, p.HurdleK, p.NetSharpe, p.NetPnL, p.Turnover, p.Fills)
	}
	fmt.Println("─── conclusions ───")
	if result.BreakEvenFound {
		fmt.Printf("Break-even cost:  %.4f bps (net PnL crosses 0)\n", result.BreakEvenCost)
	} else {
		fmt.Println("Break-even cost:  none in swept range (net PnL never crosses 0)")
	}
	fmt.Printf("Best cost_hurdle_k: %.4f  (NetSharpe=%.4f)\n", result.BestHurdleK, result.BestHurdleNetSharpe)
	fmt.Println("══════════════════")

	svg := RenderSVG(cfg.Strategy.Name, result)
	if werr := os.WriteFile(*outPath, []byte(svg), 0o600); werr != nil {
		slog.Error("Failed to write SVG", "out", *outPath, "error", werr)
		os.Exit(1)
	}
	fmt.Printf("SVG written to %s\n", *outPath)
}

// runPoint runs one backtest at a fixed (txCostBps, hurdleK) and returns the
// cost-aware metrics. It wires the engine exactly as cmd/backtest does — fresh
// portfolio, risk manager, executor and engine per call — but overrides the
// executor's TransactionCostBps and, for OBI, attaches a CostHurdle with the
// given K. A fresh JSONL journal per call gives us the fills to compute the
// cost breakdown and turnover.
func runPoint(cfg *config.Config, dataPath string, txCostBps, hurdleK float64) (SweepPoint, error) {
	port := portfolio.New(cfg.Capital.InitialCash)

	activeStrategy, serr := buildStrategy(cfg, txCostBps, hurdleK)
	if serr != nil {
		return SweepPoint{}, serr
	}

	// NewJSONLWriter opens O_APPEND, so explicitly truncate the per-run journal
	// first. Without this each grid point appends to the previous one and
	// ReadFills re-reads the whole accumulating file — ballooning fill counts
	// and making every point's metrics wrong.
	if terr := os.Truncate(journalPath, 0); terr != nil && !os.IsNotExist(terr) {
		return SweepPoint{}, terr
	}
	jWriter, jerr := journal.NewJSONLWriter(journalPath)
	if jerr != nil {
		return SweepPoint{}, jerr
	}

	riskManager := risk.NewManager(cfg.Risk, cfg.Capital.InitialCash)

	exec := executor.New(activeStrategy, port, jWriter, riskManager, executor.Config{
		TransactionCostBps:  txCostBps,
		SlippageBps:         cfg.Executor.SlippageBps,
		SlippageImpactK:     cfg.Executor.SlippageImpactK,
		SlippageImpactScale: cfg.Executor.SlippageImpactScale,
		FillLatencyMs:       cfg.Executor.FillLatencyMs,
	}, false, nil, "")

	engine := backtest.NewEngine(exec, port, jWriter, riskManager)
	if rerr := engine.Run(dataPath); rerr != nil {
		_ = jWriter.Close()
		return SweepPoint{}, rerr
	}
	if cerr := jWriter.Close(); cerr != nil {
		return SweepPoint{}, cerr
	}

	equity := engine.EquityCurve()
	fills, ferr := journal.ReadFills(journalPath)
	if ferr != nil {
		return SweepPoint{}, ferr
	}

	cost := metrics.ComputeCostBreakdown(fills)
	return SweepPoint{
		TxCostBps: txCostBps,
		HurdleK:   hurdleK,
		NetSharpe: metrics.NetSharpe(equity, 0.0),
		NetPnL:    cost.NetPnL,
		GrossPnL:  cost.GrossPnL,
		Turnover:  metrics.Turnover(fills),
		Fills:     len(fills),
	}, nil
}

// buildStrategy mirrors cmd/backtest's strategy selection but additionally, for
// OBI, attaches a CostHurdle gate keyed to the swept cost so the gate's
// internal cost estimate agrees with the executor that fills it.
func buildStrategy(cfg *config.Config, txCostBps, hurdleK float64) (strategy.Strategy, error) {
	switch cfg.Strategy.Name {
	case "obi":
		obi := strategy.NewOBIThreshold(cfg.Strategy.Threshold, cfg.Strategy.OrderSize, cfg.Strategy.OrderSize*10)
		if hurdleK > 0 {
			obi.SetCostHurdle(&strategy.CostHurdle{
				K:                  hurdleK,
				TransactionCostBps: txCostBps,
				SlippageBps:        cfg.Executor.SlippageBps,
				Edge:               strategy.OBIEdgeModel{BpsPerUnit: cfg.Executor.OBIEdgeBpsPerUnit},
			})
		}
		return obi, nil
	case "vpin":
		return strategy.NewVPINBreakout(cfg.Strategy.Threshold, cfg.Strategy.OrderSize, time.Minute), nil
	case "vwap_deviation":
		return strategy.NewVWAPDeviation(cfg.Strategy.Threshold, cfg.Strategy.OrderSize, cfg.Strategy.OrderSize*10), nil
	case "ema_crossover":
		return strategy.NewEMACrossover(cfg.Strategy.FastPeriod, cfg.Strategy.SlowPeriod, cfg.Strategy.OrderSize, cfg.Strategy.OrderSize*10), nil
	default:
		return nil, fmt.Errorf("unknown strategy %q", cfg.Strategy.Name)
	}
}

// Aggregate folds the raw grid points into the headline conclusions. It is pure
// (no I/O) so it can be unit-tested directly.
//
//   - Break-even cost is found along the BASELINE hurdle column (the smallest
//     HurdleK present): walking cost ascending, the first adjacent pair whose
//     net PnL straddles zero is linearly interpolated to the zero crossing.
//   - Best hurdle K is the K of the grid point with the maximum NetSharpe over
//     the entire grid.
func Aggregate(points []SweepPoint) SweepResult {
	res := SweepResult{Points: append([]SweepPoint(nil), points...)}
	if len(points) == 0 {
		return res
	}

	// Best NetSharpe over the whole grid → best hurdle K.
	best := points[0]
	for _, p := range points[1:] {
		if p.NetSharpe > best.NetSharpe {
			best = p
		}
	}
	res.BestHurdleK = best.HurdleK
	res.BestHurdleNetSharpe = best.NetSharpe

	// Break-even along the baseline (lowest-K) column.
	baselineK := points[0].HurdleK
	for _, p := range points {
		if p.HurdleK < baselineK {
			baselineK = p.HurdleK
		}
	}
	var col []SweepPoint
	for _, p := range points {
		if p.HurdleK == baselineK {
			col = append(col, p)
		}
	}
	sort.Slice(col, func(i, j int) bool { return col[i].TxCostBps < col[j].TxCostBps })

	for i := 1; i < len(col); i++ {
		a, b := col[i-1], col[i]
		if straddlesZero(a.NetPnL, b.NetPnL) {
			res.BreakEvenCost = interpZero(a.TxCostBps, a.NetPnL, b.TxCostBps, b.NetPnL)
			res.BreakEvenFound = true
			break
		}
	}
	return res
}

// straddlesZero reports whether a sign change (inclusive of touching zero)
// occurs between two net-PnL samples, i.e. there is a zero crossing in [a,b].
func straddlesZero(a, b float64) bool {
	if a == 0 || b == 0 {
		return true
	}
	return (a > 0) != (b > 0)
}

// interpZero linearly interpolates the x (cost) at which y (net PnL) hits zero
// between points (x0,y0) and (x1,y1). Callers must ensure the segment straddles
// zero. If the two y-values are equal (degenerate flat-on-zero), it returns the
// midpoint to stay well-defined.
func interpZero(x0, y0, x1, y1 float64) float64 {
	if y1 == y0 {
		return (x0 + x1) / 2
	}
	t := -y0 / (y1 - y0)
	return x0 + t*(x1-x0)
}

// linspace returns n evenly spaced values from lo to hi inclusive. n<1 yields
// nil; n==1 yields [lo].
func linspace(lo, hi float64, n int) []float64 {
	if n < 1 {
		return nil
	}
	if n == 1 {
		return []float64{lo}
	}
	out := make([]float64, n)
	step := (hi - lo) / float64(n-1)
	for i := 0; i < n; i++ {
		out[i] = lo + step*float64(i)
	}
	return out
}

// parseHurdles parses a comma-separated K list. Empty input means "no hurdle
// sweep" → a single inert column at K=0.
func parseHurdles(s string) ([]float64, error) {
	if s == "" {
		return []float64{0}, nil
	}
	var out []float64
	start := 0
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == ',' {
			tok := s[start:i]
			start = i + 1
			if tok == "" {
				continue
			}
			var v float64
			if _, err := fmt.Sscanf(tok, "%g", &v); err != nil {
				return nil, fmt.Errorf("bad hurdle value %q: %w", tok, err)
			}
			out = append(out, v)
		}
	}
	if len(out) == 0 {
		out = []float64{0}
	}
	return out, nil
}
