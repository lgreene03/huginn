package strategy

import (
	"fmt"
	"math"
	"sort"
	"sync"

	"github.com/lgreene03/huginn/internal/model"
)

// ── Pluggable Alpha framework (quant-alpha-3) ───────────────────────────────
//
// This file is the data/signal-extensibility headline of the Norse stack: a
// quant team adds a new alpha by writing one small type that implements Alpha
// and registering it, then composing it into a CompositeStrategy with a weight.
// No existing strategy is touched, and the composite reuses the SAME signed
// position semantics + net-of-cost CostHurdle gate + risk path as OBIThreshold.
//
// # The contract
//
// An Alpha reads a model.FeatureEvent (which already carries the rich
// multi-asset feature Values — obi, momentum*, volatility, funding,
// openInterest, fearGreed, mlScore, newsSentiment, …) and emits a normalized
// AlphaScore. The composite blends a configured, weighted set of these into one
// combined score, applies an entry threshold to choose side+size, and routes
// the resulting entry through the existing cost hurdle and the existing
// signed-position throttle exactly like OBIThreshold.OnFeature does.
//
// # Concurrency
//
// Alphas may hold internal rolling state. The dispatch contract (see
// strategy.go) is single-goroutine OnFeature, but state may also be read from
// the persistence/HTTP goroutines. Alphas that carry mutable state MUST guard
// it with their own mutex; stateless alphas need no locking. The bundled
// EMAReversionAlpha is the canonical guarded-state example; MomentumAlpha and
// the *FieldAlpha helpers are stateless.

// AlphaScore is the normalized output of an Alpha.
//
//   - Value is the signal in [-1, 1]: the SIGN is the desired direction
//     (>0 ⇒ long/buy, <0 ⇒ short/sell) and the MAGNITUDE is conviction. Values
//     outside [-1, 1] are clamped by the composite before blending so a
//     misbehaving alpha cannot dominate.
//   - Confidence is in [0, 1]: how much to trust this score right now (e.g.
//     0 while an alpha is still warming up its rolling window). The composite
//     can optionally weight each contribution by Confidence.
type AlphaScore struct {
	Value      float64
	Confidence float64
}

// clampUnit clamps x to [-1, 1].
func clampUnit(x float64) float64 {
	if x > 1 {
		return 1
	}
	if x < -1 {
		return -1
	}
	return x
}

// clamp01 clamps x to [0, 1].
func clamp01(x float64) float64 {
	if x > 1 {
		return 1
	}
	if x < 0 {
		return 0
	}
	return x
}

// Alpha is a single, composable signal source. Implementations are the unit of
// data/signal extensibility: add a data field to the feature event, write an
// Alpha that reads it, register it, give it a weight.
type Alpha interface {
	// Name is a stable identifier used for registry lookup and the per-alpha
	// Prometheus contribution metric. Must be unique within a registry.
	Name() string

	// Compute maps a feature event to a normalized AlphaScore. Implementations
	// that hold rolling state must be safe under the strategy concurrency
	// contract (guard mutable state with a mutex). Compute MUST be a pure
	// read of the event plus the alpha's own state — it must not emit orders or
	// touch portfolio/risk state.
	Compute(event model.FeatureEvent) AlphaScore
}

// AlphaRegistry is a name→Alpha lookup. It lets the composite (and, later, a
// config-driven boot path) assemble a strategy from named alphas. Safe for
// concurrent registration and lookup.
type AlphaRegistry struct {
	mu     sync.RWMutex
	alphas map[string]Alpha
}

// NewAlphaRegistry returns an empty registry.
func NewAlphaRegistry() *AlphaRegistry {
	return &AlphaRegistry{alphas: make(map[string]Alpha)}
}

// ErrAlphaNotFound is returned by Get when no alpha is registered under a name.
type alphaNotFoundError struct{ name string }

func (e alphaNotFoundError) Error() string {
	return fmt.Sprintf("strategy: no alpha registered under %q", e.name)
}

// ErrAlphaExists is returned by Register when a name is already taken.
type alphaExistsError struct{ name string }

func (e alphaExistsError) Error() string {
	return fmt.Sprintf("strategy: alpha %q already registered", e.name)
}

