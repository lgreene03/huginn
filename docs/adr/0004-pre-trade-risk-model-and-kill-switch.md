# 0004. Centralized pre-trade risk model with a typed kill switch

- **Status:** Accepted
- **Date:** 2026-06-20
- **Deciders:** Project maintainer
- **Related:** [ADR-0003 — Dual-mode executor](0003-dual-mode-paper-live-executor.md), [risk-model.md](../risk-model.md), [STRATEGY_STATE_DESIGN.md](../STRATEGY_STATE_DESIGN.md)

## Context

A strategy can be wrong, a feed can stall, and an operator sometimes needs to stop everything *now*. None of that can live inside the strategies: each strategy already carries its own entry/exit logic, and duplicating loss limits and a halt switch across four (and growing) strategies would guarantee they drift. Risk must be a single chokepoint that every order passes through, in both paper and live mode, *before* the order is booked or published.

The controls needed are concrete: a peak-trailing drawdown stop, a daily-loss limit that resets on the UTC-day boundary, a gross position limit (with optional per-instrument overrides), and a watchdog that halts trading when feature events stop arriving (a stalled Muninn must not leave Huginn trading on stale data). Operators also need a manual halt/resume.

Two cross-cutting requirements complicate this. First, when something halts the engine, the *reason* matters for observability and for auto-recovery — a manual halt should not auto-clear, but a staleness halt should clear itself when fresh events resume. Second, the risk state that defines the limits (peak value, the day's PnL baseline, the last event time) must survive a restart, or a crash silently resets the drawdown ceiling and the daily-loss window.

## Decision

All pre-trade controls live in a single `risk.Manager`, and `executor.OnFeature` calls `riskManager.Evaluate(fill, snapshot)` for **every** order in both modes; a `false` return drops the order. `Evaluate` checks, in order: halt state → peak-trailing drawdown → daily-loss (UTC-windowed) → volatility-scaled position limit (per-instrument override taking precedence).

The halt state is a **typed reason**, not a bare boolean:

```go
type HaltReason string
const ( HaltNone; HaltManual; HaltDrawdown; HaltStaleness )
```

This drives behaviour: drawdown and staleness halts are set by the manager itself; manual halts come from the operator API. Only a `HaltStaleness` halt auto-clears, and only when `AutoResumeAfterStaleness` is configured and a fresh feature event arrives (`OnFeatureSeen`). Each reason is exported as a Prometheus gauge (`updateHaltGauges`) so a dashboard shows *why* trading stopped.

The position limit is volatility-scaled: `volScale = 1/(1 + 100*volPct)`, preferring Muninn's market-volatility feature when present and falling back to the dispersion of the strategy's own recent fill prices. The scaling is multiplicative on the configured hard limit, so a zero-volatility input collapses to the plain limit (preserving prior behaviour).

Restart-critical state (`peakValue`, `dayStart`, `dayStartRealizedPnL`, `lastFeatureEventTime`) is captured by the manager's own `MarshalState`/`RestoreState` (versioned envelope, schema v1), persisted on the same ticker as strategy state under the reserved `_risk` journal key, with a coarser `daily_pnl_snapshots` fallback if that blob is absent.

## Consequences

**Easier.**

- One place to read to know every way an order can be rejected, and one place to add a new control.
- Typed halt reasons make "why are we halted?" answerable from metrics alone, and let staleness recover automatically while keeping a manual halt sticky.
- Drawdown and daily-loss windows survive a crash, so recovery doesn't silently reset the risk ceiling.

**Harder / cost.**

- `Evaluate` takes the manager's write lock (it mutates `peakValue`, the daily window, and `recentPrices`), so it is on the hot path's critical section. Acceptable because dispatch is single-goroutine (ADR-0005) and the work is a handful of float comparisons.
- The volatility fallback (fill-price dispersion) only becomes meaningful after ~10 fills and is strategy-specific; the market-feature path is preferred precisely because it is fill-independent and available immediately. Operators should supply the volatility feature for the scaling to be trustworthy from the first trade.
- The `_risk` journal key is reserved — strategies must not use a key starting with underscore. Documented in `executor.go`.

## Alternatives Considered

- **Per-strategy risk logic.** Rejected. Guarantees drift across strategies and gives no single kill switch.
- **A bare `halted bool`.** Rejected. Cannot distinguish a manual halt (must stay halted) from a staleness halt (should auto-recover), and gives observability nothing to label.
- **Post-trade risk only (reconcile after booking).** Rejected. In live mode that would let an order reach the venue before any check; the kill switch must be able to stop an order from ever leaving Huginn.

## References

- [`internal/risk/manager.go`](../../internal/risk/manager.go) — `Evaluate`, `HaltReason`, `RunStalenessWatchdog`, `OnFeatureSeen`, `MarshalState`/`RestoreState`, `SeedFromBaseline`.
- [`internal/executor/executor.go`](../../internal/executor/executor.go) — the `Evaluate` call sites in both paper and live branches; `_risk` persistence on the state ticker.
- [`docs/risk-model.md`](../risk-model.md) — operator-facing description of each control.
