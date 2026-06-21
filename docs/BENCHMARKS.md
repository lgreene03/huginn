# Huginn Hot-Path Benchmarks

Microbenchmarks for the three latency-critical stages of the per-event
pipeline: **strategy decision** (`Strategy.OnFeature`), **fill simulation**
(`Executor.simulateFill`), and **portfolio application** (`Portfolio.ApplyFill`).
Together these are the synchronous signal-to-decision-to-ledger path the
dispatcher runs for every consumed feature event.

## How to run

```bash
make bench
# or directly:
go test -bench=. -benchmem -run=^$ ./...
```

The benchmark functions live next to the code they measure:

- `internal/strategy/obi_bench_test.go` — `OnFeature` dispatch (signal-to-decision)
- `internal/executor/executor_bench_test.go` — `simulateFill`
- `internal/portfolio/portfolio_bench_test.go` — `ApplyFill`

## Method note

Each benchmark drives the real production code path with a representative
`FeatureEvent` / `Fill`. The strategy and portfolio benchmarks redirect the
default `slog` logger to `io.Discard` so the figures reflect compute cost, not
log-formatting I/O. `b.ReportAllocs()` records allocations. Throughput is
derived as `1e9 / ns_per_op` (single-goroutine; the dispatcher calls these from
one goroutine per the package's concurrency contract).

## Environment

| | |
|---|---|
| Machine | Apple M4 (10 logical CPUs), macOS (darwin/arm64) |
| Go      | go1.25.7 |
| Mode    | single goroutine, `-benchmem`, `-benchtime=3s` |
| Date    | 2026-06-21 |

## Results

Numbers below are from `-benchtime=3s` and were stable across repeated runs
(±2% on ns/op; allocs/op identical run-to-run).

| Benchmark | Stage | ns/op | B/op | allocs/op | Throughput (events/s) |
|---|---|---:|---:|---:|---:|
| `BenchmarkOBIOnFeature_Signal`   | strategy decision, **entry fires** (all filters run, position opens, order formatted) | 418.9 | 312 | 12 | ~2.39M |
| `BenchmarkOBIOnFeature_NoSignal` | strategy decision, **dead-zone** (no order — the common case) | 378.5 | 104 | 9 | ~2.64M |
| `BenchmarkSimulateFill`          | executor fill simulation (book-aware price + sqrt market-impact slippage) | 60.5 | 32 | 2 | ~16.5M |
| `BenchmarkApplyFill`             | portfolio signed-fill apply (cash/PnL math + audit-ledger append) | 892.7 | 946 | 19 | ~1.12M |

### Reading the numbers

- **Strategy decision** dominates the path at ~380–420 ns. The `Signal` case is
  ~40 ns dearer than `NoSignal` because it formats the order's human-readable
  `Reason` string and opens a position. Both are far below the per-event budget:
  even the heaviest stage sustains >2M decisions/sec on one core, orders of
  magnitude above realistic feature-event arrival rates.
- **`simulateFill`** is the cheapest stage (~60 ns, 2 allocs) — its only
  heap traffic is the two `fmt.Sprintf`-built `OrderID` / fill fields.
- **`ApplyFill`** is the most allocation-heavy (19 allocs/op, ~900 ns). The bulk
  is the unbounded `fills` audit slice (it grows to `b.N` entries over the run,
  so the B/op figure includes amortised slice-doubling) plus the signed
  cash/PnL/average-cost arithmetic. In production the ledger is drained/snapshotted,
  so steady-state per-fill cost is lower than the growth-inclusive figure here.

### End-to-end implication

Summing the synchronous stages for an event that produces one taker fill
(`OnFeature_Signal` + `simulateFill` + `ApplyFill`) gives roughly
**420 + 61 + 893 ≈ 1.37 µs** of compute per fill-producing event, i.e. a
single-core ceiling around **730K fill-producing events/sec** before any
journal/Kafka/metric I/O. Most events take the no-signal path (~379 ns), so the
realistic sustained dispatch ceiling is bounded by `OnFeature` at >2.6M
events/sec/core. Network, journal, and Prometheus I/O — not this compute path —
are the real-world throughput limiters.

> These are microbenchmarks of in-process compute only. They deliberately
> exclude Kafka deserialization, the journal writer, risk evaluation, and
> Prometheus observation, which sit outside the three measured functions.
