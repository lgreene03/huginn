package research

import (
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/lgreene03/huginn/internal/config"
	"github.com/lgreene03/huginn/internal/model"
)

// testConfig is a self-contained config (no YAML on disk) for the OBI strategy.
func testConfig() *config.Config {
	c := &config.Config{}
	c.Strategy.Name = "obi"
	c.Strategy.Threshold = 0.5
	c.Strategy.OrderSize = 0.01
	c.Strategy.FastPeriod = 10
	c.Strategy.SlowPeriod = 30
	c.Capital.InitialCash = 100_000
	return c
}

// syntheticEvents builds n deterministic one-minute BTC feature events, all on
// the SAME calendar day, with obi alternating strong +/-. Because the whole
// series sits in one day, each fold's equity curve collapses to a single daily
// sample → fewer than 2 return observations → the fold deflated Sharpe is
// undefined for every fold (the case the test pins). The data is fully
// deterministic, so the fold count, profitability count, and fold PBO proxy are
// stable across runs.
func syntheticEvents(n int) []model.FeatureEvent {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	ev := make([]model.FeatureEvent, 0, n)
	price := 50000.0
	for i := 0; i < n; i++ {
		obi := 0.9
		if i%2 == 0 {
			obi = -0.9
		}
		price += float64(i%7) - 3
		ev = append(ev, model.FeatureEvent{
			EventTime:  base.Add(time.Duration(i) * time.Minute),
			Instrument: "BTC-USD",
			Values: map[string]float64{
				"microPrice": price, "obi": obi, "vpin": 0.3,
				"vwap": price, "volume": 10,
			},
		})
	}
	return ev
}

