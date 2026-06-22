package strategy

import (
	"testing"

	"github.com/lgreene03/huginn/internal/model"
)

// ── ImbalanceAlpha ───────────────────────────────────────────────────────────

func TestImbalanceAlpha_PositiveObiIsLong(t *testing.T) {
	a := ImbalanceAlpha{}
	s := a.Compute(evt("BTC", map[string]float64{"obi": 0.6}))
	if s.Value <= 0 {
		t.Fatalf("positive obi should yield a long (>0) score, got %v", s.Value)
	}
	if s.Value != 0.6 {
		t.Fatalf("default scale should pass obi through, want 0.6 got %v", s.Value)
	}
	if s.Confidence != 1 {
		t.Fatalf("present obi should have full confidence, got %v", s.Confidence)
	}
}

func TestImbalanceAlpha_NegativeObiIsShort(t *testing.T) {
	a := ImbalanceAlpha{}
	s := a.Compute(evt("BTC", map[string]float64{"obi": -0.4}))
	if s.Value >= 0 {
		t.Fatalf("negative obi should yield a short (<0) score, got %v", s.Value)
	}
}

func TestImbalanceAlpha_ScaleClamps(t *testing.T) {
	a := ImbalanceAlpha{Scale: 10}
	s := a.Compute(evt("BTC", map[string]float64{"obi": 0.5}))
	if s.Value != 1 {
		t.Fatalf("0.5*10 should clamp to 1, got %v", s.Value)
	}
}

func TestImbalanceAlpha_AbsentObiZeroConfidence(t *testing.T) {
	a := ImbalanceAlpha{}
	s := a.Compute(evt("BTC", map[string]float64{"midPrice": 100}))
	if s.Confidence != 0 {
		t.Fatalf("absent obi should give 0 confidence, got %v", s.Confidence)
	}
}

// ── VolatilityRegimeAlpha ────────────────────────────────────────────────────

func TestVolRegime_CalmPassesInnerThrough(t *testing.T) {
	inner := constAlpha{name: "i", score: AlphaScore{Value: 0.8, Confidence: 1}}
	a := VolatilityRegimeAlpha{Inner: inner, Low: 0.01, High: 0.05}
	s := a.Compute(evt("BTC", map[string]float64{"volatility": 0.005}))
	if s.Value != 0.8 {
		t.Fatalf("calm regime should pass inner value through, want 0.8 got %v", s.Value)
	}
	if s.Confidence != 1 {
		t.Fatalf("calm regime should pass inner confidence through, want 1 got %v", s.Confidence)
	}
}

func TestVolRegime_WildZerosConviction(t *testing.T) {
	inner := constAlpha{name: "i", score: AlphaScore{Value: 0.8, Confidence: 1}}
	a := VolatilityRegimeAlpha{Inner: inner, Low: 0.01, High: 0.05}
	s := a.Compute(evt("BTC", map[string]float64{"volatility": 0.10}))
	if s.Value != 0 {
		t.Fatalf("wild regime should zero the value, got %v", s.Value)
	}
	if s.Confidence != 0 {
		t.Fatalf("wild regime should zero the confidence, got %v", s.Confidence)
	}
}

func TestVolRegime_MidBandScalesDown(t *testing.T) {
	inner := constAlpha{name: "i", score: AlphaScore{Value: 0.8, Confidence: 1}}
	a := VolatilityRegimeAlpha{Inner: inner, Low: 0.0, High: 0.10}
	// Halfway through the band ⇒ multiplier 0.5 ⇒ value 0.4.
	s := a.Compute(evt("BTC", map[string]float64{"volatility": 0.05}))
	if diff := s.Value - 0.4; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("mid-band should halve the value, want 0.4 got %v", s.Value)
	}
	if diff := s.Confidence - 0.5; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("mid-band should halve the confidence, want 0.5 got %v", s.Confidence)
	}
}

func TestVolRegime_AbsentVolatilityPassesThrough(t *testing.T) {
	inner := constAlpha{name: "i", score: AlphaScore{Value: 0.8, Confidence: 1}}
	a := VolatilityRegimeAlpha{Inner: inner, Low: 0.01, High: 0.05}
	s := a.Compute(evt("BTC", map[string]float64{"midPrice": 100}))
	if s.Value != 0.8 {
		t.Fatalf("absent volatility should pass inner through, got %v", s.Value)
	}
}

func TestVolRegime_NilInnerInert(t *testing.T) {
	a := VolatilityRegimeAlpha{}
	s := a.Compute(evt("BTC", map[string]float64{"volatility": 0.01}))
	if s.Confidence != 0 || s.Value != 0 {
		t.Fatalf("nil inner should be inert, got %+v", s)
	}
}

// ── MeanReversionAlpha ───────────────────────────────────────────────────────

func TestMeanReversion_ColdStartZeroConfidence(t *testing.T) {
	a := &MeanReversionAlpha{WarmUp: 5}
	s := a.Compute(evt("BTC", map[string]float64{"midPrice": 101, "emaSlow": 100}))
	if s.Confidence != 0 {
		t.Fatalf("cold-start mean-reversion should have 0 confidence, got %v", s.Confidence)
	}
}

