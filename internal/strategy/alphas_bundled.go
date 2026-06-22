package strategy

import (
	"math"
	"sync"

	"github.com/lgreene03/huginn/internal/model"
)

// ── Worked bundled alphas (quant-alpha-4) ───────────────────────────────────
//
// This file is the *worked-example* layer on top of the generic Alpha framework
// in alpha.go. Each type here reads fields the live features.obi.v1 event already
// carries and turns them into a normalized AlphaScore. They exist so a reader of
// the hiring portfolio can see, concretely, the four canonical signal shapes a
// quant desk works with:
//
//   - ImbalanceAlpha       — a direct micro-structure read (order-book imbalance)
//   - MomentumAlpha        — a multi-timeframe trend blend (defined in alpha.go)
//   - VolatilityRegimeAlpha — a REGIME FILTER that scales/zeros another alpha's
//     conviction by the current volatility regime
//   - MeanReversionAlpha   — a stateful z-score reversion (midPrice vs emaSlow)
//   - FundingRateAlpha     — the NEW-DATA extensibility example (~25 lines): a
//     brand-new signal sourced from a DIFFERENT event field (funding)
//
// None of these touch OBIThreshold or any existing strategy. They register into
// the default registry (see DefaultAlphaRegistry) so a CompositeStrategy is
// usable out of the box, and they are the verbatim material docs/ADDING_AN_ALPHA.md
// points at.

// ── ImbalanceAlpha ──────────────────────────────────────────────────────────

// ImbalanceAlpha reads the order-book imbalance field ("obi") and maps it
// directly to a long/short conviction: a positive book imbalance (more bid
// pressure) implies upward pressure ⇒ long. It is stateless and therefore safe
// to share without locking.
//
// obi already lives in [-1, 1] on the event, so the default Scale of 1 makes the
// raw imbalance the conviction. Scale lets a caller dampen (<1) or sharpen (>1,
// clamped) the response.
type ImbalanceAlpha struct {
	AlphaName string
	// Scale multiplies raw obi before clamping. Defaults to 1.
	Scale float64
}

// Name implements Alpha.
func (a ImbalanceAlpha) Name() string {
	if a.AlphaName != "" {
		return a.AlphaName
	}
	return "imbalance"
}

// Compute implements Alpha. Absent obi ⇒ zero confidence (the data source
// contributes nothing rather than a spurious neutral read).
func (a ImbalanceAlpha) Compute(event model.FeatureEvent) AlphaScore {
	obi, ok := event.Values["obi"]
	if !ok {
		return AlphaScore{Value: 0, Confidence: 0}
	}
	scale := a.Scale
	if scale == 0 {
		scale = 1
	}
	return AlphaScore{Value: clampUnit(obi * scale), Confidence: 1}
}

// ── VolatilityRegimeAlpha ───────────────────────────────────────────────────

// VolatilityRegimeAlpha is a REGIME FILTER, not a directional signal on its own.
// It wraps an Inner alpha and scales that alpha's conviction by a multiplier
// derived from the current "volatility" field:
//
//   - In a calm-to-normal regime (volatility <= Low) it passes the inner alpha
//     through unchanged (multiplier 1).
//   - As volatility rises from Low toward High the multiplier ramps linearly
//     down to 0, throttling conviction in choppy markets.
//   - Above High the regime is "too wild to trade" and the multiplier is 0, so
//     the wrapped alpha contributes nothing.
//
// This is the canonical way a desk says "trade this signal, but only when the
// vol regime is friendly". It scales BOTH Value (so the blended score shrinks)
// and Confidence (so a confidence-weighted composite also de-weights it),
// matching the brief's "scales/zeros conviction".
//
// It is stateless beyond the Inner alpha; any state lives in Inner, which guards
// itself. VolatilityRegimeAlpha adds no mutable state of its own.
type VolatilityRegimeAlpha struct {
	AlphaName string
	// Inner is the directional alpha whose conviction this regime filter gates.
	Inner Alpha
	// Low is the volatility at/below which the inner alpha passes through fully.
	// High is the volatility at/above which conviction is zeroed. Low<High.
	Low  float64
	High float64
}

