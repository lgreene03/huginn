package strategy

import (
	"errors"
	"testing"
	"time"

	"github.com/lgreene03/huginn/internal/model"
)

// constAlpha is a fixed-score test alpha.
type constAlpha struct {
	name  string
	score AlphaScore
}

func (a constAlpha) Name() string                          { return a.name }
func (a constAlpha) Compute(model.FeatureEvent) AlphaScore { return a.score }

func evt(instrument string, values map[string]float64) model.FeatureEvent {
	return model.FeatureEvent{
		Instrument: instrument,
		EventTime:  time.Unix(1_700_000_000, 0).UTC(),
		Values:     values,
	}
}

// ── Registry ────────────────────────────────────────────────────────────────

func TestAlphaRegistry_RegisterAndGet(t *testing.T) {
	reg := NewAlphaRegistry()
	a := constAlpha{name: "alpha_a", score: AlphaScore{Value: 0.5, Confidence: 1}}
	if err := reg.Register(a); err != nil {
		t.Fatalf("Register: %v", err)
	}
	got, err := reg.Get("alpha_a")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Name() != "alpha_a" {
		t.Fatalf("got name %q, want alpha_a", got.Name())
	}
	if reg.Len() != 1 {
		t.Fatalf("Len = %d, want 1", reg.Len())
	}
}

func TestAlphaRegistry_MissingLookup(t *testing.T) {
	reg := NewAlphaRegistry()
	if _, err := reg.Get("nope"); err == nil {
		t.Fatal("expected error for missing alpha, got nil")
	}
}

func TestAlphaRegistry_DuplicateRejected(t *testing.T) {
	reg := NewAlphaRegistry()
	a := constAlpha{name: "dup", score: AlphaScore{Value: 1, Confidence: 1}}
	if err := reg.Register(a); err != nil {
		t.Fatalf("first Register: %v", err)
	}
	if err := reg.Register(a); err == nil {
		t.Fatal("expected duplicate Register to error, got nil")
	}
}

func TestAlphaRegistry_NilAndEmptyRejected(t *testing.T) {
	reg := NewAlphaRegistry()
	if err := reg.Register(nil); err == nil {
		t.Fatal("expected error registering nil alpha")
	}
	if err := reg.Register(constAlpha{name: ""}); err == nil {
		t.Fatal("expected error registering empty-name alpha")
	}
}

func TestAlphaRegistry_NamesSorted(t *testing.T) {
	reg := NewAlphaRegistry()
	_ = reg.Register(constAlpha{name: "zeta"})
	_ = reg.Register(constAlpha{name: "alpha"})
	_ = reg.Register(constAlpha{name: "mid"})
	names := reg.Names()
	want := []string{"alpha", "mid", "zeta"}
	if len(names) != len(want) {
		t.Fatalf("got %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("got %v, want %v", names, want)
		}
	}
}

// ── Composite combination ────────────────────────────────────────────────────

func TestComposite_TwoAlphasCombineAsExpected(t *testing.T) {
	// Two alphas, equal weight, both Value=+0.8 → weighted sum normalized by
	// total weight = (0.5*0.8 + 0.5*0.8) / 1.0 = 0.8.
	c := NewCompositeStrategy(CompositeConfig{
		Name: "T",
		Alphas: []WeightedAlpha{
			{Alpha: constAlpha{name: "a", score: AlphaScore{Value: 0.8, Confidence: 1}}, Weight: 0.5},
			{Alpha: constAlpha{name: "b", score: AlphaScore{Value: 0.8, Confidence: 1}}, Weight: 0.5},
		},
		EntryThreshold: 0.5,
		OrderSize:      0.01,
		MaxPosition:    1,
	})
	c.mu.Lock()
	got := c.score(evt("BTC", map[string]float64{"midPrice": 100}))
	c.mu.Unlock()
	if diff := got - 0.8; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("combined score = %v, want 0.8", got)
	}
}

func TestComposite_OpposingAlphasCancel(t *testing.T) {
	c := NewCompositeStrategy(CompositeConfig{
		Name: "T",
		Alphas: []WeightedAlpha{
			{Alpha: constAlpha{name: "a", score: AlphaScore{Value: 0.8, Confidence: 1}}, Weight: 0.5},
			{Alpha: constAlpha{name: "b", score: AlphaScore{Value: -0.8, Confidence: 1}}, Weight: 0.5},
		},
		EntryThreshold: 0.5,
		OrderSize:      0.01,
		MaxPosition:    1,
	})
	orders := c.OnFeature(evt("BTC", map[string]float64{"midPrice": 100}))
	if len(orders) != 0 {
		t.Fatalf("opposing alphas should cancel below threshold, got %d orders", len(orders))
	}
}

