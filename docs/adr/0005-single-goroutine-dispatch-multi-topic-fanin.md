# 0005. Multi-topic Kafka fan-in onto a single dispatch goroutine

- **Status:** Accepted
- **Date:** 2026-06-20
- **Deciders:** Project maintainer
- **Related:** [ADR-0002 — Pluggable strategy interface](0002-pluggable-strategy-interface.md), [ADR-0004 — Pre-trade risk & kill switch](0004-pre-trade-risk-model-and-kill-switch.md), [architecture.md](../architecture.md)

## Context

Muninn publishes features on multiple topics (`features.obi.v1`, `features.vwap.v1`, and so on), and Huginn may need to consume several at once — a strategy can fuse OBI with a volatility feature, and the risk model reads a market-volatility feature off the same event stream. So the consumer must read from an arbitrary configured list of topics.

But the rest of the engine is built on an assumption that makes everything downstream simpler: the strategy, the risk manager, and the portfolio are reasoned about as if `OnFeature` is never called concurrently with itself. The strategy package's concurrency contract (ADR-0002) is written around this; the portfolio's average-cost accounting and the risk manager's drawdown/daily-loss windows are far simpler if events are applied one at a time in a single order.

The tension: N topics naturally suggest N reader goroutines, but N concurrent calls into `OnFeature` would break that single-writer assumption everywhere downstream.

## Decision

Each configured topic gets its own `kafka-go` reader goroutine, but all of them **fan in** to one buffered channel (`chan model.FeatureEvent`, capacity 1000). A single **dispatcher goroutine** drains that channel and is the only caller of the handler. The guarantee — stated in `architecture.md` and the strategy package doc — is: **`OnFeature` is always called from exactly one goroutine.**

This makes per-topic reading concurrent (so a slow topic doesn't block a fast one at the network layer) while keeping the decision path strictly sequential. Adding a topic is a config change (`KAFKA_TOPICS`); no code changes, and the single-goroutine guarantee is unaffected by topic count.

## Consequences

**Easier.**

- The entire decision path — strategy → risk → executor → portfolio — can assume single-threaded application. The only genuinely concurrent operations are the state-persist ticker and the dashboard config handler, which is why those (and only those) require the strategy's mutex.
- Backpressure is explicit and bounded: the 1000-element channel absorbs bursts; if the dispatcher falls behind, readers block on the channel send rather than the system growing memory without bound.
- Adding/removing a topic is configuration, not code.

**Harder / cost.**

- The dispatcher is a throughput ceiling: every event from every topic is processed by one goroutine. For Huginn's workload (feature events, not raw ticks) this is ample, but it is a deliberate cap, not an accident. If a strategy ever needed true parallelism it would have to shard *behind* the dispatcher and re-establish ordering itself.
- Cross-topic ordering is not guaranteed. Events interleave in channel-arrival order, which depends on per-topic reader scheduling, not on event time. Strategies must therefore key their own state per-instrument/per-feature and not assume a global event-time ordering across topics (they already derive decisions from each event's own `EventTime`).
- A malformed message is logged and skipped per-reader (`json.Unmarshal` failure), so one bad record never stalls a topic — but it is silently dropped from the strategy's view.

## Alternatives Considered

- **One reader goroutine per topic calling the handler directly.** Rejected. Yields N concurrent `OnFeature` calls and forces fine-grained locking through strategy, risk, and portfolio — the opposite of the simplicity this design buys.
- **A single multi-topic consumer-group subscription.** Considered. `kafka-go`'s ergonomic `Reader` is single-topic; a `ConsumerGroup` consuming many topics in one loop would also serialize reads, removing the per-topic read concurrency without buying anything for Huginn's event volume.
- **An unbounded queue.** Rejected. Trades a bounded, observable backpressure point for an unbounded memory risk under a producer burst.

## References

- [`internal/kafka/consumer.go`](../../internal/kafka/consumer.go) — per-topic reader goroutines, the fan-in channel, and the single dispatcher.
- [`internal/strategy/strategy.go`](../../internal/strategy/strategy.go) — the concurrency contract that depends on this guarantee.
- [`docs/architecture.md`](../architecture.md) — "single-process, single-strategy engine; OnFeature is always called from a single goroutine."
