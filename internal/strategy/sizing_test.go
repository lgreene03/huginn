package strategy

import (
	"math"
	"testing"
	"time"
)

func TestParseSizingMode(t *testing.T) {
	cases := []struct {
		in       string
		wantMode SizingMode
		wantOK   bool
	}{
		{"", SizingFixed, true},
		{"fixed", SizingFixed, true},
		{"kelly", SizingKelly, true},
		{"inverse_vol", SizingInverseVol, true},
		{"inverse-vol", SizingInverseVol, true},
		{"nonsense", SizingFixed, false},
	}
	for _, c := range cases {
		m, ok := ParseSizingMode(c.in)
		if m != c.wantMode || ok != c.wantOK {
			t.Errorf("ParseSizingMode(%q) = (%v,%v), want (%v,%v)", c.in, m, ok, c.wantMode, c.wantOK)
		}
	}
}

func TestSizeOrder_FixedAndDegenerate(t *testing.T) {
	base := 0.02
	// Fixed mode always returns base.
	if got := SizeOrder(SizingFixed, SizingParams{BaseQty: base, Equity: 1e5, Price: 100}); got != base {
		t.Errorf("fixed = %v, want %v", got, base)
	}
	// Non-positive equity / price degrade to base.
	if got := SizeOrder(SizingKelly, SizingParams{BaseQty: base, Equity: 0, Price: 100, KellyFraction: 0.1}); got != base {
		t.Errorf("zero equity = %v, want %v", got, base)
	}
	if got := SizeOrder(SizingKelly, SizingParams{BaseQty: base, Equity: 1e5, Price: 0, KellyFraction: 0.1}); got != base {
		t.Errorf("zero price = %v, want %v", got, base)
	}
}

func TestSizeOrder_Kelly(t *testing.T) {
	// 10% of 100k = 10k notional / price 50 = 200 units.
	got := SizeOrder(SizingKelly, SizingParams{BaseQty: 1, Equity: 100_000, Price: 50, KellyFraction: 0.1})
	if math.Abs(got-200) > 1e-9 {
		t.Errorf("kelly = %v, want 200", got)
	}
	// Zero kelly fraction falls back to base.
	if got := SizeOrder(SizingKelly, SizingParams{BaseQty: 1, Equity: 100_000, Price: 50}); got != 1 {
		t.Errorf("zero kelly fraction = %v, want base 1", got)
	}
}

func TestSizeOrder_InverseVol(t *testing.T) {
	// VolTarget 0.01 / vol 0.02 = 0.5 * equity 100k = 50k notional / price 100 = 500.
	got := SizeOrder(SizingInverseVol, SizingParams{
		BaseQty: 1, Equity: 100_000, Price: 100, Volatility: 0.02, VolTarget: 0.01,
	})
	if math.Abs(got-500) > 1e-9 {
		t.Errorf("inverse-vol = %v, want 500", got)
	}
	// Higher vol => smaller size.
	hi := SizeOrder(SizingInverseVol, SizingParams{BaseQty: 1, Equity: 100_000, Price: 100, Volatility: 0.04, VolTarget: 0.01})
	if hi >= got {
		t.Errorf("higher vol should size smaller: hi=%v lo=%v", hi, got)
	}
	// Missing vol falls back to base.
	if got := SizeOrder(SizingInverseVol, SizingParams{BaseQty: 1, Equity: 100_000, Price: 100, VolTarget: 0.01}); got != 1 {
		t.Errorf("missing vol = %v, want base 1", got)
	}
}

func TestSizeOrder_MaxNotionalCap(t *testing.T) {
	// Kelly wants 50% of equity but cap is 25%.
	got := SizeOrder(SizingKelly, SizingParams{
		BaseQty: 1, Equity: 100_000, Price: 100, KellyFraction: 0.5, MaxNotionalFraction: 0.25,
	})
	// capped notional 25k / 100 = 250
	if math.Abs(got-250) > 1e-9 {
		t.Errorf("capped kelly = %v, want 250", got)
	}
}

func TestDefaultOBIParams_MatchHistorical(t *testing.T) {
	p := DefaultOBIParams()
	if p.StopLossPct != 0.005 || p.TakeProfitPct != 0.003 ||
		p.MaxHoldTime != 30*time.Minute || p.Cooldown != 60*time.Second || p.MaxNotional != 500.0 {
		t.Errorf("DefaultOBIParams drifted from historical values: %+v", p)
	}
}

func TestNewOBIThresholdWithParams_ZeroFieldsFallBack(t *testing.T) {
	// All-zero params should yield the historical defaults.
	s := NewOBIThresholdWithParams(0.7, 0.01, 0.1, OBIParams{})
	if s.stopLossPct != 0.005 || s.takeProfitPct != 0.003 ||
		s.maxHoldTime != 30*time.Minute || s.cooldown != 60*time.Second || s.maxNotional != 500.0 {
		t.Errorf("zero OBIParams did not fall back to defaults: stop=%v tp=%v hold=%v cd=%v notional=%v",
			s.stopLossPct, s.takeProfitPct, s.maxHoldTime, s.cooldown, s.maxNotional)
	}
}

func TestNewOBIThresholdWithParams_Overrides(t *testing.T) {
	s := NewOBIThresholdWithParams(0.7, 0.01, 0.1, OBIParams{
		StopLossPct:   0.02,
		TakeProfitPct: 0.04,
		MaxHoldTime:   5 * time.Minute,
		Cooldown:      10 * time.Second,
		MaxNotional:   1000,
	})
	if s.stopLossPct != 0.02 || s.takeProfitPct != 0.04 ||
		s.maxHoldTime != 5*time.Minute || s.cooldown != 10*time.Second || s.maxNotional != 1000 {
		t.Errorf("overrides not applied: %+v", s)
	}
}