// Register adds an alpha under its Name(). It returns an error on a nil alpha,
// an empty name, or a duplicate name (registration is not silently
// overwritten — a duplicate is a config bug worth surfacing).
func (r *AlphaRegistry) Register(a Alpha) error {
	if a == nil {
		return fmt.Errorf("strategy: cannot register a nil alpha")
	}
	name := a.Name()
	if name == "" {
		return fmt.Errorf("strategy: cannot register an alpha with an empty name")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.alphas[name]; exists {
		return alphaExistsError{name: name}
	}
	r.alphas[name] = a
	return nil
}

// Get returns the alpha registered under name, or an error if none is.
func (r *AlphaRegistry) Get(name string) (Alpha, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	a, ok := r.alphas[name]
	if !ok {
		return nil, alphaNotFoundError{name: name}
	}
	return a, nil
}

// Names returns the registered alpha names in sorted order (deterministic for
// logging/tests).
func (r *AlphaRegistry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.alphas))
	for n := range r.alphas {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Len returns the number of registered alphas.
func (r *AlphaRegistry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.alphas)
}

// ── Bundled alphas ──────────────────────────────────────────────────────────
//
// These read fields the live features.obi.v1 event already carries, so they
// double as worked examples of the data-extensibility story.

// FieldAlpha is the simplest possible Alpha: it reads a single named feature
// Value, scales it by Scale, clamps to [-1, 1], and reports a fixed Confidence
// (0 when the field is absent, so a missing data source contributes nothing
// rather than a spurious zero-with-confidence). It is stateless and therefore
// safe to share without locking.
//
// Example: FieldAlpha{Field: "obi", Scale: 1, Conf: 1} turns raw OBI in
// [-1, 1] directly into a long/short conviction.
type FieldAlpha struct {
	AlphaName string
	Field     string
	Scale     float64
	Conf      float64
	// Invert flips the sign: useful when a positive field value implies a
	// short (e.g. an over-bought sentiment reading).
	Invert bool
}

// Name implements Alpha.
func (a FieldAlpha) Name() string {
	if a.AlphaName != "" {
		return a.AlphaName
	}
	return "field:" + a.Field
}

// Compute implements Alpha.
func (a FieldAlpha) Compute(event model.FeatureEvent) AlphaScore {
	v, ok := event.Values[a.Field]
	if !ok {
		return AlphaScore{Value: 0, Confidence: 0}
	}
	scale := a.Scale
	if scale == 0 {
		scale = 1
	}
	val := v * scale
	if a.Invert {
		val = -val
	}
	return AlphaScore{Value: clampUnit(val), Confidence: clamp01(a.Conf)}
}

// MomentumAlpha blends the multi-timeframe momentum fields (momentum1m,
// momentum, momentum15m) the feature event carries into a single trend score.
// Positive ⇒ uptrend ⇒ long. It is stateless (reads only the event), so it
// needs no locking. Sensitivity scales raw momentum (typically small, e.g.
// 0.001–0.01) up into the [-1, 1] band.
type MomentumAlpha struct {
	AlphaName   string
	Sensitivity float64
}

// Name implements Alpha.
func (a MomentumAlpha) Name() string {
	if a.AlphaName != "" {
		return a.AlphaName
	}
	return "momentum"
}

// Compute implements Alpha.
func (a MomentumAlpha) Compute(event model.FeatureEvent) AlphaScore {
	m1, ok1 := event.Values["momentum1m"]
	m5, ok5 := event.Values["momentum"]
	m15, ok15 := event.Values["momentum15m"]
	n := 0
	var sum float64
	if ok1 {
		sum += m1
		n++
	}
	if ok5 {
		sum += m5
		n++
	}
	if ok15 {
		sum += m15
		n++
	}
	if n == 0 {
		return AlphaScore{Value: 0, Confidence: 0}
	}
	sens := a.Sensitivity
	if sens <= 0 {
		sens = 100 // raw momentum ~0.01 → ~1.0
	}
	avg := sum / float64(n)
	// Confidence rises with how many timeframes agree on sign.
	agree := 0
	for _, m := range []struct {
		v  float64
		ok bool
	}{{m1, ok1}, {m5, ok5}, {m15, ok15}} {
		if m.ok && (m.v > 0) == (avg > 0) {
			agree++
		}
	}
	conf := float64(agree) / 3.0
	return AlphaScore{Value: clampUnit(avg * sens), Confidence: clamp01(conf)}
}

// EMAReversionAlpha is the canonical STATEFUL alpha example: it maintains a
// rolling estimate from the emaFast/emaSlow fields and emits a mean-reversion
// score (price stretched far above the slow EMA ⇒ short). Because it holds
// mutable state (the last spread it saw, used to gate confidence on warm-up),
// it guards that state with a mutex per the strategy concurrency contract.
type EMAReversionAlpha struct {
	AlphaName string
	// Sensitivity scales the normalized (fast-slow)/slow stretch into [-1,1].
	Sensitivity float64

	mu       sync.Mutex
	warmed   bool
	lastFast float64
}

