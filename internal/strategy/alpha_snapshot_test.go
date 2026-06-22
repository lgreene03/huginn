package strategy

import (
	"math"
	"testing"
)

// TestAlphaSnapshot_BeforeFirstEvent asserts a freshly-constructed composite
// reports the configured per-alpha weights but NULL contribution/confidence and
// an EMPTY IC history — never fabricated numbers — before any OnFeature.
func TestAlphaSnapshot_BeforeFirstEvent(t *testing.T) {
	c := NewCompositeStrategy(CompositeConfig{
		Name: "Composite",
		Alphas: []WeightedAlpha{
			{Alpha: constAlpha{name: "a", score: AlphaScore{Value: 0.4, Confidence: 1}}, Weight: 0.6},
			{Alpha: constAlpha{name: "b", score: AlphaScore{Value: -0.2, Confidence: 0.5}}, Weight: 0.4},
		},
		BlendMode:      BlendWeightedSum,
		EntryThreshold: 0.3,
		OrderSize:      1,
	})

	snap := c.AlphaSnapshot()
	if snap.Blend != "weighted_sum" {
		t.Errorf("blend = %q, want weighted_sum", snap.Blend)
	}
	if snap.EntryThreshold != 0.3 {
		t.Errorf("entryThreshold = %v, want 0.3", snap.EntryThreshold)
	}
	if snap.CompositeScore != 0 {
		t.Errorf("compositeScore = %v, want 0 before first event", snap.CompositeScore)
	}
	if len(snap.Alphas) != 2 {
		t.Fatalf("len(alphas) = %d, want 2", len(snap.Alphas))
	}
	for _, a := range snap.Alphas {
		if a.Contribution != nil {
			t.Errorf("%s contribution = %v, want null before first event", a.Name, *a.Contribution)
		}
		if a.Confidence != nil {
			t.Errorf("%s confidence = %v, want null before first event", a.Name, *a.Confidence)
		}
		if len(a.IC) != 0 {
			t.Errorf("%s ic = %v, want empty before first event", a.Name, a.IC)
		}
	}
	if got, want := snap.Alphas[0].Weight, 0.6; got != want {
		t.Errorf("alpha[0] weight = %v, want %v", got, want)
	}
}

// TestAlphaSnapshot_RecordsLastContribution asserts that after an OnFeature the
// snapshot reports REAL last contribution + confidence per alpha (no longer
// null) and a composite score consistent with the blend.
func TestAlphaSnapshot_RecordsLastContribution(t *testing.T) {
	c := NewCompositeStrategy(CompositeConfig{
		Alphas: []WeightedAlpha{
			{Alpha: constAlpha{name: "a", score: AlphaScore{Value: 0.5, Confidence: 0.9}}, Weight: 0.6},
			{Alpha: constAlpha{name: "b", score: AlphaScore{Value: -0.5, Confidence: 0.4}}, Weight: 0.4},
		},
		BlendMode:      BlendWeightedSum,
		UseConfidence:  true,
		EntryThreshold: 0.5,
		OrderSize:      1,
	})

	c.OnFeature(evt("BTC", map[string]float64{"midPrice": 100}))

	snap := c.AlphaSnapshot()
	if snap.Alphas[0].Contribution == nil || snap.Alphas[1].Contribution == nil {
		t.Fatalf("contributions must be non-null after OnFeature: %+v", snap.Alphas)
	}
	// Confidence-weighted contribution: weight*value*conf.
	wantA := 0.6 * 0.5 * 0.9
	if got := *snap.Alphas[0].Contribution; math.Abs(got-wantA) > 1e-9 {
		t.Errorf("alpha a contribution = %v, want %v", got, wantA)
	}
	if snap.Alphas[0].Confidence == nil || *snap.Alphas[0].Confidence != 0.9 {
		t.Errorf("alpha a confidence = %v, want 0.9", snap.Alphas[0].Confidence)
	}
	// Composite score = Σcontrib / Σ|weight|.
	wantCombined := (wantA + 0.4*(-0.5)*0.4) / (0.6 + 0.4)
	if math.Abs(snap.CompositeScore-wantCombined) > 1e-9 {
		t.Errorf("compositeScore = %v, want %v", snap.CompositeScore, wantCombined)
	}
}

// TestAlphaSnapshot_ICEmptyUntilEnoughSamples asserts IC stays empty until at
// least icMinSamples paired (contribution, forward-return) points exist, and
// becomes populated once enough events with varying mid prices arrive.
func TestAlphaSnapshot_ICEmptyUntilEnoughSamples(t *testing.T) {
	c := NewCompositeStrategy(CompositeConfig{
		Alphas: []WeightedAlpha{
			{Alpha: constAlpha{name: "a", score: AlphaScore{Value: 0.2, Confidence: 1}}, Weight: 1},
		},
		BlendMode:      BlendWeightedSum,
		EntryThreshold: 0.9, // high so no trades fire; we only exercise observability
		OrderSize:      1,
	})

	// Feed icMinSamples-1 events: the first stages a pending sample, each later
	// one realizes a forward return. After k events there are k-1 paired points.
	for i := 0; i < icMinSamples; i++ {
		mid := 100.0 + float64(i) // strictly varying so forward returns are non-zero
		c.OnFeature(evt("BTC", map[string]float64{"midPrice": mid}))
	}
	// After icMinSamples events there are icMinSamples-1 paired points (< min).
	if ic := c.AlphaSnapshot().Alphas[0].IC; len(ic) != 0 {
		t.Errorf("ic = %v, want empty with < icMinSamples paired points", ic)
	}

	// Two more events push paired points to icMinSamples+1, crossing the bar.
	c.OnFeature(evt("BTC", map[string]float64{"midPrice": 200}))
	c.OnFeature(evt("BTC", map[string]float64{"midPrice": 150}))
	if ic := c.AlphaSnapshot().Alphas[0].IC; len(ic) == 0 {
		t.Errorf("ic = empty, want populated once >= icMinSamples paired points exist")
	}
}

// TestAlphaSnapshot_ZScoreBlendLabel asserts the blend label reflects the mode.
func TestAlphaSnapshot_ZScoreBlendLabel(t *testing.T) {
	c := NewCompositeStrategy(CompositeConfig{
		Alphas:    []WeightedAlpha{{Alpha: constAlpha{name: "a"}, Weight: 1}},
		BlendMode: BlendZScore,
		OrderSize: 1,
	})
	if got := c.AlphaSnapshot().Blend; got != "zscore" {
		t.Errorf("blend = %q, want zscore", got)
	}
}