func TestMeanReversion_StretchAboveMeanIsShort(t *testing.T) {
	a := &MeanReversionAlpha{WarmUp: 5, Sensitivity: 0.5}
	// Feed a stable history where midPrice tracks emaSlow closely (small dev),
	// then a large positive deviation. The final z-score is strongly positive,
	// so the mean-reversion score must be negative (short).
	hist := []float64{100.0, 100.1, 99.9, 100.05, 99.95, 100.02}
	var last AlphaScore
	for _, mid := range hist {
		last = a.Compute(evt("BTC", map[string]float64{"midPrice": mid, "emaSlow": 100}))
	}
	// Now a big jump up.
	last = a.Compute(evt("BTC", map[string]float64{"midPrice": 110, "emaSlow": 100}))
	if last.Value >= 0 {
		t.Fatalf("midPrice stretched far above slow EMA should yield a short (negative) score, got %v", last.Value)
	}
	if last.Confidence <= 0 {
		t.Fatalf("warmed alpha should report non-zero confidence, got %v", last.Confidence)
	}
}

func TestMeanReversion_StretchBelowMeanIsLong(t *testing.T) {
	a := &MeanReversionAlpha{WarmUp: 5, Sensitivity: 0.5}
	hist := []float64{100.0, 100.1, 99.9, 100.05, 99.95, 100.02}
	for _, mid := range hist {
		_ = a.Compute(evt("BTC", map[string]float64{"midPrice": mid, "emaSlow": 100}))
	}
	last := a.Compute(evt("BTC", map[string]float64{"midPrice": 90, "emaSlow": 100}))
	if last.Value <= 0 {
		t.Fatalf("midPrice stretched far below slow EMA should yield a long (positive) score, got %v", last.Value)
	}
}

func TestMeanReversion_AbsentFieldsZeroConfidence(t *testing.T) {
	a := &MeanReversionAlpha{WarmUp: 5}
	s := a.Compute(evt("BTC", map[string]float64{"obi": 0.5}))
	if s.Confidence != 0 {
		t.Fatalf("missing midPrice/emaSlow should give 0 confidence, got %v", s.Confidence)
	}
}

// ── FundingRateAlpha (new-data example) ──────────────────────────────────────

func TestFundingRate_PositiveFundingIsContrarianShort(t *testing.T) {
	a := FundingRateAlpha{}
	// Positive funding (crowded longs) ⇒ contrarian short ⇒ negative score.
	s := a.Compute(evt("BTC", map[string]float64{"funding": 0.005}))
	if s.Value >= 0 {
		t.Fatalf("positive funding should yield a short (negative) score, got %v", s.Value)
	}
	if s.Confidence != 1 {
		t.Fatalf("present funding should have full confidence, got %v", s.Confidence)
	}
}

func TestFundingRate_NegativeFundingIsLong(t *testing.T) {
	a := FundingRateAlpha{}
	s := a.Compute(evt("BTC", map[string]float64{"funding": -0.005}))
	if s.Value <= 0 {
		t.Fatalf("negative funding should yield a long (positive) score, got %v", s.Value)
	}
}

func TestFundingRate_ScaleSaturates(t *testing.T) {
	a := FundingRateAlpha{Scale: 100}
	// 0.02 * 100 = 2 ⇒ clamps to magnitude 1 (negated for contrarian sign).
	s := a.Compute(evt("BTC", map[string]float64{"funding": 0.02}))
	if s.Value != -1 {
		t.Fatalf("large funding should saturate to -1, got %v", s.Value)
	}
}

func TestFundingRate_AbsentFundingZeroConfidence(t *testing.T) {
	a := FundingRateAlpha{}
	s := a.Compute(evt("BTC", map[string]float64{"obi": 0.5}))
	if s.Confidence != 0 {
		t.Fatalf("absent funding should give 0 confidence, got %v", s.Confidence)
	}
}

// ── Default registry ─────────────────────────────────────────────────────────

func TestDefaultAlphaRegistry_HasAllBundledAlphas(t *testing.T) {
	reg := DefaultAlphaRegistry()
	want := []string{"ema_reversion", "funding_rate", "imbalance", "mean_reversion", "momentum", "vol_regime"}
	got := reg.Names()
	if len(got) != len(want) {
		t.Fatalf("registry Names = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("registry Names = %v, want %v", got, want)
		}
	}
}

func TestDefaultAlphaRegistry_BuildsUsableComposite(t *testing.T) {
	reg := DefaultAlphaRegistry()
	c, err := NewCompositeFromRegistry(reg, map[string]float64{
		"imbalance":    0.6,
		"funding_rate": 0.4,
	}, CompositeConfig{
		EntryThreshold: 0.3, OrderSize: 0.01, MaxPosition: 1, UseConfidence: true,
	})
	if err != nil {
		t.Fatalf("NewCompositeFromRegistry: %v", err)
	}
	if len(c.alphas) != 2 {
		t.Fatalf("want 2 alphas wired, got %d", len(c.alphas))
	}
	// A strong positive obi with calm/neutral funding should clear the threshold
	// and BUY — proving the registry-built composite actually trades.
	orders := c.OnFeature(evt("BTC", map[string]float64{"obi": 0.9, "funding": 0.0}))
	if len(orders) != 1 {
		t.Fatalf("expected the registry composite to trade on strong obi, got %d orders", len(orders))
	}
	if orders[0].Side != model.Buy {
		t.Fatalf("strong positive obi should BUY, got %v", orders[0].Side)
	}
}

// ── Interface conformance ────────────────────────────────────────────────────

func TestBundledAlphas_ImplementInterface(t *testing.T) {
	var _ Alpha = ImbalanceAlpha{}
	var _ Alpha = VolatilityRegimeAlpha{}
	var _ Alpha = (*MeanReversionAlpha)(nil)
	var _ Alpha = FundingRateAlpha{}
}
