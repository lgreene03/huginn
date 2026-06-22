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
// sample → fewer than 2 return observations → the Deflated Sharpe is undefined
// for every fold (the case the test pins). The data is fully deterministic, so
// the fold count, profitability count, and PBO are stable across runs.
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

// TestRunDeterministicFoldsAndUndefinedDSR pins the walk-forward window math and
// the undefined-DSR handling on a tiny synthetic dataset.
//
//   - 120 events, testPct 0.2 → testSize 24; step = (120-24)/3 = 32 ≥ testSize,
//     so all 3 requested folds materialise, each with a 24-event test window.
//   - The whole series is one calendar day → every fold's equity curve has <2
//     return observations → DeflatedSharpe is NaN per fold and the aggregate
//     Result.DeflatedSharpe stays nil (never a misleading 0).
func TestRunDeterministicFoldsAndUndefinedDSR(t *testing.T) {
	cfg := testConfig()
	events := syntheticEvents(120)
	grid := BuildGrid(cfg, []float64{0.5, 0.7}, nil) // 2 configs → PBO is defined

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
		// One-day window → undefined OOS Deflated Sharpe.
		if !math.IsNaN(f.DeflatedSharpe) {
			t.Errorf("fold[%d].DeflatedSharpe = %v, want NaN (one-day OOS window)", i, f.DeflatedSharpe)
		}
	}

	// Undefined DSR across every fold → the aggregate stays nil (not 0).
	if res.DeflatedSharpe != nil {
		t.Errorf("Result.DeflatedSharpe = %v, want nil", *res.DeflatedSharpe)
	}

	// Flat OBI fills on this fixture → zero realized PnL, so no fold is
	// profitable. Deterministic and known.
	if res.OOSFoldsProfitable != 0 {
		t.Errorf("OOSFoldsProfitable = %d, want 0", res.OOSFoldsProfitable)
	}

	// PBO is defined (≥2 folds, ≥2 configs). With identical OOS Sharpes across
	// configs, the IS-best config (index 0 by tie-break) ranks worst OOS in
	// every fold → every fold is flagged overfit → PBO = 1.0.
	if math.IsNaN(res.PBO) {
		t.Fatalf("PBO = NaN, want a defined value")
	}
	if res.PBO != 1.0 {
		t.Errorf("PBO = %v, want 1.0", res.PBO)
	}
}

// TestFoldResultMarshalsNaNAsNull guards the artifact contract: NaN metric
// fields (here the undefined Deflated Sharpe) serialize as JSON null, never as a
// value that would fail the encode or fabricate a number. This is the same
// *float64-null-for-undefined-DSR handling the server's validationHandler
// relies on when reading the persisted artifact.
func TestFoldResultMarshalsNaNAsNull(t *testing.T) {
	f := FoldResult{
		Fold:           1,
		DeflatedSharpe: math.NaN(),
		Sharpe:         math.Inf(1),
		TrainSharpe:    1.25,
	}
	data, err := json.Marshal(f)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["deflated_sharpe"] != nil {
		t.Errorf("deflated_sharpe = %v, want null", got["deflated_sharpe"])
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
