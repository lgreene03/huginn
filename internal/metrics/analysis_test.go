package metrics

import (
	"math"
	"testing"
	"time"

	"github.com/lgreene03/huginn/internal/model"
)

func TestCalculateSharpeRatio(t *testing.T) {
	equity := []float64{100.0, 101.0, 102.01, 103.0301} // 1% steady growth
	sharpe := CalculateSharpeRatio(equity, 0.0)

	if sharpe <= 0 || math.IsNaN(sharpe) {
		t.Errorf("Expected positive sharpe ratio, got %v", sharpe)
	}

	if got := CalculateSharpeRatio([]float64{100, 100, 100}, 0); got != 0 {
		t.Errorf("flat equity should give 0 Sharpe, got %f", got)
	}
}

func TestCalculateSharpeRatioAnnualizationFactor(t *testing.T) {
	// The annualization factor must be applied as mean*P and stddev*sqrt(P),
	// so for a fixed series the Sharpe scales by sqrt(P). Compare the default
	// crypto cadence (365) against 252 and assert the exact ratio.
	equity := []float64{100.0, 101.0, 100.0, 102.0, 101.0, 103.0}

	s365 := CalculateSharpeRatioWithPeriods(equity, 0.0, 365)
	s252 := CalculateSharpeRatioWithPeriods(equity, 0.0, 252)

	if s252 == 0 {
		t.Fatalf("expected non-zero Sharpe for varied equity series")
	}
	// With zero risk-free rate, Sharpe = (mean/stddev) * sqrt(P), so the ratio
	// of the two is sqrt(365/252).
	wantRatio := math.Sqrt(365.0 / 252.0)
	if got := s365 / s252; math.Abs(got-wantRatio) > 1e-9 {
		t.Errorf("annualization factor wrong: s365/s252 = %v, want %v", got, wantRatio)
	}

	// The 2-arg default must use the crypto cadence (365).
	if def := CalculateSharpeRatio(equity, 0.0); math.Abs(def-s365) > 1e-12 {
		t.Errorf("default CalculateSharpeRatio should use 365 periods/year: got %v, want %v", def, s365)
	}
}

func TestCalculateSharpeRatioNonPositivePriorEquity(t *testing.T) {
	// A zero (and a negative) prior-equity point would divide by zero/negative
	// and produce Inf/NaN. The denominator guard must skip those steps and
	// still return a finite number.
	equity := []float64{0.0, 100.0, 101.0, 102.01, 103.0301}
	sharpe := CalculateSharpeRatio(equity, 0.0)
	if math.IsNaN(sharpe) || math.IsInf(sharpe, 0) {
		t.Errorf("zero prior equity must not produce Inf/NaN, got %v", sharpe)
	}

	withNeg := []float64{100.0, -5.0, 101.0, 102.01, 103.0301}
	sharpeNeg := CalculateSharpeRatio(withNeg, 0.0)
	if math.IsNaN(sharpeNeg) || math.IsInf(sharpeNeg, 0) {
		t.Errorf("negative prior equity must not produce Inf/NaN, got %v", sharpeNeg)
	}

	// All-non-positive prior equity → no usable return steps → 0, not NaN.
	if got := CalculateSharpeRatio([]float64{0.0, 0.0}, 0.0); got != 0 {
		t.Errorf("no positive prior equity should yield 0 Sharpe, got %v", got)
	}

	// Normal positive series must be unaffected by the guard: a clean steady
	// series gives the same answer whether or not the guard exists.
	clean := []float64{100.0, 101.0, 102.01, 103.0301}
	if got := CalculateSharpeRatio(clean, 0.0); math.IsNaN(got) || got <= 0 {
		t.Errorf("expected positive finite Sharpe for clean series, got %v", got)
	}
}

