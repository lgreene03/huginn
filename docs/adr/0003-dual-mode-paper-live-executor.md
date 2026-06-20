# 0003. One executor, two modes (paper and live)

- **Status:** Accepted
- **Date:** 2026-06-20
- **Deciders:** Project maintainer
- **Related:** [ADR-0002 — Pluggable strategy interface](0002-pluggable-strategy-interface.md), [ADR-0004 — Pre-trade risk & kill switch](0004-pre-trade-risk-model-and-kill-switch.md), [architecture.md](../architecture.md)

## Context

Huginn must do two things that look superficially different. In **paper mode** (the default, and the only mode used in backtest, calibrate, and walk-forward) it simulates a fill locally — applying slippage, transaction cost, and an optional latency offset — and books it straight into the portfolio. In **live mode** it publishes an order intent to Sleipnir over Kafka (`executions.intents.v1`) and later receives the real fill asynchronously on `executions.fills.v1`.

The temptation is two code paths, or even two binaries. But the two modes must share everything that matters for correctness: the strategy dispatch, the pre-trade risk check, the portfolio accounting, the journal, and the metrics. If paper and live diverge, then a strategy validated in backtest is not the strategy that trades real money — which defeats the entire research-to-production pipeline the Norse stack is built around.

A subtlety: in paper mode the risk check and the fill are synchronous and local, so risk is evaluated against the *simulated* fill. In live mode the fill comes back from the venue later, so risk must be evaluated against a *prospective* fill built before publishing the intent, and the returned fill is booked on a separate inbound path.

## Decision

A single `Executor` type carries a `liveMode bool` and an optional `IntentPublisher`. `OnFeature` runs strategy dispatch, opt-in sizing, and metrics identically for both modes, then branches at the point of execution:

- **Paper:** `simulateFill` → `riskManager.Evaluate(fill, snapshot)` → journal → `portfolio.ApplyFill` → metrics → persist state.
- **Live:** build a *prospective* `model.Fill` at the estimated price → `riskManager.Evaluate(prospectiveFill, snapshot)` → `publisher.PublishIntent`. The real fill arrives later via `OnExecutionFill`, which journals and books it.

`IntentPublisher` is an interface (`PublishIntent(ctx, order, orderID) error`), so the executor depends on a capability, not on the Kafka producer. Tests inject a recording double; production injects `kafka.Producer`. Live mode is gated by the `LIVE_EXECUTION` env var / config flag and **defaults to off**, so the safe behaviour (simulate, never touch a venue) is the one you get by default.

## Consequences

**Easier.**

- A strategy validated in paper/backtest runs byte-identically up to the execution branch in live — same strategy code, same risk check, same portfolio math.
- The offline tools (`backtest`, `calibrate`, `walkforward`) reuse the exact production executor with `liveMode=false` and a `NullWriter`, so their results reflect the real engine, not a reimplementation.
- Swapping the publisher (e.g. a different gateway) is a one-interface change.

**Harder / cost.**

- The risk check is *structurally* the same in both modes but semantically asymmetric: paper risks the simulated fill, live risks a prospective fill at `estimatePrice(event)` with zero cost/slippage. This is correct (the real cost is unknown until the venue responds) but is a place a careless edit could make the two modes diverge. It is called out in `executor.go` and here.
- The inbound live-fill path is asynchronous and can receive duplicates (Sleipnir's boot reconciliation can re-emit a fill the WebSocket already delivered). This forces the dedup cache that ADR otherwise wouldn't need — see `dedup.go` and `OnExecutionFill`, keyed on `Fill.ExecutionID`.
- Live mode with no publisher configured logs a warning and drops the intent rather than crashing — a deliberate fail-safe, but one that can silently no-op if misconfigured.

## Alternatives Considered

- **Two separate binaries / packages.** Rejected. Guarantees drift between the validated strategy and the live one, duplicates the risk and portfolio code, and breaks the "backtest runs the real executor" property.
- **A `FillEngine` strategy-pattern interface (PaperEngine / LiveEngine).** Considered. Cleaner on paper, but the two modes share far more than they differ (everything up to the branch); a full Strategy-pattern split would scatter the shared dispatch/risk/metrics logic or duplicate it. The single-type branch keeps the shared path provably shared. Could be revisited if a third mode appears.
- **Evaluate risk only on the returned live fill.** Rejected. That would publish an intent to the venue *before* the risk check — the kill switch must be able to stop an order from ever leaving Huginn.

## References

- [`internal/executor/executor.go`](../../internal/executor/executor.go) — `OnFeature` branch, `simulateFill`, `OnExecutionFill`, `IntentPublisher`.
- [`internal/executor/dedup.go`](../../internal/executor/dedup.go) — bounded LRU dedup on `ExecutionID` for the inbound live path.
- [`internal/kafka/producer.go`](../../internal/kafka/producer.go) — the production `IntentPublisher` (publishes `GatewayOrder` to Sleipnir).