// Name implements Alpha.
func (a *EMAReversionAlpha) Name() string {
	if a.AlphaName != "" {
		return a.AlphaName
	}
	return "ema_reversion"
}

// Compute implements Alpha. Guards its rolling state with a.mu.
func (a *EMAReversionAlpha) Compute(event model.FeatureEvent) AlphaScore {
	fast, okF := event.Values["emaFast"]
	slow, okS := event.Values["emaSlow"]
	if !okF || !okS || slow == 0 {
		return AlphaScore{Value: 0, Confidence: 0}
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	// First observation only warms up state; emit zero confidence so a
	// cold-started alpha contributes nothing until it has context.
	confidence := 1.0
	if !a.warmed {
		a.warmed = true
		a.lastFast = fast
		confidence = 0
	}
	a.lastFast = fast

	stretch := (fast - slow) / slow
	sens := a.Sensitivity
	if sens <= 0 {
		sens = 200
	}
	// Mean reversion: stretched ABOVE slow ⇒ expect down ⇒ short (negative).
	val := -clampUnit(stretch * sens)
	return AlphaScore{Value: val, Confidence: clamp01(confidence)}
}

// ── Combination math (shared by CompositeStrategy and tests) ────────────────

// WeightedAlpha pairs an alpha with its blend weight and an optional
// per-alpha sign flip.
type WeightedAlpha struct {
	Alpha  Alpha
	Weight float64
}

// BlendMode selects how per-alpha scores are combined into the composite score.
type BlendMode int

const (
	// BlendWeightedSum combines as Σ Weight_i * Value_i (optionally * Conf_i).
	// This is the default.
	BlendWeightedSum BlendMode = iota
	// BlendZScore standardizes the weighted contributions across the alpha set
	// (subtract mean, divide by stdev) before summing, so no single alpha's raw
	// scale dominates. Falls back to the weighted sum when the set has <2
	// non-zero contributions or zero variance.
	BlendZScore
)

// combineScores blends per-alpha scores into one combined score in (roughly)
// [-1, 1]. When useConfidence is true each contribution is multiplied by its
// Confidence. Returns the combined score and the per-alpha weighted
// contributions (weight*value[*conf]) in the SAME order as the input slice, so
// callers can emit per-alpha observability without recomputing.
func combineScores(weighted []WeightedAlpha, scores []AlphaScore, mode BlendMode, useConfidence bool) (combined float64, contributions []float64) {
	contributions = make([]float64, len(scores))
	var totalWeight float64
	for i := range scores {
		w := weighted[i].Weight
		v := clampUnit(scores[i].Value)
		c := contribution(w, v, scores[i].Confidence, useConfidence)
		contributions[i] = c
		totalWeight += math.Abs(w)
	}

	switch mode {
	case BlendZScore:
		combined = zscoreBlend(contributions)
	default:
		var sum float64
		for _, c := range contributions {
			sum += c
		}
		// Normalize by total absolute weight so the combined score stays on a
		// stable scale regardless of how many alphas / how large the weights.
		if totalWeight > 0 {
			combined = sum / totalWeight
		} else {
			combined = sum
		}
	}
	return clampUnit(combined), contributions
}

// contribution is the single-alpha weighted contribution.
func contribution(weight, value, confidence float64, useConfidence bool) float64 {
	c := weight * value
	if useConfidence {
		c *= clamp01(confidence)
	}
	return c
}

// zscoreBlend standardizes the contributions and returns their standardized
// sum, squashed back into [-1,1] via tanh. Falls back to the plain mean when
// there is insufficient spread to standardize.
func zscoreBlend(contributions []float64) float64 {
	n := 0
	var sum float64
	for _, c := range contributions {
		if c != 0 {
			n++
		}
		sum += c
	}
	if n < 2 {
		return sum
	}
	mean := sum / float64(len(contributions))
	var variance float64
	for _, c := range contributions {
		d := c - mean
		variance += d * d
	}
	variance /= float64(len(contributions))
	if variance == 0 {
		return sum
	}
	std := math.Sqrt(variance)
	var z float64
	for _, c := range contributions {
		z += (c - mean) / std
	}
	// tanh squashes the standardized sum into (-1, 1).
	return math.Tanh(z / float64(len(contributions)))
}
