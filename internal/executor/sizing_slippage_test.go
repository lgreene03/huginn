package executor

import (
	"math"
	"testing"
	"time"

	"github.com/lgreene03/huginn/internal/model"
	"github.com/lgreene03/huginn/internal/portfolio"
	"github.com/lgreene03/huginn/internal/strategy"
)

// TestEffectiveSlippageBps_DefaultIsFlat verifies that with no impact
// coefficient (the default), effective slippage equals the flat config value
// regardless of order quantity — i.e. the original behaviour is preserved.
func TestEffectiveSlippageBps_DefaultIsFlat(t *testing.T) {
	c := Config{SlippageBps: 5}
	for _, qty := range []float64{0.001, 1, 100, 1e6} {
		if got := c.effectiveSlippageBps(qty); got != 5 {
			t.Errorf("qty=%v: effective slip = %v, want flat 5", qty, got)
		}
	}
}

// TestEffectiveSlippageBps_SqrtImpact verifies the size-dependent term:
// base + k*sqrt(qty/scale), and that it grows monotonically with size.
func TestEffectiveSlippageBps_SqrtImpact(t *testing.T) {
	c := Config{SlippageBps: 5, SlippageImpactK: 2, SlippageImpactScale: 4}
	// qty=4 => sqrt(4/4)=1 => 5 + 2*1 = 7
	if got := c.effectiveSlippageBps(4); math.Abs(got-7) > 1e-9 {
		t.Errorf("qty=4: got %v, want 7", got)
	}
	// qty=16 => sqrt(16/4)=2 => 5 + 2*2 = 9
	if got := c.effectiveSlippageBps(16); math.Abs(got-9) > 1e-9 {
		t.Errorf("qty=16: got %v, want 9", got)
	}
	if c.effectiveSlippageBps(16) <= c.effectiveSlippageBps(4) {
		t.Error("expected larger order to incur more slippage")
	}
}

// TestEffectiveSlippageBps_NonPositiveScaleFallsBack ensures a misconfigured
// (non-positive) scale uses 1.0 rather than dividing by zero.
func TestEffectiveSlippageBps_NonPositiveScaleFallsBack(t *testing.T) {
	c := Config{SlippageBps: 1, SlippageImpactK: 3, SlippageImpactScale: 0}
	// qty=1 => sqrt(1/1)=1 => 1 + 3 = 4
	if got := c.effectiveSlippageBps(1); math.Abs(got-4) > 1e-9 {
		t.Errorf("got %v, want 4 (scale should fall back to 1.0)", got)
	}
}

// TestSimulateFill_RecordsEffectiveSlippage checks the fill carries the
// size-dependent slippage and the price reflects it.
func TestSimulateFill_RecordsEffectiveSlippage(t *testing.T) {
	p := portfolio.New(100_000)
	e := New(strategy.NewOBIThreshold(0.6, 0.01, 0.1), p, nil, nil, Config{
		SlippageBps: 10, SlippageImpactK: 10, SlippageImpactScale: 1,
	}, false, nil, "")

	order := model.Order{Instrument: "BTC-USDT", Side: model.Buy, Quantity: 4}
	ev := model.FeatureEvent{Values: map[string]float64{"microPrice": 100}}
	fill := e.simulateFill(order, ev)

	// qty=4 => 10 + 10*sqrt(4/1)=10+20 = 30 bps
	if math.Abs(fill.SlippageBps-30) > 1e-9 {
		t.Errorf("fill SlippageBps = %v, want 30", fill.SlippageBps)
	}
	// Buy fills at price*(1+30bps) = 100 * 1.003 = 100.3
	if math.Abs(fill.FillPrice-100.3) > 1e-9 {
		t.Errorf("fill price = %v, want 100.3", fill.FillPrice)
	}
}

// TestApplySizing_FixedIsNoop confirms SizingFixed leaves quantity untouched.
func TestApplySizing_FixedIsNoop(t *testing.T) {
	p := portfolio.New(100_000)
	e := New(strategy.NewOBIThreshold(0.6, 0.01, 0.1), p, nil, nil, Config{
		Sizing: strategy.SizingFixed,
	}, false, nil, "")

	orders := []model.Order{{Instrument: "X", Side: model.Buy, Quantity: 0.01, LimitPrice: 100}}
	e.applySizing(orders, model.FeatureEvent{Values: map[string]float64{}})
	if orders[0].Quantity != 0.01 {
		t.Errorf("fixed sizing changed qty to %v, want 0.01", orders[0].Quantity)
	}
}

// TestApplySizing_Kelly rescales by Kelly fraction of equity.
func TestApplySizing_Kelly(t *testing.T) {
	p := portfolio.New(100_000) // equity = 100k
	e := New(strategy.NewOBIThreshold(0.6, 0.01, 0.1), p, nil, nil, Config{
		Sizing:              strategy.SizingKelly,
		SizingKellyFraction: 0.1, // allocate 10% of equity = $10k
	}, false, nil, "")

	orders := []model.Order{{Instrument: "X", Side: model.Buy, Quantity: 0.01, LimitPrice: 100}}
	e.applySizing(orders, model.FeatureEvent{Values: map[string]float64{}})
	// target notional 10k / price 100 = 100 units
	if math.Abs(orders[0].Quantity-100) > 1e-9 {
		t.Errorf("kelly sizing qty = %v, want 100", orders[0].Quantity)
	}
}

// TestApplySizing_InverseVolFallsBackWithoutVol verifies inverse-vol degrades
// gracefully to the base quantity when the event has no volatility feature.
func TestApplySizing_InverseVolFallsBackWithoutVol(t *testing.T) {
	p := portfolio.New(100_000)
	e := New(strategy.NewOBIThreshold(0.6, 0.01, 0.1), p, nil, nil, Config{
		Sizing:          strategy.SizingInverseVol,
		SizingVolTarget: 0.01,
	}, false, nil, "")

	orders := []model.Order{{Instrument: "X", Side: model.Buy, Quantity: 0.01, LimitPrice: 100}}
	e.applySizing(orders, model.FeatureEvent{Values: map[string]float64{}}) // no volatility
	if orders[0].Quantity != 0.01 {
		t.Errorf("inverse-vol with no vol should keep base qty, got %v", orders[0].Quantity)
	}
}

// TestApplySizing_OnFeatureIntegration ensures OnFeature applies the sizing
// override end-to-end (the order that fills should carry the resized qty).
func TestApplySizing_OnFeatureIntegration(t *testing.T) {
	p := portfolio.New(100_000)
	s := strategy.NewOBIThreshold(0.6, 0.01, 1_000_000)
	e := New(s, p, nil, nil, Config{
		Sizing:              strategy.SizingKelly,
		SizingKellyFraction: 0.05,
	}, false, nil, "")

	// Strong negative OBI triggers a buy at microPrice 100.
	ev := model.FeatureEvent{
		Instrument: "BTC-USDT",
		EventTime:  time.Date(2026, 5, 10, 14, 0, 0, 0, time.UTC),
		Values: map[string]float64{
			"obi":        -0.9,
			"microPrice": 100,
			"midPrice":   100,
		},
	}
	e.OnFeature(ev)
	snap := p.Snapshot()
	pos := snap.Positions["BTC-USDT"]
	// 5% of 100k = 5000 notional / 100 = 50 units (>> the 0.01 base size).
	if math.Abs(pos.Quantity-50) > 1e-6 {
		t.Errorf("expected kelly-sized position ~50 units, got %v", pos.Quantity)
	}
}
