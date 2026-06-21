package main

import (
	"math"
	"strings"
	"testing"
)

func almostEqual(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestLinspace(t *testing.T) {
	got := linspace(0, 10, 6)
	want := []float64{0, 2, 4, 6, 8, 10}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if !almostEqual(got[i], want[i]) {
			t.Errorf("linspace[%d] = %v, want %v", i, got[i], want[i])
		}
	}
	if n := linspace(5, 9, 1); len(n) != 1 || n[0] != 5 {
		t.Errorf("n==1 case = %v, want [5]", n)
	}
	if n := linspace(0, 1, 0); n != nil {
		t.Errorf("n<1 case = %v, want nil", n)
	}
}

func TestParseHurdles(t *testing.T) {
	if got, _ := parseHurdles(""); len(got) != 1 || got[0] != 0 {
		t.Errorf("empty = %v, want [0]", got)
	}
	got, err := parseHurdles("0,1.5,3")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	want := []float64{0, 1.5, 3}
	for i := range want {
		if !almostEqual(got[i], want[i]) {
			t.Errorf("parseHurdles[%d] = %v, want %v", i, got[i], want[i])
		}
	}
	if _, err := parseHurdles("0,abc"); err == nil {
		t.Error("expected error on bad token")
	}
	// Trailing/empty tokens are skipped.
	if got, _ := parseHurdles("1,,2,"); len(got) != 2 {
		t.Errorf("skip-empty = %v, want len 2", got)
	}
}

func TestInterpZero(t *testing.T) {
	// PnL +10 at cost 4, -10 at cost 6 → break-even at cost 5.
	if got := interpZero(4, 10, 6, -10); !almostEqual(got, 5) {
		t.Errorf("interpZero = %v, want 5", got)
	}
	// Crossing nearer the low end.
	if got := interpZero(0, 30, 10, -10); !almostEqual(got, 7.5) {
		t.Errorf("interpZero = %v, want 7.5", got)
	}
	// Degenerate flat segment → midpoint.
	if got := interpZero(2, 5, 8, 5); !almostEqual(got, 5) {
		t.Errorf("interpZero degenerate = %v, want 5 (midpoint)", got)
	}
}