// TestRunDeterministicFoldsAndUndefinedFoldDeflatedSharpe pins the walk-forward
// window math and the undefined fold deflated Sharpe handling on a tiny
// synthetic dataset.
//
//   - 120 events, testPct 0.2 → testSize 24; step = (120-24)/3 = 32 ≥ testSize,
//     so all 3 requested folds materialise, each with a 24-event test window.
//   - The whole series is one calendar day → every fold's equity curve has <2
//     return observations → FoldDeflatedSharpe is NaN per fold and the
//     aggregate Result.MeanFoldDeflatedSharpe stays nil (never a misleading 0).
func TestRunDeterministicFoldsAndUndefinedFoldDeflatedSharpe(t *testing.T) {
	cfg := testConfig()
	events := syntheticEvents(120)
	grid := BuildGrid(cfg, []float64{0.5, 0.7}, nil) // 2 configs → the fold PBO proxy is defined

	res, err := Run(Options{
		Config:  cfg,
		Events:  events,
		Folds:   3,
		TestPct: 0.2,
		Grid:    grid,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(res.Folds) != 3 {
		t.Fatalf("folds = %d, want 3", len(res.Folds))
	}
	if res.GridSize != 2 {
		t.Errorf("gridSize = %d, want 2", res.GridSize)
	}
	for i, f := range res.Folds {
		if f.Fold != i+1 {
			t.Errorf("fold[%d].Fold = %d, want %d", i, f.Fold, i+1)
		}
		if f.TestEventCount != 24 {
			t.Errorf("fold[%d].TestEventCount = %d, want 24", i, f.TestEventCount)
		}
		if f.CombosSearched != 2 {
			t.Errorf("fold[%d].CombosSearched = %d, want 2", i, f.CombosSearched)
		}
		// One-day window → undefined fold deflated Sharpe.
		if !math.IsNaN(f.FoldDeflatedSharpe) {
			t.Errorf("fold[%d].FoldDeflatedSharpe = %v, want NaN (one-day OOS window)", i, f.FoldDeflatedSharpe)
		}
	}

	// Undefined fold deflated Sharpe across every fold → the mean stays nil
	// (not 0).
	if res.MeanFoldDeflatedSharpe != nil {
		t.Errorf("Result.MeanFoldDeflatedSharpe = %v, want nil", *res.MeanFoldDeflatedSharpe)
	}

	// Flat OBI fills on this fixture → zero realized PnL, so no fold is
	// profitable. Deterministic and known.
	if res.OOSFoldsProfitable != 0 {
		t.Errorf("OOSFoldsProfitable = %d, want 0", res.OOSFoldsProfitable)
	}

	// The fold PBO proxy is defined (≥2 folds, ≥2 configs). With identical OOS
	// Sharpes across configs, the IS-best config (index 0 by tie-break) ranks
	// worst OOS in every fold → every fold is flagged overfit → proxy = 1.0.
	if math.IsNaN(res.FoldPBOProxy) {
		t.Fatalf("FoldPBOProxy = NaN, want a defined value")
	}
	if res.FoldPBOProxy != 1.0 {
		t.Errorf("FoldPBOProxy = %v, want 1.0", res.FoldPBOProxy)
	}
}

// TestFoldResultMarshalsNaNAsNull guards the artefact contract: NaN metric
// fields (here the undefined fold deflated Sharpe) serialize as JSON null, never
// as a value that would fail the encode or fabricate a number. This is the same
// null-for-undefined handling the server's validationHandler relies on when
// reading the persisted artefact.
func TestFoldResultMarshalsNaNAsNull(t *testing.T) {
	f := FoldResult{
		Fold:               1,
		FoldDeflatedSharpe: math.NaN(),
		Sharpe:             math.Inf(1),
		TrainSharpe:        1.25,
	}
	data, err := json.Marshal(f)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if v, present := got["fold_deflated_sharpe"]; !present || v != nil {
		t.Errorf("fold_deflated_sharpe = %v (present %v), want a present null", v, present)
	}
	if got["sharpe"] != nil {
		t.Errorf("sharpe = %v, want null (was +Inf)", got["sharpe"])
	}
	if v, ok := got["train_sharpe"].(float64); !ok || v != 1.25 {
		t.Errorf("train_sharpe = %v, want 1.25", got["train_sharpe"])
	}
	// json:"-" window-count fields must NOT appear (keeps the artifact unchanged).
	if _, present := got["TrainEventCount"]; present {
		t.Errorf("TrainEventCount leaked into JSON; must be omitted")
	}
}

// TestFoldResultJSONUsesFoldQualifiedDeflatedSharpeKey pins the per-fold key of
// the walk-forward artefact (cmd/walkforward writes these records to
// data/walkforward_results.json, and the research gateway returns them). The
// figure is a deflated Sharpe applied to one fold's OOS returns, not the
// evaluation gate's deflated Sharpe (norse-stack research/norse_research, goal
// G1.3), so the key is fold_deflated_sharpe and never deflated_sharpe.
func TestFoldResultJSONUsesFoldQualifiedDeflatedSharpeKey(t *testing.T) {
	data, err := json.Marshal(FoldResult{Fold: 1})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, present := got["fold_deflated_sharpe"]; !present {
		t.Errorf("per-fold JSON lacks fold_deflated_sharpe; got %s", data)
	}
	if _, present := got["deflated_sharpe"]; present {
		t.Errorf("per-fold JSON carries the unqualified key deflated_sharpe; got %s", data)
	}
}

// tradingConfig is testConfig with a risk policy loose enough that fills are
// actually accepted. The bare testConfig leaves RiskConfig zero-valued, which
// means PositionLimitHard is 0 and the risk manager throttles every order, so a
// fold built on it never produces a single fill and can say nothing about
// fill-derived metrics.
func tradingConfig() *config.Config {
	c := testConfig()
	c.Risk.MaxDrawdownPct = 0.5
	c.Risk.DailyLossLimit = 1e9
	c.Risk.PositionLimitHard = 500_000
	return c
}

// hitRateEvents builds n one-minute events whose OBI flips every 3 bars (so
// positions open and close) over a sawtooth price path that makes some round
// trips win and others lose. Fully deterministic.
func hitRateEvents(n int) []model.FeatureEvent {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	ev := make([]model.FeatureEvent, 0, n)
	price := 50000.0
	for i := 0; i < n; i++ {
		obi := 0.9
		if (i/3)%2 == 0 {
			obi = -0.9
		}
		switch i % 4 {
		case 0:
			price += 60
		case 1:
			price -= 20
		case 2:
			price += 15
		case 3:
			price -= 45
		}
		ev = append(ev, model.FeatureEvent{
			EventTime:  base.Add(time.Duration(i) * time.Minute),
			Instrument: "BTC-USD",
			Values: map[string]float64{
				"microPrice": price, "obi": obi, "vpin": 0.3,
				"vwap": price, "volume": 10,
			},
		})
	}
	return ev
}

// risingEvents is hitRateEvents' price path replaced by a monotonic ramp, so
// every closed round trip clears costs and wins. Same flip cadence, so the fill
// count matches; only the outcome of each round trip differs.
func risingEvents(n int) []model.FeatureEvent {
	ev := hitRateEvents(n)
	price := 50000.0
	for i := range ev {
		price += 50
		ev[i].Values["microPrice"] = price
		ev[i].Values["vwap"] = price
	}
	return ev
}

// TestRunFoldComputesHitRateFromFills guards a regression in which runFold
// returned a hardcoded hitRate of 0. That zero flowed into FoldResult.HitRate
// and printed as "Hit: 0.0%" next to genuinely computed metrics, so a reader
// reasonably concluded the strategy won none of its trades.
//
// The fix routes the figure through metrics.HitRate over the portfolio's own
// fill ledger, which is how cmd/backtest and cmd/calibrate already compute it.
// Two fixtures share one code path and one fill count but have different known
// hit rates, so no constant can satisfy both assertions:
//
//   - a sawtooth path where exactly one round trip in three wins
//   - a monotonic ramp where every round trip wins
func TestRunFoldComputesHitRateFromFills(t *testing.T) {
	cfg := tradingConfig()
	params := Params{Threshold: 0.5, OrderSize: 0.01}

	mixed := runFold(cfg, hitRateEvents(120), params)
	if mixed.fills == 0 {
		t.Fatal("fixture produced no fills; hit rate would be vacuously 0")
	}
	if got, want := mixed.hitRate, 1.0/3.0; math.Abs(got-want) > 1e-9 {
		t.Errorf("mixed fixture hitRate = %v, want %v", got, want)
	}

	rising := runFold(cfg, risingEvents(120), params)
	if rising.fills != mixed.fills {
		t.Fatalf("fixtures disagree on fill count: rising %d, mixed %d", rising.fills, mixed.fills)
	}
	if got := rising.hitRate; math.Abs(got-1.0) > 1e-9 {
		t.Errorf("rising fixture hitRate = %v, want 1.0", got)
	}
}