// ── Threshold → side ─────────────────────────────────────────────────────────

func TestComposite_PositiveScoreBuys(t *testing.T) {
	c := NewCompositeStrategy(CompositeConfig{
		Alphas: []WeightedAlpha{
			{Alpha: constAlpha{name: "a", score: AlphaScore{Value: 0.9, Confidence: 1}}, Weight: 1},
		},
		EntryThreshold: 0.5,
		OrderSize:      0.01,
		MaxPosition:    1,
	})
	orders := c.OnFeature(evt("BTC", map[string]float64{"midPrice": 100}))
	if len(orders) != 1 {
		t.Fatalf("want 1 order, got %d", len(orders))
	}
	if orders[0].Side != model.Buy {
		t.Fatalf("positive score should BUY, got %v", orders[0].Side)
	}
}

func TestComposite_NegativeScoreSells(t *testing.T) {
	c := NewCompositeStrategy(CompositeConfig{
		Alphas: []WeightedAlpha{
			{Alpha: constAlpha{name: "a", score: AlphaScore{Value: -0.9, Confidence: 1}}, Weight: 1},
		},
		EntryThreshold: 0.5,
		OrderSize:      0.01,
		MaxPosition:    1,
	})
	orders := c.OnFeature(evt("BTC", map[string]float64{"midPrice": 100}))
	if len(orders) != 1 {
		t.Fatalf("want 1 order, got %d", len(orders))
	}
	if orders[0].Side != model.Sell {
		t.Fatalf("negative score should SELL, got %v", orders[0].Side)
	}
}

func TestComposite_BelowThresholdNoTrade(t *testing.T) {
	c := NewCompositeStrategy(CompositeConfig{
		Alphas: []WeightedAlpha{
			{Alpha: constAlpha{name: "a", score: AlphaScore{Value: 0.3, Confidence: 1}}, Weight: 1},
		},
		EntryThreshold: 0.5,
		OrderSize:      0.01,
		MaxPosition:    1,
	})
	if orders := c.OnFeature(evt("BTC", map[string]float64{"midPrice": 100})); len(orders) != 0 {
		t.Fatalf("score below threshold should not trade, got %d orders", len(orders))
	}
}

// ── Zero confidence → no trade (confidence-weighted) ────────────────────────

func TestComposite_AllZeroConfidenceNoTrade(t *testing.T) {
	c := NewCompositeStrategy(CompositeConfig{
		Alphas: []WeightedAlpha{
			{Alpha: constAlpha{name: "a", score: AlphaScore{Value: 0.9, Confidence: 0}}, Weight: 0.5},
			{Alpha: constAlpha{name: "b", score: AlphaScore{Value: 0.9, Confidence: 0}}, Weight: 0.5},
		},
		UseConfidence:  true,
		EntryThreshold: 0.5,
		OrderSize:      0.01,
		MaxPosition:    1,
	})
	if orders := c.OnFeature(evt("BTC", map[string]float64{"midPrice": 100})); len(orders) != 0 {
		t.Fatalf("zero-confidence set should produce no trade, got %d orders", len(orders))
	}
}

// ── Per-instrument throttle ──────────────────────────────────────────────────

func TestComposite_SinglePositionThrottle(t *testing.T) {
	c := NewCompositeStrategy(CompositeConfig{
		Alphas: []WeightedAlpha{
			{Alpha: constAlpha{name: "a", score: AlphaScore{Value: 0.9, Confidence: 1}}, Weight: 1},
		},
		EntryThreshold: 0.5,
		OrderSize:      0.01,
		MaxPosition:    1,
	})
	if orders := c.OnFeature(evt("BTC", map[string]float64{"midPrice": 100})); len(orders) != 1 {
		t.Fatalf("first entry: want 1 order, got %d", len(orders))
	}
	if orders := c.OnFeature(evt("BTC", map[string]float64{"midPrice": 100})); len(orders) != 0 {
		t.Fatalf("second entry should be throttled (already in position), got %d orders", len(orders))
	}
}

