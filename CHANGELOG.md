# Changelog

All notable changes to Huginn are documented here. Format follows [Keep a Changelog](https://keepachangelog.com/en/1.0.0/). Versions follow [Semantic Versioning](https://semver.org/).

---

## [Unreleased]

### Added
- **Phase 9 — Research / validation gateway** (`cmd/research`). Standalone HTTP service (port `8094`, `RESEARCH_PORT`) that runs walk-forward + PBO + Deflated-Sharpe validation **out of the live trading process**. `POST /api/research/runs` submits a `{strategy, thresholds, folds}` job (async, returns `202`); `GET /api/research/runs` and `GET /api/research/runs/{id}` read results; `GET /healthz` for liveness. Reuses the same `internal/research` engine as `cmd/walkforward`, owns no Kafka/Postgres, replays a JSONL dataset on disk, and persists finished runs to `RESEARCH_RESULTS_DIR` (reloaded on restart). Container image: `Dockerfile.research`. Documented in README, CLAUDE.md, and `docs/ROADMAP.md` Phase 9.
- **Committed test fixture** `data/btc_test.jsonl` (~550KB) — the dataset the research gateway and walk-forward replay read — so `clone -> run` works config-free.
- Surfaced previously-orphaned docs in the MkDocs nav: `docs/ADDING_AN_ALPHA.md`, `docs/BENCHMARKS.md`, `docs/STRATEGY_STATE_DESIGN.md`.

### Fixed
- README/CLAUDE.md strategy count corrected from "four" to the six bundled strategies (`obi`, `vpin`, `vwap_deviation`, `ema_crossover`, `ou`, `composite`) plus the pluggable alpha framework (`internal/strategy/alpha.go`, `alphas_bundled.go`, `composite.go`).
- README Docker quick-start health check now uses host port `8083` (compose publishes Huginn on `8083:8081`), not `8081`.
- `.gitignore`: ignore the per-run output dir `data/research/`; stop ignoring the committed `data/btc_test.jsonl` fixture.
- Removed the stale empty tracked file `data/backtest_trades.jsonl` (now gitignored).
- CLAUDE.md backtest example now references the shipping `data/btc_test.jsonl` fixture instead of the non-existent `data/features.jsonl`.
- `Dockerfile.research`: replaced the dangling "integration notes" reference with the norse-stack compose location.

---

## [0.8.0] — 2026-06-19

### Added
- **Phase 8 — Muninn SSE feature-stream consumer.** New `feed.source=stream` mode connects to Muninn's `GET /api/v1/features/stream` (SSE) endpoint as an alternative to Kafka topic consumption. Configured via `FEED_SOURCE`, `FEED_STREAM_URL`, `FEED_STREAM_FEATURE`. See [ADR-0009](https://github.com/lgreene03/muninn/blob/main/docs/adr/0009-streaming-features-sse.md) on the Muninn side.
- **Standalone smoke test** (`scripts/smoke.sh`) — boots Docker, pushes a synthetic OBI event, verifies the strategy fires and portfolio updates.
- **CLAUDE.md** — agent-oriented project context for AI assistants.
- `gosec` linter enabled in golangci-lint with G304 suppressed globally (internal CLI paths only) and all gosec rules suppressed in test files.
- Documented concurrency contract on the `Strategy.OnFeature` interface method.

### Fixed
- Dockerfile now copies `configs/` directory into the image (was missing, causing container crash on startup).
- README GitHub links corrected from `github.com/lgreene/` to `github.com/lgreene03/`.
- `/api/fills/mock` now routes through the executor for full journal parity with real Sleipnir fills.
- Directory permissions tightened from `0755` to `0750` (gosec G301) in calibrate, fetcher, and journal writer.
- Journal file permissions tightened from `0644` to `0600` (gosec G302).
- `ReadHeaderTimeout` added to HTTP server to prevent Slowloris attacks (gosec G112).

---

## [0.7.0] — 2026-06-08

### Added
- **Phase 7 — Release engineering.** Multi-arch GHCR release workflow, Dockerfile digest pinning, `internal/version` package, `CONTRIBUTING.md`, `SECURITY.md`, `CODE_OF_CONDUCT.md`, golangci-lint in CI.
- **MkDocs documentation site** with architecture, strategies, risk model, and calibration guides. Deployed to GitHub Pages.

---

## [0.6.0] — 2026-06-02

### Added
- **Phase 6 — Operator dashboard.** React 19 + Vite + TypeScript web UI: real-time SSE equity curve, positions table, fills log, manual halt/resume, manual-fill console. Nginx production config. Strategy config panel with live parameter display.
- **Phase 3 — Observability.** OpenTelemetry W3C TraceContext propagation via Kafka headers. Risk-aware Prometheus metrics, `/api/snapshot` endpoint, Grafana dashboard JSON.
- Playwright end-to-end smoke test for the web dashboard.
- Bearer-token auth for mutating HTTP endpoints (`HUGINN_API_TOKEN`).
- `/api/strategy/config` GET/PUT for live parameter tuning.
- Equity history ring buffer with `/api/snapshot/history` endpoint.

### Fixed
- ESLint, gofmt, goimports, misspell, and staticcheck findings resolved.
- golangci-lint migrated to v2 format for Go 1.25 compatibility.

---

## [0.5.0] — 2026-05-22

### Added
- **Phase 5 — Journal hardening.** Versioned Postgres migrations, connection pool tunables, daily PnL snapshots, operations runbook.

---

## [0.4.0] — 2026-05-16

### Added
- **Phase 4 — Backtest engine.** Multi-strategy support via `AddExecutor`, order-book-aware fill pricing with latency model, `--report` flag for self-contained HTML reports, parity test against live path.
- Calibration CLI (`cmd/calibrate`) for grid-search over strategy parameters.

### Fixed
- Year-boundary collapse in daily equity sampler corrected.

---

## [0.3.0] — 2026-05-10

### Added
- **Phase 2 — Strategy calibration.** Walk-forward and property-based tests for all four strategies. Per-strategy failure-mode documentation. Analysis metrics (Sharpe, MDD, hit rate).

---

## [0.2.0] — 2026-05-04

### Added
- **Phase 1 — Risk hardening.** Daily loss reset with UTC-day boundary tracking, peak-value persistence to journal, per-instrument position limits, feature-staleness watchdog with auto-halt/resume.
- Stateful strategy interface — strategies survive process restart via journal recovery.
- Live-fill deduplication by `ExecutionID` (companion to Sleipnir).

---

## [0.1.0] — 2026-04-28

### Added
- **Phase 0 — Foundations.** Four bundled strategies (OBI Threshold, VPIN Breakout, EMA Crossover, VWAP Deviation), dual-mode executor (paper/live), thread-safe portfolio tracker (FIFO avg-cost), pluggable journal (JSONL + Postgres), multi-topic Kafka consumer, HTTP observability server, Docker Compose stack with Redpanda and Postgres.
- Sleipnir integration: intent producer → fills consumer pipeline.
- Historical data fetcher (`cmd/fetcher`) for Binance aggTrades → windowed features.