// Name implements Alpha.
func (a VolatilityRegimeAlpha) Name() string {
	if a.AlphaName != "" {
		return a.AlphaName
	}
	if a.Inner != nil {
		return "vol_regime:" + a.Inner.Name()
	}
	return "vol_regime"
}

// regimeMultiplier maps the current volatility to a conviction scale in [0, 1].
func (a VolatilityRegimeAlpha) regimeMultiplier(vol float64) float64 {
	low, high := a.Low, a.High
	// Safe defaults if unconfigured: ramp from 0.0 to 0.05 raw vol.
	if low <= 0 && high <= 0 {
		low, high = 0.0, 0.05
	}
	if high <= low {
		// Degenerate band: behave as a hard cutoff at low.
		if vol <= low {
			return 1
		}
		return 0
	}
	if vol <= low {
		return 1
	}
	if vol >= high {
		return 0
	}
	// Linear ramp down across the band.
	return 1 - (vol-low)/(high-low)
}

// Compute implements Alpha. With no Inner it is inert (zero confidence).
func (a VolatilityRegimeAlpha) Compute(event model.FeatureEvent) AlphaScore {
	if a.Inner == nil {
		return AlphaScore{Value: 0, Confidence: 0}
	}
	inner := a.Inner.Compute(event)
	vol, ok := event.Values["volatility"]
	if !ok {
		// No volatility field ⇒ no regime information ⇒ pass the inner score
		// through unchanged. (Absence is not evidence of a bad regime.)
		return inner
	}
	m := a.regimeMultiplier(vol)
	return AlphaScore{
		Value:      clampUnit(inner.Value * m),
		Confidence: clamp01(inner.Confidence * m),
	}
}

// ── MeanReversionAlpha ──────────────────────────────────────────────────────

// MeanReversionAlpha is the canonical STATEFUL z-score reversion alpha. It
// maintains a rolling mean and variance of the deviation of midPrice from the
// slow EMA (emaSlow) and emits a score proportional to the standardized
// deviation, with the sign FLIPPED: a midPrice stretched far ABOVE its slow EMA
// (large positive z) is expected to revert down ⇒ short (negative score).
//
// It uses Welford's online algorithm so the rolling statistics are O(1) per
// event and numerically stable. Because it carries mutable state it guards that
// state with a mutex, per the strategy concurrency contract (see strategy.go).
type MeanReversionAlpha struct {
	AlphaName string
	// WarmUp is the number of observations required before the alpha reports
	// non-zero confidence (so a cold start contributes nothing). Defaults to 20.
	WarmUp int
	// Sensitivity scales the z-score into [-1, 1] before clamping. Defaults to
	// 0.5 (so a 2σ deviation maps to a full-conviction ±1).
	Sensitivity float64

	mu    sync.Mutex
	count int
	mean  float64
	m2    float64 // sum of squares of differences from the running mean
}

// Name implements Alpha.
func (a *MeanReversionAlpha) Name() string {
	if a.AlphaName != "" {
		return a.AlphaName
	}
	return "mean_reversion"
}

// Compute implements Alpha. Guards rolling state with a.mu.
func (a *MeanReversionAlpha) Compute(event model.FeatureEvent) AlphaScore {
	mid, okMid := event.Values["midPrice"]
	slow, okSlow := event.Values["emaSlow"]
	if !okMid || !okSlow || slow == 0 {
		return AlphaScore{Value: 0, Confidence: 0}
	}
	// Deviation as a fraction of the slow EMA keeps the statistic scale-free
	// across instruments with very different price levels.
	dev := (mid - slow) / slow

	a.mu.Lock()
	defer a.mu.Unlock()

	// Welford online update.
	a.count++
	delta := dev - a.mean
	a.mean += delta / float64(a.count)
	delta2 := dev - a.mean
	a.m2 += delta * delta2

	warmUp := a.WarmUp
	if warmUp <= 0 {
		warmUp = 20
	}
	if a.count < warmUp || a.count < 2 {
		return AlphaScore{Value: 0, Confidence: 0}
	}

	variance := a.m2 / float64(a.count-1)
	if variance <= 0 {
		return AlphaScore{Value: 0, Confidence: 0}
	}
	std := math.Sqrt(variance)
	z := (dev - a.mean) / std

	sens := a.Sensitivity
	if sens <= 0 {
		sens = 0.5
	}
	// Mean reversion: stretched ABOVE the mean ⇒ expect reversion DOWN ⇒ short.
	val := -clampUnit(z * sens)
	// Confidence ramps up over the warm-up window, capped at 1.
	conf := clamp01(float64(a.count) / float64(warmUp*2))
	return AlphaScore{Value: val, Confidence: conf}
}