// ── Cost-hurdle reuse ────────────────────────────────────────────────────────

func TestComposite_ReusesCostHurdleGate(t *testing.T) {
	c := NewCompositeStrategy(CompositeConfig{
		Alphas: []WeightedAlpha{
			{Alpha: constAlpha{name: "a", score: AlphaScore{Value: 0.9, Confidence: 1}}, Weight: 1},
		},
		EntryThreshold: 0.5,
		OrderSize:      0.01,
		MaxPosition:    1,
	})
	// Active hurdle with the conservative DefaultEdgeModel (always 0 edge) and
	// K>0 ⇒ every entry is suppressed. This is the SAME gate OBIThreshold uses.
	c.SetCostHurdle(&CostHurdle{
		K:                  1,
		TransactionCostBps: 5,
		SlippageBps:        2,
		Edge:               DefaultEdgeModel{},
	})
	if orders := c.OnFeature(evt("BTC", map[string]float64{"midPrice": 100})); len(orders) != 0 {
		t.Fatalf("active cost hurdle should suppress entry, got %d orders", len(orders))
	}

	// Clearing the hurdle (nil = inert) restores trading.
	c.SetCostHurdle(nil)
	if orders := c.OnFeature(evt("BTC", map[string]float64{"midPrice": 100})); len(orders) != 1 {
		t.Fatalf("inert hurdle should allow entry, got %d orders", len(orders))
	}
}

func TestComposite_NilHurdleIsInert(t *testing.T) {
	c := NewCompositeStrategy(CompositeConfig{
		Alphas: []WeightedAlpha{
			{Alpha: constAlpha{name: "a", score: AlphaScore{Value: 0.9, Confidence: 1}}, Weight: 1},
		},
		EntryThreshold: 0.5,
		OrderSize:      0.01,
		MaxPosition:    1,
	})
	// costHurdle is nil by default — must not suppress.
	if orders := c.OnFeature(evt("BTC", map[string]float64{"midPrice": 100})); len(orders) != 1 {
		t.Fatalf("nil hurdle (default) should be inert, got %d orders", len(orders))
	}
}

// ── Signed maxPosition cap ───────────────────────────────────────────────────

func TestComposite_SignedPositionCap(t *testing.T) {
	c := NewCompositeStrategy(CompositeConfig{
		Alphas: []WeightedAlpha{
			{Alpha: constAlpha{name: "a", score: AlphaScore{Value: 0.9, Confidence: 1}}, Weight: 1},
		},
		EntryThreshold: 0.5,
		OrderSize:      1,
		MaxPosition:    1, // one BUY of size 1 reaches the long cap
	})
	if orders := c.OnFeature(evt("BTC", map[string]float64{"midPrice": 100})); len(orders) != 1 {
		t.Fatalf("first buy should fill, got %d", len(orders))
	}
	// netPosition is now +1 = maxPosition. A different instrument long entry
	// must be rejected by the signed cap.
	if orders := c.OnFeature(evt("ETH", map[string]float64{"midPrice": 50})); len(orders) != 0 {
		t.Fatalf("buy at long cap should be rejected, got %d orders", len(orders))
	}
}

// ── Registry-driven construction ─────────────────────────────────────────────

func TestNewCompositeFromRegistry(t *testing.T) {
	reg := NewAlphaRegistry()
	_ = reg.Register(constAlpha{name: "a", score: AlphaScore{Value: 0.9, Confidence: 1}})
	_ = reg.Register(constAlpha{name: "b", score: AlphaScore{Value: 0.9, Confidence: 1}})

	c, err := NewCompositeFromRegistry(reg, map[string]float64{"a": 0.5, "b": 0.5}, CompositeConfig{
		EntryThreshold: 0.5, OrderSize: 0.01, MaxPosition: 1,
	})
	if err != nil {
		t.Fatalf("NewCompositeFromRegistry: %v", err)
	}
	if len(c.alphas) != 2 {
		t.Fatalf("want 2 alphas, got %d", len(c.alphas))
	}
}

