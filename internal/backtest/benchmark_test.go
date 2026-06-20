package backtest

import (
	"math"
	"testing"
	"time"

	"github.com/lgreene03/huginn/internal/model"
)

func ev(inst string, day int, micro float64) model.FeatureEvent {
	return model.FeatureEvent{
		Instrument: inst,
		EventTime:  time.Date(2026, 1, day, 12, 0, 0, 0, time.UTC),
		Values:     map[string]float64{"microPrice": micro},
	}
}

func TestBenchmarkBuyHold_SingleInstrumentDoubles(t *testing.T) {
	// One instrument, price doubles from 100 → 200 over three days.
	events := []model.FeatureEvent{
		ev("BTC", 1, 100),
		ev("BTC", 2, 150),
		ev("BTC", 3, 200),
	}
	b := BenchmarkBuyHold(events, 1000)

	if b.Instruments != 1 {
		t.Fatalf("Instruments = %d, want 1", b.Instruments)
	}
	if math.Abs(b.Invested-1000) > 1e-9 {
		t.Errorf("Invested = %f, want 1000", b.Invested)
	}
	// Bought 10 units at 100; final mark 200 → 2000 → +100%.
	if math.Abs(b.TotalReturn-1.0) > 1e-9 {
		t.Errorf("TotalReturn = %f, want 1.0", b.TotalReturn)
	}
	last := b.Equity[len(b.Equity)-1]
	if math.Abs(last-2000) > 1e-9 {
		t.Errorf("final equity = %f, want 2000", last)
	}
}

func TestBenchmarkBuyHold_EqualNotionalSplit(t *testing.T) {
	// Two instruments, equal-notional split of 1000 → 500 each.
	// A doubles (100→200), B flat (50→50).
	events := []model.FeatureEvent{
		ev("A", 1, 100),
		ev("B", 1, 50),
		ev("A", 2, 200),
		ev("B", 2, 50),
	}
	b := BenchmarkBuyHold(events, 1000)
	if b.Instruments != 2 {
		t.Fatalf("Instruments = %d, want 2", b.Instruments)
	}
	// A: 5 units * 200 = 1000; B: 10 units * 50 = 500 → 1500 → +50%.
	if math.Abs(b.TotalReturn-0.5) > 1e-9 {
		t.Errorf("TotalReturn = %f, want 0.5", b.TotalReturn)
	}
}

func TestBenchmarkBuyHold_NoPriceableInstruments(t *testing.T) {
	events := []model.FeatureEvent{
		{Instrument: "X", EventTime: time.Now(), Values: map[string]float64{"obi": 0.7}},
	}
	b := BenchmarkBuyHold(events, 1000)
	if b.Instruments != 0 {
		t.Errorf("Instruments = %d, want 0", b.Instruments)
	}
	if b.TotalReturn != 0 {
		t.Errorf("TotalReturn = %f, want 0", b.TotalReturn)
	}
	if len(b.Equity) < 2 {
		t.Errorf("Equity len = %d, want >= 2 for defined downstream math", len(b.Equity))
	}
}

func TestExcessAndStrategyReturn(t *testing.T) {
	// Final value 1100 from initial cash 1000 => +10%, measured from the same
	// cost basis as the benchmark (not from a mid-window equity mark).
	b := Benchmark{TotalReturn: 0.04}
	if got := StrategyTotalReturn(1100, 1000); math.Abs(got-0.1) > 1e-9 {
		t.Errorf("StrategyTotalReturn = %f, want 0.1", got)
	}
	if got := ExcessReturn(1100, 1000, b); math.Abs(got-0.06) > 1e-9 {
		t.Errorf("ExcessReturn = %f, want 0.06", got)
	}
	// A losing run must read negative even if its first daily mark was already
	// depressed (the old equity[0] basis could flip this positive).
	if got := StrategyTotalReturn(89153.86, 100000); got >= 0 {
		t.Errorf("losing run StrategyTotalReturn = %f, want negative", got)
	}
}

func TestInformationRatio_ZeroVolatility(t *testing.T) {
	// Strategy and benchmark move identically every period → zero active
	// volatility → IR defined as 0 (no divide-by-zero blow-up).
	strat := []float64{1000, 1100, 1210}
	b := Benchmark{Equity: []float64{1000, 1100, 1210}}
	if got := InformationRatio(strat, b); got != 0 {
		t.Errorf("InformationRatio = %f, want 0 for identical curves", got)
	}
}

func TestInformationRatio_PositiveEdge(t *testing.T) {
	// Strategy beats a flat benchmark by a steady margin → positive IR.
	strat := []float64{1000, 1020, 1040, 1061}
	b := Benchmark{Equity: []float64{1000, 1000, 1000, 1000}}
	if got := InformationRatio(strat, b); got <= 0 {
		t.Errorf("InformationRatio = %f, want > 0", got)
	}
}
