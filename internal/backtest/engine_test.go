package backtest

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/lgreene03/huginn/internal/config"
	"github.com/lgreene03/huginn/internal/executor"
	"github.com/lgreene03/huginn/internal/model"
	"github.com/lgreene03/huginn/internal/portfolio"
	"github.com/lgreene03/huginn/internal/risk"
)

// noopStrategy emits no orders; keeps the backtest engine test free of
// strategy-specific signal logic so we can focus on the equity-curve sampler.
type noopStrategy struct{}

func (n *noopStrategy) Name() string                                   { return "noop" }
func (n *noopStrategy) OnFeature(_ model.FeatureEvent) []model.Order   { return nil }

// writeJSONLEvents encodes events as JSONL into a temp file and returns the path.
func writeJSONLEvents(t *testing.T, events []model.FeatureEvent) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "backtest-*.jsonl")
	if err != nil {
		t.Fatalf("create temp file: %v", err)
	}
	enc := json.NewEncoder(f)
	for _, ev := range events {
		if err := enc.Encode(ev); err != nil {
			t.Fatalf("encode event: %v", err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close temp file: %v", err)
	}
	return f.Name()
}

// buildEngine returns a minimal Engine suitable for unit tests: noop strategy,
// fresh portfolio, nil journal (executor handles nil gracefully), permissive risk.
func buildEngine(t *testing.T) *Engine {
	t.Helper()
	port := portfolio.New(10_000)
	rm := risk.NewManager(config.RiskConfig{
		MaxDrawdownPct:   0.99,
		DailyLossLimit:   9_900,
		PositionLimitHard: 100,
		StalenessTimeout: 0, // disable staleness watchdog in tests
	}, 10_000)
	exec := executor.New(
		&noopStrategy{}, port, nil, rm,
		executor.Config{SlippageBps: 0, TransactionCostBps: 0},
		false, nil, "",
	)
	return NewEngine(exec, port, nil, rm)
}

// TestEquityCurveYearBoundary is the regression test for the YearDay-only day
// comparison. YearDay() returns 1–366 and wraps at year boundaries: Jan 1 2024
// and Jan 1 2025 both return 1, so a naive comparison misses the boundary and
// collapses a multi-year backtest into a single equity sample. The fix encodes
// (year, yday) as year*1000+yday, which is globally unique.
func TestEquityCurveYearBoundary(t *testing.T) {
	t.Parallel()

	events := []model.FeatureEvent{
		{
			EventID:     "evt-2024",
			EventTime:   time.Date(2024, 1, 1, 10, 0, 0, 0, time.UTC),
			FeatureName: "obi",
			Instrument:  "BTC-USD",
			Values:      map[string]float64{},
		},
		{
			EventID:     "evt-2025",
			EventTime:   time.Date(2025, 1, 1, 10, 0, 0, 0, time.UTC),
			FeatureName: "obi",
			Instrument:  "BTC-USD",
			Values:      map[string]float64{},
		},
	}

	eng := buildEngine(t)
	path := writeJSONLEvents(t, events)

	if err := eng.Run(path); err != nil {
		t.Fatalf("Run: %v", err)
	}

	curve := eng.EquityCurve()
	// Two distinct calendar days → one mid-stream snapshot (on crossing from
	// 2024-01-01 to 2025-01-01) plus one final snapshot = 2 total.
	// Before the fix: both events share YearDay()=1 so the boundary is never
	// detected; only the final snapshot lands → len == 1.
	if len(curve) != 2 {
		t.Errorf("equity curve len = %d, want 2; year-boundary not detected (pre-fix behaviour)", len(curve))
	}
}

// TestMultiStrategySharedPortfolio verifies that AddExecutor causes a second
// strategy's fills to land in the same portfolio as the first strategy.
//
// Setup: two OBI strategies, one long-biased (threshold 0.3) and one
// short-biased (threshold 0.3 with opposing signal values). Both share the
// same portfolio; every fill from either strategy changes the shared book.
// We verify that the total fill count (TotalFills on the snapshot) is the
// sum of both strategies' individual fill counts, not just one of them.
func TestMultiStrategySharedPortfolio(t *testing.T) {
	t.Parallel()

	const initialCash = 100_000.0
	port := portfolio.New(initialCash)
	rm := risk.NewManager(config.RiskConfig{
		MaxDrawdownPct:    0.99,
		DailyLossLimit:    99_000,
		PositionLimitHard: 1_000,
		StalenessTimeout:  0,
	}, initialCash)

	// OBI strategy A: fires on events where obi >= 0.6
	stratA := &thresholdSignalStrategy{threshold: 0.6, orderSize: 0.001}
	execA := executor.New(
		stratA, port, nil, rm,
		executor.Config{SlippageBps: 5, TransactionCostBps: 2},
		false, nil, "",
	)

	// OBI strategy B: fires on events where obi >= 0.6 (same signal, distinct executor)
	stratB := &thresholdSignalStrategy{threshold: 0.6, orderSize: 0.001}
	execB := executor.New(
		stratB, port, nil, rm,
		executor.Config{SlippageBps: 5, TransactionCostBps: 2},
		false, nil, "",
	)

	eng := NewEngine(execA, port, nil, rm)
	eng.AddExecutor(execB)

	// Build 4 events: two with obi above threshold (triggers both strategies)
	// and two below (no fills). Each above-threshold event should produce 2
	// fills — one from stratA and one from stratB.
	t0 := time.Date(2025, 1, 1, 9, 0, 0, 0, time.UTC)
	events := []model.FeatureEvent{
		{EventID: "e1", EventTime: t0, FeatureName: "obi", Instrument: "BTC-USD",
			Values: map[string]float64{"value": 50_000, "obi": 0.8}},  // above: 2 fills
		{EventID: "e2", EventTime: t0.Add(time.Minute), FeatureName: "obi", Instrument: "BTC-USD",
			Values: map[string]float64{"value": 50_010, "obi": 0.2}},  // below: 0 fills
		{EventID: "e3", EventTime: t0.Add(2 * time.Minute), FeatureName: "obi", Instrument: "BTC-USD",
			Values: map[string]float64{"value": 50_020, "obi": 0.9}},  // above: 2 fills
		{EventID: "e4", EventTime: t0.Add(3 * time.Minute), FeatureName: "obi", Instrument: "BTC-USD",
			Values: map[string]float64{"value": 50_030, "obi": 0.1}},  // below: 0 fills
	}

	path := writeJSONLEvents(t, events)
	if err := eng.Run(path); err != nil {
		t.Fatalf("Run: %v", err)
	}

	snap := port.Snapshot()
	// Two above-threshold events × 2 strategies = 4 fills.
	if snap.TotalFills != 4 {
		t.Errorf("TotalFills = %d, want 4 (2 events × 2 strategies)", snap.TotalFills)
	}
}

// thresholdSignalStrategy is a minimal test strategy that emits a BUY when
// event.Values["obi"] >= threshold.
type thresholdSignalStrategy struct {
	threshold float64
	orderSize float64
}

func (s *thresholdSignalStrategy) Name() string { return "threshold-signal" }
func (s *thresholdSignalStrategy) OnFeature(event model.FeatureEvent) []model.Order {
	obi, ok := event.Values["obi"]
	if !ok || obi < s.threshold {
		return nil
	}
	return []model.Order{{
		Instrument: event.Instrument,
		Side:       model.Buy,
		Quantity:   s.orderSize,
		Reason:     "obi signal",
		Timestamp:  event.EventTime,
	}}
}

// TestEquityCurveWithinSingleDay verifies that events in the same calendar day
// produce exactly one equity sample (the mandatory final snapshot).
func TestEquityCurveWithinSingleDay(t *testing.T) {
	t.Parallel()

	events := []model.FeatureEvent{
		{EventID: "a", EventTime: time.Date(2025, 6, 15, 9, 0, 0, 0, time.UTC), FeatureName: "obi", Instrument: "BTC-USD", Values: map[string]float64{}},
		{EventID: "b", EventTime: time.Date(2025, 6, 15, 15, 0, 0, 0, time.UTC), FeatureName: "obi", Instrument: "BTC-USD", Values: map[string]float64{}},
	}

	eng := buildEngine(t)
	path := writeJSONLEvents(t, events)

	if err := eng.Run(path); err != nil {
		t.Fatalf("Run: %v", err)
	}

	curve := eng.EquityCurve()
	if len(curve) != 1 {
		t.Errorf("equity curve len = %d, want 1 for single-day events", len(curve))
	}
}