func TestCalculateMaxDrawdown(t *testing.T) {
	equity := []float64{100.0, 110.0, 99.0, 120.0}
	// Peak is 110, trough is 99. Drop is 11. 11/110 = 0.10 (10%)
	mdd := CalculateMaxDrawdown(equity)

	if math.Abs(mdd-0.10) > 1e-6 {
		t.Errorf("Expected max drawdown to be 0.10, got %v", mdd)
	}

	if got := CalculateMaxDrawdown(nil); got != 0 {
		t.Errorf("empty equity should give 0 drawdown, got %f", got)
	}
}

func mkFill(side model.Side, qty, price float64, secs int64) model.Fill {
	return model.Fill{
		Instrument: "BTC-USD",
		Side:       side,
		Quantity:   qty,
		FillPrice:  price,
		Timestamp:  time.Unix(secs, 0),
	}
}

func TestHitRate(t *testing.T) {
	// Three round trips: 100→110 win, 110→105 loss, 105→115 win → 2/3.
	fills := []model.Fill{
		mkFill(model.Buy, 1, 100, 1),
		mkFill(model.Sell, 1, 110, 2),
		mkFill(model.Buy, 1, 110, 3),
		mkFill(model.Sell, 1, 105, 4),
		mkFill(model.Buy, 1, 105, 5),
		mkFill(model.Sell, 1, 115, 6),
	}
	if got := HitRate(fills); math.Abs(got-2.0/3.0) > 1e-9 {
		t.Errorf("expected HitRate=2/3, got %f", got)
	}
	if HitRate(nil) != 0 {
		t.Errorf("expected 0 HitRate on empty fills")
	}
}

func TestHitRate_FIFOPartialFills(t *testing.T) {
	// Two buys then one big sell: first buy (price 100) closed first,
	// then second buy (price 200) gets closed by remainder.
	// Sell at 150 → first round trip wins (+50), second loses (-50). 1/2.
	fills := []model.Fill{
		mkFill(model.Buy, 1, 100, 1),
		mkFill(model.Buy, 1, 200, 2),
		mkFill(model.Sell, 2, 150, 3),
	}
	if got := HitRate(fills); math.Abs(got-0.5) > 1e-9 {
		t.Errorf("expected HitRate=0.5 for FIFO partial fill, got %f", got)
	}
}

func TestTurnover(t *testing.T) {
	// Two trades of 100 notional each, max at-cost stays at 100 → turnover 2.
	fills := []model.Fill{
		mkFill(model.Buy, 1, 100, 1),
		mkFill(model.Sell, 1, 100, 2),
	}
	if got := Turnover(fills); math.Abs(got-2.0) > 1e-9 {
		t.Errorf("expected Turnover=2.0, got %f", got)
	}
	if Turnover(nil) != 0 {
		t.Errorf("expected 0 Turnover on empty fills")
	}
}

func TestAvgHoldTimeSeconds(t *testing.T) {
	// Two round trips, holds of 5s and 15s → mean 10s.
	fills := []model.Fill{
		mkFill(model.Buy, 1, 100, 0),
		mkFill(model.Sell, 1, 110, 5),
		mkFill(model.Buy, 1, 110, 10),
		mkFill(model.Sell, 1, 120, 25),
	}
	if got := AvgHoldTimeSeconds(fills); math.Abs(got-10.0) > 1e-9 {
		t.Errorf("expected AvgHold=10s, got %f", got)
	}
	if AvgHoldTimeSeconds(nil) != 0 {
		t.Errorf("expected 0 hold time on empty fills")
	}
}

func TestHitRateOrderingIndependence(t *testing.T) {
	// closedRoundTripStats sorts defensively; same set in scrambled order
	// must give the same answer.
	fills := []model.Fill{
		mkFill(model.Buy, 1, 100, 1),
		mkFill(model.Sell, 1, 110, 2),
		mkFill(model.Buy, 1, 110, 3),
		mkFill(model.Sell, 1, 105, 4),
	}
	scrambled := []model.Fill{fills[3], fills[0], fills[2], fills[1]}
	if HitRate(fills) != HitRate(scrambled) {
		t.Errorf("hit rate must be order-independent: %f vs %f",
			HitRate(fills), HitRate(scrambled))
	}
}
