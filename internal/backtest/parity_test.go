package backtest

// TestBacktestEngineParity is the cross-path correctness proof for Phase 4.
//
// The backtest engine (Engine.Run) reads a JSONL file and feeds each event to
// executor.OnFeature. The "consumer path" in this test calls OnFeature directly
// in a loop from the same in-memory slice. Both paths must produce identical
// fill counts and realized PnL — any divergence means Engine.Run is either
// skipping, reordering, or double-dispatching events.
//
// The test uses the OBI threshold strategy with engineered events so fills are
// guaranteed: alternating obi = ±0.9 against a threshold of 0.5 crosses the
// signal boundary on every other event, generating predictable orders.

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"testing"
	"time"

	"github.com/lgreene03/huginn/internal/config"
	"github.com/lgreene03/huginn/internal/executor"
	"github.com/lgreene03/huginn/internal/model"
	"github.com/lgreene03/huginn/internal/portfolio"
	"github.com/lgreene03/huginn/internal/risk"
	"github.com/lgreene03/huginn/internal/strategy"
)

// buildParityPair returns two independent but identically-configured (engine,
// executor, portfolio) triples that start from the same clean state.
func buildParityPair(t *testing.T) (engA *Engine, portA *portfolio.Portfolio, engB *Engine, portB *portfolio.Portfolio) {
	t.Helper()

	makeSet := func() (*Engine, *portfolio.Portfolio) {
		port := portfolio.New(100_000)
		rm := risk.NewManager(config.RiskConfig{
			MaxDrawdownPct:    0.99,
			DailyLossLimit:    99_000,
			PositionLimitHard: 100,
			StalenessTimeout:  0,
		}, 100_000)
		strat := strategy.NewOBIThreshold(0.5, 0.01, 10.0)
		exec := executor.New(
			strat, port, nil, rm,
			executor.Config{SlippageBps: 5, TransactionCostBps: 2},
			false, nil, "",
		)
		eng := NewEngine(exec, port, nil, rm)
		return eng, port
	}

	engA, portA = makeSet()
	engB, portB = makeSet()
	return
}

// makeParityEvents builds n FeatureEvents with alternating obi values that
// reliably cross the ±0.5 threshold, along with a mid-price so simulateFill
// can compute a fill price. All events share the same instrument and a single
// calendar day so the equity curve emits exactly one sample.
func makeParityEvents(n int) []model.FeatureEvent {
	events := make([]model.FeatureEvent, n)
	base := time.Date(2025, 7, 1, 9, 0, 0, 0, time.UTC)
	for i := 0; i < n; i++ {
		obi := 0.9
		if i%2 == 1 {
			obi = -0.9
		}
		events[i] = model.FeatureEvent{
			EventID:     fmt.Sprintf("parity-evt-%04d", i),
			EventTime:   base.Add(time.Duration(i) * time.Minute),
			FeatureName: "obi",
			Instrument:  "BTC-USD",
			Values: map[string]float64{
				"obi":         obi,
				"micro_price": 50_000.0,
			},
		}
	}
	return events
}

// writeJSONLFile encodes events to a temp JSONL file and returns the path.
func writeJSONLFile(t *testing.T, events []model.FeatureEvent) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "parity-*.jsonl")
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

// TestBacktestEngineParity is the load-bearing proof that Engine.Run dispatches
// events identically to a direct OnFeature loop. It uses 100 engineered OBI
// events so fills are guaranteed; two independent execution stacks run the same
// events and must agree on fill count and realized PnL to 6 decimal places.
func TestBacktestEngineParity(t *testing.T) {
	t.Parallel()

	const eventCount = 100
	events := makeParityEvents(eventCount)
	path := writeJSONLFile(t, events)

	engA, portA, engB, portB := buildParityPair(t)

	// Path A: backtest Engine.Run from the JSONL file.
	if err := engA.Run(path); err != nil {
		t.Fatalf("engine A Run: %v", err)
	}

	// Path B: direct OnFeature loop (the reference path — no file I/O, no
	// JSON serialisation round-trip).
	for _, ev := range events {
		engB.execs[0].OnFeature(ev)
	}

	snapA := portA.Snapshot()
	snapB := portB.Snapshot()

	if snapA.TotalFills != snapB.TotalFills {
		t.Errorf("fill count mismatch: engine=%d direct=%d", snapA.TotalFills, snapB.TotalFills)
	}

	const tolerance = 1e-6
	if math.Abs(snapA.RealizedPnL-snapB.RealizedPnL) > tolerance {
		t.Errorf("realized PnL mismatch: engine=%.8f direct=%.8f (diff=%.2e)",
			snapA.RealizedPnL, snapB.RealizedPnL, math.Abs(snapA.RealizedPnL-snapB.RealizedPnL))
	}

	if math.Abs(snapA.Cash-snapB.Cash) > tolerance {
		t.Errorf("cash mismatch: engine=%.8f direct=%.8f", snapA.Cash, snapB.Cash)
	}

	// Sanity: at least some fills must have occurred so the test is not a
	// trivially-passing no-op. With 100 alternating OBI events and a 0.5
	// threshold, the strategy should produce many signals.
	if snapA.TotalFills == 0 {
		t.Error("parity test is a no-op: zero fills produced — check that OBI values cross the threshold")
	}
}

// TestBacktestEngineParity_LargeDataset repeats the parity check at 1 000
// events to catch any accumulated floating-point divergence or buffering
// artefact that only surfaces at scale.
func TestBacktestEngineParity_LargeDataset(t *testing.T) {
	t.Parallel()

	const eventCount = 1_000
	events := makeParityEvents(eventCount)
	path := writeJSONLFile(t, events)

	engA, portA, engB, portB := buildParityPair(t)

	if err := engA.Run(path); err != nil {
		t.Fatalf("engine A Run: %v", err)
	}
	for _, ev := range events {
		engB.execs[0].OnFeature(ev)
	}

	snapA := portA.Snapshot()
	snapB := portB.Snapshot()

	if snapA.TotalFills != snapB.TotalFills {
		t.Errorf("fill count mismatch at 1k events: engine=%d direct=%d",
			snapA.TotalFills, snapB.TotalFills)
	}

	const tolerance = 1e-6
	if math.Abs(snapA.RealizedPnL-snapB.RealizedPnL) > tolerance {
		t.Errorf("realized PnL mismatch at 1k events: engine=%.8f direct=%.8f (diff=%.2e)",
			snapA.RealizedPnL, snapB.RealizedPnL, math.Abs(snapA.RealizedPnL-snapB.RealizedPnL))
	}

	if snapA.TotalFills == 0 {
		t.Error("no fills in 1k-event parity test")
	}
}
