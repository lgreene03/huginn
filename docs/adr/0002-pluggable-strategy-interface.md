# 0002. Pluggable strategy interface with optional stateful recovery

- **Status:** Accepted
- **Date:** 2026-06-20
- **Deciders:** Project maintainer
- **Related:** [ADR-0003 — Dual-mode executor](0003-dual-mode-paper-live-executor.md), [strategies.md](../strategies.md), [STRATEGY_STATE_DESIGN.md](../STRATEGY_STATE_DESIGN.md)

## Context

Huginn ships four strategies — OBI threshold, VPIN breakout, EMA crossover, VWAP deviation — and is expected to grow more. Each consumes Muninn feature events and emits orders, but they differ wildly internally: OBI carries per-instrument open positions, entry/exit state, cooldowns, and a netPosition; EMA carries a continuously-mutating moving-average accumulator; a trivial threshold strategy carries nothing.

Two forces pull in opposite directions. First, the executor's hot path (`executor.OnFeature`) must treat every strategy identically — it cannot know which concrete type it holds. Second, strategies that carry state must survive a crash: if Huginn restarts mid-session, an EMA accumulator or a set of open OBI positions cannot silently reset to zero, or the risk picture and the strategy's own logic diverge from reality.

A third force is concurrency. `OnFeature` is dispatched from a single goroutine (see ADR-0005), but a 5-second ticker goroutine (`executor.RunStatePersister`) and the dashboard's `PUT /api/strategy/config` handler both touch strategy fields concurrently.

## Decision

A strategy is any type implementing a deliberately tiny interface:

```go
type Strategy interface {
    Name() string
    OnFeature(event model.FeatureEvent) []model.Order
}
```

State recovery is an **optional, separately-declared capability** via a second interface, not a method bolted onto `Strategy`:

```go
type Stateful interface {
    MarshalState() ([]byte, error)
    RestoreState([]byte) error
}
```

The executor type-asserts for `Stateful` (`if sf, ok := e.strategy.(strategy.Stateful); ok`) and only persists/restores when the strategy opts in. Stateless strategies stay free of persistence boilerplate. The contract is documented in the package doc of [`strategy.go`](../../internal/strategy/strategy.go): strategies must be self-synchronizing (per-instance `sync.Mutex`), must derive all decisions from event time (never wall-clock) for deterministic replay, and must never block.

State blobs are **versioned envelopes**: each strategy writes a `{version, fields}` wrapper (see `MarshalEnvelope`/`ParseEnvelope` in `stateful.go`) so the on-disk schema can evolve. `OBIThreshold.RestoreState` already handles both a v1 (netPosition only) and a v2 (full positions + last-trade times) payload, upgrading old blobs transparently.

## Consequences

**Easier.**

- Adding a strategy is a single new file implementing `OnFeature` plus a constructor wired in `cmd/huginn/main.go`; nothing in the executor, risk, or portfolio layers changes.
- Stateless strategies stay trivial — no persistence ceremony.
- Schema evolution is safe: bump the envelope version, add a `case` to `RestoreState`, and old journals still load.

**Harder / cost.**

- The mutex contract is a convention enforced by review and the `-race` test run (`go test -race ./...`), not by the compiler. A new strategy that mutates shared fields without locking will race against the persister goroutine. The package doc points new authors at `ema_crossover.go` as the canonical shape.
- Reading strategy parameters out for the dashboard (`executor.GetConfig`/`UpdateConfig`) requires a concrete-type `switch` over the four known strategies — the one place the executor is *not* polymorphic. A new strategy that wants live tuning must add itself to both switches. This is an accepted, localized leak documented here so the next author expects it.

## Alternatives Considered

- **One fat interface with `MarshalState`/`RestoreState` mandatory.** Rejected. Forces every stateless strategy to implement no-op persistence methods, and conflates "is a strategy" with "needs recovery".
- **Reflection-based generic state capture.** Rejected. Strategies hold unexported fields, time values, and maps that need bespoke serialization (RFC3339Nano timestamps, side enums); reflection would be fragile and would defeat the explicit version-gating that makes schema evolution safe.
- **A config-driven rules DSL instead of Go code.** Rejected for now. The strategies do non-trivial things (multi-timeframe momentum filters, regime-aware threshold adaptation, liquidation-cascade blocks); expressing that in a DSL would be a larger engine than the strategies themselves.

## References

- [`internal/strategy/strategy.go`](../../internal/strategy/strategy.go) — the `Strategy` interface and its concurrency/lifecycle contract.
- [`internal/strategy/stateful.go`](../../internal/strategy/stateful.go) — `Stateful`, `MarshalEnvelope`, `ParseEnvelope`.
- [`internal/strategy/obi_threshold.go`](../../internal/strategy/obi_threshold.go) — versioned `RestoreState` handling v1 and v2 blobs; regime-aware threshold adaptation.
- [`internal/executor/executor.go`](../../internal/executor/executor.go) — `PersistStrategyState`, the `Stateful` type-assertion, and the `GetConfig`/`UpdateConfig` type switch.
