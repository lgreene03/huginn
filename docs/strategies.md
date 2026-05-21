# Strategies

## Overview

Huginn ships four bundled strategies. Select one via `strategy.name` in the config or the `STRATEGY_NAME` env var:

| Name | Type | Source file |
|---|---|---|
| `obi` | Mean-reversion on order-book imbalance | `internal/strategy/obi_threshold.go` |
| `vpin` | Momentum on informed-flow probability | `internal/strategy/vpin_breakout.go` |
| `ema_crossover` | Trend-following via dual EMA | `internal/strategy/ema_crossover.go` |
| `vwap_deviation` | Mean-reversion on VWAP spread | `internal/strategy/vwap_deviation.go` |

All four implement the `Strategy` interface and the `Stateful` interface. Their internal state survives restarts via the journal.

---

## Concurrency contract

`OnFeature` is called from a **single goroutine**. Implementations are not required to be thread-safe on their own. The `Stateful` methods `MarshalState` / `RestoreState` are called while the dispatcher is paused; they also run single-threaded.

---

## OBI Threshold (`obi`)

**Signal hypothesis.** Order Book Imbalance measures the asymmetry between bid and ask volume at the top of the book:

```
OBI = (bid_volume - ask_volume) / (bid_volume + ask_volume)  ∈ [-1, 1]
```

Extreme positive OBI (more bids than asks) predicts a short-term price decline as the imbalance resolves — the engine **sells**. Extreme negative OBI predicts a price rise — the engine **buys**.

**Entry logic.**
- `OBI > threshold` → SELL `order_size` (fade the buy pressure)
- `OBI < -threshold` → BUY `order_size` (fade the sell pressure)
- Position is throttled by `max_position`; no new order is emitted when the limit is reached.

**Key parameters.**

| Parameter | Config key | Default | Sensitivity |
|---|---|---|---|
| Signal threshold | `strategy.threshold` | 0.7 | High. Lower values → more fills, higher noise. |
| Order size | `strategy.order_size` | 0.01 | Linear effect on notional exposure. |

**Known failure modes.**

1. **Regime change (trending market).** OBI is a mean-reversion signal. In a strong trending regime the imbalance is persistently one-sided; the strategy will fade the trend repeatedly and accumulate a loss. Calibrate `max_position` conservatively and monitor drawdown.
2. **Thin-book manipulation.** Layered spoofing can produce extreme OBI without real supply/demand imbalance. The strategy has no spoof-detection layer.
3. **Low-liquidity periods.** OBI computed from a shallow book is noisy. The strategy does not gate on minimum volume.

---

## VPIN Breakout (`vpin`)

**Signal hypothesis.** Volume-Synchronized Probability of Informed Trading (VPIN) measures the fraction of trading volume attributed to informed (toxic) flow. High VPIN indicates informed traders are active; this strategy enters **in the direction of the prevailing informed flow** with a cooldown.

**Entry logic.**
- `VPIN > threshold` and not in cooldown → enter with `order_size` in the direction of the last trade's `side` field.
- After entry, a configurable `cooldown_ms` prevents re-entry until the cooldown expires.

**Key parameters.**

| Parameter | Config key | Default | Sensitivity |
|---|---|---|---|
| Signal threshold | `strategy.threshold` | 0.5 | High. VPIN baseline varies by instrument. |
| Order size | `strategy.order_size` | 0.01 | Linear. |
| Re-entry cooldown | `strategy.cooldown_ms` | 5000 | Prevents churn; too long misses the move. |

**Known failure modes.**

1. **Calibration sensitivity.** VPIN's absolute value depends on bucket size and normalization — a threshold of 0.5 on one instrument is noise on another. Always calibrate per instrument.
2. **Direction ambiguity.** When the feature event does not carry a `side` field, the direction defaults to the last known side. Stale direction on a new VPIN spike can produce a wrong-way trade.
3. **Cooldown vs. persistence.** If informed flow persists across the cooldown window, the strategy misses subsequent entries. A cooldown that is too long sacrifices most of the alpha.

---

## EMA Crossover (`ema_crossover`)

**Signal hypothesis.** A fast exponential moving average crossing above a slow EMA indicates upward momentum (buy); crossing below indicates downward momentum (sell). Classic trend-following.

**Entry logic.**
- Warm-up: accumulates `slow_period` samples before emitting any signal.
- `fast_ema > slow_ema` after being `≤`: BUY `order_size`.
- `fast_ema < slow_ema` after being `≥`: SELL `order_size`.
- Position throttled by `max_position`.

**Key parameters.**