func TestNewCompositeFromRegistry_MissingAlpha(t *testing.T) {
	reg := NewAlphaRegistry()
	_ = reg.Register(constAlpha{name: "a"})
	_, err := NewCompositeFromRegistry(reg, map[string]float64{"a": 1, "ghost": 1}, CompositeConfig{})
	if err == nil {
		t.Fatal("expected error for missing alpha, got nil")
	}
	var nf alphaNotFoundError
	if !errors.As(err, &nf) {
		t.Fatalf("want alphaNotFoundError, got %T: %v", err, err)
	}
}

// ── Empty composite never trades ─────────────────────────────────────────────

func TestComposite_EmptyNeverTrades(t *testing.T) {
	c := NewCompositeStrategy(CompositeConfig{OrderSize: 0.01, MaxPosition: 1})
	if orders := c.OnFeature(evt("BTC", map[string]float64{"midPrice": 100})); len(orders) != 0 {
		t.Fatalf("empty composite should never trade, got %d orders", len(orders))
	}
}

// ── State round-trip (Stateful) ──────────────────────────────────────────────

func TestComposite_StateRoundTrip(t *testing.T) {
	c := NewCompositeStrategy(CompositeConfig{
		Alphas: []WeightedAlpha{
			{Alpha: constAlpha{name: "a", score: AlphaScore{Value: 0.9, Confidence: 1}}, Weight: 1},
		},
		EntryThreshold: 0.5, OrderSize: 0.01, MaxPosition: 1,
	})
	if orders := c.OnFeature(evt("BTC", map[string]float64{"midPrice": 100})); len(orders) != 1 {
		t.Fatalf("want a position open, got %d orders", len(orders))
	}
	blob, err := c.MarshalState()
	if err != nil {
		t.Fatalf("MarshalState: %v", err)
	}

	restored := NewCompositeStrategy(CompositeConfig{
		Alphas: []WeightedAlpha{
			{Alpha: constAlpha{name: "a", score: AlphaScore{Value: 0.9, Confidence: 1}}, Weight: 1},
		},
		EntryThreshold: 0.5, OrderSize: 0.01, MaxPosition: 1,
	})
	if err := restored.RestoreState(blob); err != nil {
		t.Fatalf("RestoreState: %v", err)
	}
	restored.mu.Lock()
	_, hasPos := restored.positions["BTC"]
	net := restored.netPosition
	restored.mu.Unlock()
	if !hasPos {
		t.Fatal("restored composite should have the BTC position")
	}
	if net != c.netPosition {
		t.Fatalf("restored netPosition = %v, want %v", net, c.netPosition)
	}
}

// ── Implements the Strategy + Stateful interfaces ────────────────────────────

func TestComposite_ImplementsInterfaces(t *testing.T) {
	var _ Strategy = (*CompositeStrategy)(nil)
	var _ Stateful = (*CompositeStrategy)(nil)
}

// ── Bundled alphas: stateless field/momentum + stateful EMA warm-up ──────────

func TestFieldAlpha_AbsentFieldZeroConfidence(t *testing.T) {
	a := FieldAlpha{Field: "obi", Scale: 1, Conf: 1}
	s := a.Compute(evt("BTC", map[string]float64{"midPrice": 100}))
	if s.Confidence != 0 {
		t.Fatalf("absent field should give 0 confidence, got %v", s.Confidence)
	}
}

func TestFieldAlpha_InvertFlipsSign(t *testing.T) {
	a := FieldAlpha{Field: "obi", Scale: 1, Conf: 1, Invert: true}
	s := a.Compute(evt("BTC", map[string]float64{"obi": 0.5}))
	if s.Value != -0.5 {
		t.Fatalf("invert should flip sign, got %v", s.Value)
	}
}

func TestEMAReversionAlpha_ColdStartZeroConfidence(t *testing.T) {
	a := &EMAReversionAlpha{}
	first := a.Compute(evt("BTC", map[string]float64{"emaFast": 110, "emaSlow": 100}))
	if first.Confidence != 0 {
		t.Fatalf("cold-start EMA alpha should have 0 confidence, got %v", first.Confidence)
	}
	second := a.Compute(evt("BTC", map[string]float64{"emaFast": 110, "emaSlow": 100}))
	if second.Confidence == 0 {
		t.Fatalf("warmed EMA alpha should have non-zero confidence")
	}
	// fast above slow ⇒ stretched up ⇒ mean-reversion short ⇒ negative value.
	if second.Value >= 0 {
		t.Fatalf("fast>slow should yield a short (negative) score, got %v", second.Value)
	}
}
