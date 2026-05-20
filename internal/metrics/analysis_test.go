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