func TestStraddlesZero(t *testing.T) {
	cases := []struct {
		a, b float64
		want bool
	}{
		{10, -5, true},
		{-3, 4, true},
		{5, 5, false},
		{-1, -2, false},
		{10, 0, true},  // touching zero counts
		{0, -4, true},
	}
	for _, c := range cases {
		if got := straddlesZero(c.a, c.b); got != c.want {
			t.Errorf("straddlesZero(%v,%v) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestAggregate_BreakEvenAndBestK(t *testing.T) {
	// Single baseline column (K=0): net PnL declines through zero between cost
	// 5 and cost 10. Linear interp: +5 at 5, -5 at 10 → break-even at 7.5.
	// A second column K=2 has a higher NetSharpe somewhere → best K = 2.
	points := []SweepPoint{
		{TxCostBps: 0, HurdleK: 0, NetSharpe: 1.0, NetPnL: 20},
		{TxCostBps: 5, HurdleK: 0, NetSharpe: 0.6, NetPnL: 5},
		{TxCostBps: 10, HurdleK: 0, NetSharpe: 0.1, NetPnL: -5},
		{TxCostBps: 0, HurdleK: 2, NetSharpe: 1.4, NetPnL: 18}, // best NetSharpe
		{TxCostBps: 5, HurdleK: 2, NetSharpe: 0.9, NetPnL: 7},
		{TxCostBps: 10, HurdleK: 2, NetSharpe: 0.2, NetPnL: 1},
	}
	res := Aggregate(points)

	if !res.BreakEvenFound {
		t.Fatal("expected break-even to be found")
	}
	if !almostEqual(res.BreakEvenCost, 7.5) {
		t.Errorf("BreakEvenCost = %v, want 7.5", res.BreakEvenCost)
	}
	if res.BestHurdleK != 2 {
		t.Errorf("BestHurdleK = %v, want 2", res.BestHurdleK)
	}
	if !almostEqual(res.BestHurdleNetSharpe, 1.4) {
		t.Errorf("BestHurdleNetSharpe = %v, want 1.4", res.BestHurdleNetSharpe)
	}
}

func TestAggregate_NoBreakEven(t *testing.T) {
	// Net PnL stays positive across the whole baseline column → no crossing.
	points := []SweepPoint{
		{TxCostBps: 0, HurdleK: 0, NetSharpe: 1.0, NetPnL: 20},
		{TxCostBps: 5, HurdleK: 0, NetSharpe: 0.8, NetPnL: 12},
		{TxCostBps: 10, HurdleK: 0, NetSharpe: 0.5, NetPnL: 3},
	}
	res := Aggregate(points)
	if res.BreakEvenFound {
		t.Errorf("expected no break-even, got %v", res.BreakEvenCost)
	}
	if res.BestHurdleK != 0 {
		t.Errorf("BestHurdleK = %v, want 0", res.BestHurdleK)
	}
}

func TestAggregate_BaselineColumnIsLowestK(t *testing.T) {
	// Break-even must be computed on the LOWEST-K column even when that column
	// is listed after a higher-K one. K=1 column crosses zero between 4 and 8
	// (+4 → -4 ⇒ 6). The K=5 column never crosses; it must be ignored.
	points := []SweepPoint{
		{TxCostBps: 4, HurdleK: 5, NetSharpe: 0.2, NetPnL: 50},
		{TxCostBps: 8, HurdleK: 5, NetSharpe: 0.1, NetPnL: 40},
		{TxCostBps: 4, HurdleK: 1, NetSharpe: 0.9, NetPnL: 4},
		{TxCostBps: 8, HurdleK: 1, NetSharpe: 0.7, NetPnL: -4},
	}
	res := Aggregate(points)
	if !res.BreakEvenFound || !almostEqual(res.BreakEvenCost, 6) {
		t.Errorf("BreakEvenCost = %v (found=%v), want 6", res.BreakEvenCost, res.BreakEvenFound)
	}
}

func TestAggregate_Empty(t *testing.T) {
	res := Aggregate(nil)
	if res.BreakEvenFound || len(res.Points) != 0 {
		t.Errorf("empty aggregate misbehaved: %+v", res)
	}
}

func TestRenderSVG_WellFormed(t *testing.T) {
	points := []SweepPoint{
		{TxCostBps: 0, HurdleK: 0, NetSharpe: 1.0, NetPnL: 10, Turnover: 5},
		{TxCostBps: 10, HurdleK: 0, NetSharpe: -0.2, NetPnL: -3, Turnover: 4},
		{TxCostBps: 0, HurdleK: 2, NetSharpe: 1.2, NetPnL: 9, Turnover: 2},
		{TxCostBps: 10, HurdleK: 2, NetSharpe: 0.3, NetPnL: 2, Turnover: 1},
	}
	svg := RenderSVG("obi", Aggregate(points))
	if !strings.HasPrefix(svg, "<svg") || !strings.HasSuffix(svg, "</svg>") {
		t.Fatal("output is not a well-formed SVG envelope")
	}
	for _, want := range []string{"Cost Sweep", "Net Sharpe", "turnover", "break-even", "polyline", "K=0.00", "K=2.00"} {
		if !strings.Contains(svg, want) {
			t.Errorf("SVG missing %q", want)
		}
	}
	// Roughly balanced angle brackets ⇒ no obviously truncated tag.
	if strings.Count(svg, "<") != strings.Count(svg, ">") {
		t.Error("unbalanced angle brackets in SVG")
	}
}

func TestPadRange(t *testing.T) {
	if lo, hi := padRange(2, 8); lo != 2 || hi != 8 {
		t.Errorf("padRange(2,8) = %v,%v want 2,8", lo, hi)
	}
	if lo, hi := padRange(0, 0); lo != -1 || hi != 1 {
		t.Errorf("padRange(0,0) = %v,%v want -1,1", lo, hi)
	}
	lo, hi := padRange(5, 5)
	if !(lo < 5 && hi > 5) {
		t.Errorf("padRange(5,5) = %v,%v want a band around 5", lo, hi)
	}
}