// ── FundingRateAlpha (NEW-DATA extensibility example) ───────────────────────
//
// This is the worked "add a brand-new data source" example referenced verbatim
// in docs/ADDING_AN_ALPHA.md. The obi-bridge already publishes a "funding" field
// on every event; this ~25-line alpha is the entire cost of turning that data
// into a tradeable signal — no change to any existing strategy, executor, or
// risk code.
//
// Reasoning: a large POSITIVE perpetual funding rate means longs are paying
// shorts (crowded longs) — a contrarian short bias. So we INVERT the sign:
// positive funding ⇒ negative (short) conviction. Stateless, no locking.

// FundingRateAlpha turns the perpetual "funding" rate into a contrarian
// conviction: crowded longs (positive funding) bias short, and vice versa.
type FundingRateAlpha struct {
	AlphaName string
	// Scale maps a raw funding rate (e.g. 0.01 = 1bp/interval) into [-1, 1].
	// Defaults to 100 (so 1% funding saturates to full conviction).
	Scale float64
}

// Name implements Alpha.
func (a FundingRateAlpha) Name() string {
	if a.AlphaName != "" {
		return a.AlphaName
	}
	return "funding_rate"
}

// Compute implements Alpha. Absent funding ⇒ zero confidence.
func (a FundingRateAlpha) Compute(event model.FeatureEvent) AlphaScore {
	funding, ok := event.Values["funding"]
	if !ok {
		return AlphaScore{Value: 0, Confidence: 0}
	}
	scale := a.Scale
	if scale == 0 {
		scale = 100
	}
	// Contrarian: positive funding (crowded longs) ⇒ short ⇒ negative.
	return AlphaScore{Value: clampUnit(-funding * scale), Confidence: 1}
}

// ── Default registry: composite usable out of the box ───────────────────────

// DefaultAlphaRegistry returns an AlphaRegistry pre-populated with every bundled
// alpha under its default Name(), so a config-driven boot path (or a test) can
// assemble a CompositeStrategy purely from names + weights. Each alpha gets a
// fresh instance — the stateful ones (MeanReversionAlpha, EMAReversionAlpha)
// must not be shared across composites.
//
// Registered names:
//
//	imbalance, momentum, vol_regime, mean_reversion, ema_reversion, funding_rate
//
// The vol_regime entry wraps the imbalance alpha as its inner signal — the
// most common pairing (gate a micro-structure signal by the vol regime). A
// caller wanting a different pairing constructs VolatilityRegimeAlpha directly.
func DefaultAlphaRegistry() *AlphaRegistry {
	reg := NewAlphaRegistry()
	// Errors here are impossible (fixed, unique names) but checked so a future
	// rename that introduces a collision fails loudly in tests.
	mustRegister(reg, ImbalanceAlpha{AlphaName: "imbalance"})
	mustRegister(reg, MomentumAlpha{AlphaName: "momentum"})
	mustRegister(reg, VolatilityRegimeAlpha{
		AlphaName: "vol_regime",
		Inner:     ImbalanceAlpha{AlphaName: "vol_regime_inner"},
	})
	mustRegister(reg, &MeanReversionAlpha{AlphaName: "mean_reversion"})
	mustRegister(reg, &EMAReversionAlpha{AlphaName: "ema_reversion"})
	mustRegister(reg, FundingRateAlpha{AlphaName: "funding_rate"})
	return reg
}

// mustRegister panics on a registration error. Only used with fixed, unique
// names in DefaultAlphaRegistry, where an error is a programming bug.
func mustRegister(reg *AlphaRegistry, a Alpha) {
	if err := reg.Register(a); err != nil {
		panic("strategy: DefaultAlphaRegistry: " + err.Error())
	}
}