| Parameter | Config key | Default | Sensitivity |
|---|---|---|---|
| Fast period | `strategy.fast_period` | 12 | Lower → more crossovers, more whipsaw. |
| Slow period | `strategy.slow_period` | 26 | Higher → slower signal, smoother but lagging. |
| Order size | `strategy.order_size` | 0.01 | Linear. |

**Known failure modes.**

1. **Whipsaw in ranging markets.** EMA crossovers generate many false signals when price oscillates without trend. Expected to underperform OBI/VWAP in low-volatility sideways conditions.
2. **Warmup gap.** The first sample after warmup sets `prevFastEMA` and `prevSlowEMA` to the current price (equal). A large price jump on the first post-warmup event triggers an immediate crossover signal that is not backed by trend history. Use at least `slow_period × 2` events of warmup for reliable signals.
3. **Fixed periods.** EMA periods are not adapted to volatility regimes. A slow/fast pair tuned for daily bars behaves poorly on 1-second ticks.

---

## VWAP Deviation (`vwap_deviation`)

**Signal hypothesis.** When the current mid-price deviates from the rolling VWAP by more than `threshold_pct`, price is expected to revert toward VWAP. This is a mean-reversion signal based on intraday fair value.

**Entry logic.**
- `(price - vwap) / vwap > threshold_pct` → SELL (price above fair value, expect reversion down).
- `(price - vwap) / vwap < -threshold_pct` → BUY (price below fair value, expect reversion up).
- Position throttled by `max_position`.

**Key parameters.**

| Parameter | Config key | Default | Sensitivity |
|---|---|---|---|
| Deviation threshold | `strategy.threshold` | 0.001 (0.1%) | Very high. Tight threshold → constant signal. |
| Order size | `strategy.order_size` | 0.01 | Linear. |

**Known failure modes.**

1. **Regime-dependent VWAP.** Intraday VWAP anchors at the session open. In Muninn's current implementation, VWAP is a rolling windowed computation, not session-anchored. Deviation from rolling VWAP is not the same as deviation from true intraday VWAP. Expect different performance characteristics.
2. **Large sustained moves.** If price trends strongly away from VWAP (news-driven gap), this strategy will aggressively build a position against the trend until `max_position` is hit. Pairs poorly with low drawdown limits.
3. **Threshold too tight.** The default 0.001 (0.1%) produces a signal on nearly every tick for BTC-USDT during active hours. Calibrate; see [Calibration](calibration.md).

---

## Authoring a new strategy

### 1. Implement the interface

```go
package strategy

import "github.com/lgreene03/huginn/internal/model"

type MyStrategy struct {
    Threshold float64
    OrderSize float64
    // ...internal state...
}

func (s *MyStrategy) OnFeature(event model.FeatureEvent) []model.Order {
    // Return zero or more orders. Never block.
    // Called from a single goroutine — no locking needed for strategy-local state.
    return nil
}
```

### 2. Add crash-recovery (recommended)

```go
import "encoding/json"

type myStrategyState struct {
    Version     int     `json:"version"`
    NetPosition float64 `json:"net_position"`
    // ...
}

func (s *MyStrategy) MarshalState() ([]byte, error) {
    return json.Marshal(myStrategyState{Version: 1, NetPosition: s.netPosition})
}

func (s *MyStrategy) RestoreState(b []byte) error {
    var st myStrategyState
    if err := json.Unmarshal(b, &st); err != nil {
        return err
    }
    if st.Version != 1 {
        return fmt.Errorf("unknown state version %d", st.Version)
    }
    s.netPosition = st.NetPosition
    return nil
}
```

Always version the state blob from day one. Future schema changes increment the version field and add a migration path.

### 3. Register in the factory

In `cmd/huginn/main.go`, add your strategy to the `switch` block that creates the active strategy:

```go
case "my_strategy":
    strat = strategy.NewMyStrategy(cfg.Strategy)
```

### 4. Write tests

At minimum:
- Happy-path signal test: provide feature event(s) that should produce an order.
- No-signal test: provide events that should produce no order (threshold not exceeded, cooldown active, max position reached).
- State round-trip test: `MarshalState` → `RestoreState` → same output.
- Property test (recommended): use `testing/quick` to assert `OnFeature` never returns an order with `Quantity <= 0` or exceeding `MaxPosition`.

### Checklist

- [ ] `OnFeature` never blocks, never panics on nil fields, handles zero-value `FeatureEvent`
- [ ] `MarshalState` / `RestoreState` round-trip (with version field)
- [ ] Docstring includes: signal hypothesis, expected regime, known failure mode, key parameters
- [ ] Registered in `cmd/huginn/main.go` factory
- [ ] Tests cover: signal, no-signal, state round-trip
- [ ] Calibrated with `cmd/calibrate` before connecting to a live risk limit
