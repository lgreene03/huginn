# Adding an Alpha

Huginn's signal layer is **pluggable**. A new trading signal is one small type
that implements the `Alpha` interface plus one line of config — no change to the
executor, risk manager, portfolio, cost model, or any existing strategy. This is
the data/signal-extensibility story of the Norse stack: it is meant to read like
the rails a quant research team works on.

This guide walks the three steps end to end, using the bundled
`FundingRateAlpha` as a verbatim worked example of sourcing a **brand-new data
field**.

- Code: [`internal/strategy/alpha.go`](../internal/strategy/alpha.go) (the
  interface + generic helpers) and
  [`internal/strategy/alphas_bundled.go`](../internal/strategy/alphas_bundled.go)
  (the worked alphas).
- Wiring: [`internal/strategy/composite.go`](../internal/strategy/composite.go).

---

## The contract

An `Alpha` maps one feature event to a normalized score. It never emits orders,
never touches portfolio/risk state, and never reads the wall clock — it is a
**pure read** of the event plus its own internal state.

```go
type Alpha interface {
    Name() string                                 // unique registry key + metric label
    Compute(event model.FeatureEvent) AlphaScore  // pure read → normalized score
}

type AlphaScore struct {
    Value      float64 // [-1, 1]: SIGN = direction (>0 long, <0 short), MAGNITUDE = conviction
    Confidence float64 // [0, 1]: how much to trust this score right now (0 while warming up)
}
```

A `CompositeStrategy` blends a weighted set of alphas into one combined score,
applies an entry threshold to pick side + size, and routes the entry through the
**same** signed-position throttle, signed `maxPosition` cap, and net-of-cost
`CostHurdle` gate that `OBIThreshold` uses. You get all of that for free.

---

## Step 1 — Add your data to the feature event

Alphas read `model.FeatureEvent.Values`, a `map[string]float64` of named
features. The live `features.obi.v1` event already carries a rich multi-asset
set you can read today without changing anything:

```
obi, midPrice, microPrice, spread,
momentum, momentum1m, momentum15m,
emaFast, emaSlow, volatility,
funding, openInterest, fearGreed, mlScore, newsSentiment
```

To add a **new** field, publish it from the producer side:

- **obi-bridge / muninn** — compute the feature and add it to the event's
  `values` map under a stable key (e.g. `"funding"`). The bridge already
  publishes `funding`, `openInterest`, `fearGreed`, `mlScore`, and
  `newsSentiment`, so most new signals need no producer change at all — they
  just read a field that is already on the wire.

That is the whole data-side cost: one key in a map. No schema migration, no new
topic, no consumer change.

---

## Step 2 — Implement the `Alpha` interface

Here is the bundled new-data example **verbatim** — the entire cost of turning
the `funding` field into a tradeable contrarian signal:

```go
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
```

Three conventions worth copying:

1. **Absent field ⇒ `Confidence: 0`.** A missing data source contributes
   nothing rather than a spurious neutral read that a confidence-weighted
   composite would still trust.
2. **Normalize to `[-1, 1]`** with `clampUnit` so no single alpha can dominate
   the blend with a runaway scale.
3. **Sign is direction, magnitude is conviction.** Decide deliberately which
   way the signal points (here: contrarian, so we negate).

### Stateful alphas

If your alpha keeps rolling state (a moving average, a z-score window), guard it
with a `sync.Mutex` — `OnFeature` is single-goroutine, but state-persistence and
HTTP goroutines read strategy state concurrently (see the concurrency contract
in [`strategy.go`](../internal/strategy/strategy.go)). `MeanReversionAlpha` and
`EMAReversionAlpha` are the canonical guarded-state examples; emit
`Confidence: 0` until your window has warmed up.

### Regime filters

`VolatilityRegimeAlpha` shows the wrapper pattern: it takes an `Inner` alpha and
**scales its conviction** by the current `volatility` regime (full pass-through
when calm, ramping to zero when wild). Use it to say "trade this signal, but
only when the vol regime is friendly".

---

## Step 3 — Register and weight it

Register the alpha so a composite can be assembled from names + weights. The
bundled set lives in `DefaultAlphaRegistry()`:

```go
func DefaultAlphaRegistry() *AlphaRegistry {
    reg := NewAlphaRegistry()
    mustRegister(reg, ImbalanceAlpha{AlphaName: "imbalance"})
    mustRegister(reg, MomentumAlpha{AlphaName: "momentum"})
    mustRegister(reg, VolatilityRegimeAlpha{
        AlphaName: "vol_regime",
        Inner:     ImbalanceAlpha{AlphaName: "vol_regime_inner"},
    })
    mustRegister(reg, &MeanReversionAlpha{AlphaName: "mean_reversion"})
    mustRegister(reg, &EMAReversionAlpha{AlphaName: "ema_reversion"})
    mustRegister(reg, FundingRateAlpha{AlphaName: "funding_rate"}) // ← your line
    return reg
}
```

Then build a composite from a `(name → weight)` map. This is the config-driven
boot path — a YAML/env list of alpha names and weights becomes a running
strategy:

```go
reg := strategy.DefaultAlphaRegistry()
composite, err := strategy.NewCompositeFromRegistry(reg, map[string]float64{
    "imbalance":    0.5,
    "momentum":     0.3,
    "funding_rate": 0.2, // ← weight your new signal into the blend
}, strategy.CompositeConfig{
    EntryThreshold: 0.5,   // |combined| an entry must clear
    OrderSize:      0.01,
    UseConfidence:  true,  // de-weight cold / low-confidence alphas
})
```

Out of the box, `DefaultCompositeConfig(threshold, orderSize)` already wires a
ready-to-run three-alpha blend (OBI + multi-timeframe momentum + EMA reversion),
which is what the `composite` strategy switch in `cmd/huginn` and `cmd/backtest`
constructs. Add your alpha to that config (or build your own from the registry)
and it trades on the next event.

---

## What you get for free

Because the composite reuses the existing machinery, your new alpha inherits:

- the net-of-cost **`CostHurdle`** gate (inert by default),
- the **signed-position** portfolio (long + short) and `maxPosition` cap,
- the per-instrument single-position **throttle**,
- the **risk manager** path (drawdown, daily loss, staleness),
- per-alpha **Prometheus** observability (`AlphaContribution{alpha=…}` and
  `CompositeScore`), and
- the **walk-forward + PBO** validation gate and Deflated Sharpe in research.

So the marginal cost of a new signal is genuinely: one type, one weight. That is
the point.

---

## Checklist

- [ ] Field is on the event (`Values["yourField"]`) — produce it in obi-bridge /
      muninn, or reuse one already on the wire.
- [ ] Type implements `Alpha`: `Name()` + `Compute()`, normalized to `[-1, 1]`,
      `Confidence: 0` when the field is absent.
- [ ] Stateful? Guard with a mutex; emit `Confidence: 0` until warmed up.
- [ ] Registered in `DefaultAlphaRegistry()` (or your own registry).
- [ ] Weighted into a `CompositeConfig` / `NewCompositeFromRegistry` map.
- [ ] Unit test: known input → expected score sign + magnitude (see
      [`alphas_bundled_test.go`](../internal/strategy/alphas_bundled_test.go)).
