# Huginn — Roadmap

Phased delivery, mirroring the discipline of the [Muninn server ROADMAP](https://github.com/lgreene03/muninn/blob/main/docs/steering/ROADMAP.md) and [muninn-py](https://github.com/lgreene03/muninn-py/blob/main/docs/ROADMAP.md). Each phase ends with a working, tested, documented increment. Phases are not skipped.

> **Reading this for the first time?** Start with **Current state assessment** below to understand what is already in `main`, then read the phases.

---

## Current state assessment

### What is built and working

- **Strategy interface** (`internal/strategy/strategy.go`): single-method `OnFeature(FeatureEvent) []Order` — clean, no concurrency contract documented but `EMACrossover` self-mutexes, the others do not.
- **Four strategies implemented**: `OBIThreshold` (`obi_threshold.go`), `VPINBreakout` (same file, lines 87–143), `VWAPDeviation` (`vwap_deviation.go`), `EMACrossover` (`ema_crossover.go`). Each tested with at least one happy-path and one negative case (`internal/strategy/strategy_test.go`, 207 lines, 6 tests).
- **Risk manager** (`internal/risk/manager.go`): peak-trailing drawdown stop (`peakValue` ratchet), daily-loss limit on `RealizedPnL`, hard position limit, **volatility-scaled position limit** (CV of last 30 fill prices, lines 106–141), manual halt/resume, dynamic `UpdateLimits`. `manager_test.go` covers all four reject paths.
- **Executor** (`internal/executor/executor.go`): dual-mode — paper sim with bps slippage + tx cost on `microPrice`/`value`, or live mode that publishes order intents to Kafka (`executions.intents.v1`) and ingests fills from Sleipnir (`executions.fills.v1`). Dynamic `UpdateConfig` / `GetConfig` for live retuning.
- **Portfolio** (`internal/portfolio/portfolio.go`): thread-safe FIFO-avg-cost book, realized PnL on sell, unrealized via `LastMarkPrice`. `portfolio_test.go` validates round-trip PnL.
- **Journal** (`internal/journal/`): pluggable `Writer` interface, JSONL writer + JSONL reader/replay for crash recovery, Postgres writer with auto-init schema (`trade_fills` table, lines 51–67 of `postgres.go`) and replay-from-DB recovery.
- **Kafka** (`internal/kafka/`): multi-topic fan-in consumer with buffered channel + per-topic goroutines, intent producer (`segmentio/kafka-go`, `RequireAll` acks), fills consumer.
- **HTTP server** (`internal/server/http.go`): `/healthz` (returns portfolio snapshot), `/readyz`, `/metrics` (Prom), `/api/stream` (SSE @ 500 ms), `/api/breaker/{trigger,reset}`, `/api/fills/mock`.
- **Backtest engine** (`internal/backtest/engine.go`): JSONL replay, daily equity sampling (by `event.EventTime.YearDay()`), terminal Sharpe + MDD via `internal/metrics/analysis.go`.
- **Historical fetcher** (`cmd/fetcher/main.go`): Binance `aggTrades` → windowed OBI/VPIN/VWAP → JSONL. Used to feed the backtest engine.
- **Web UI** (`web/`): React 19 + Vite + TS dashboard with neon-cyan theme. Connects to `http://localhost:8083/api/stream` (SSE), renders inline-SVG equity curve, positions table, fills log, manual halt/resume buttons, manual-fill console.
- **Docker Compose** (`docker-compose.yml`): huginn + dashboard + sleipnir + redpanda + postgres. Sleipnir is wired as a sibling service pointing at Binance testnet.
- **CI**: `.github/workflows/ci.yml` — Go 1.23, `go mod verify`, build, vet, `go test ./...`. Single Linux job, no race, no coverage, no matrix.

### What is broken right now

1. **`go build ./...` fails on `main`.** `cmd/huginn/main.go:146` calls `server.New(":"+cfg.Server.Port, port, riskManager)` (3 args) but `internal/server/http.go:30` signature is `New(addr, portf, riskMgr, exec *executor.Executor)`. Mid-refactor leak. The CI workflow above would catch this on the next push — but the last commit went in red, or CI is not actually green. **This is Phase 0.**
2. **`web/package.json` pins `typescript ~6.0.2` and `vite ^8.0.12`.** Neither version exists yet on npm (TS is at 5.x, Vite at 6.x). `npm ci` will fail. Either the deps were hand-edited optimistically or this is dead-on-arrival for fresh clones.
3. **`docker-compose.yml`** references a `sleipnir` build context at `../sleipnir` — fine on the author's machine, broken for any other clone or CI.

### What the README claims but isn't real

- README's strategy table only mentions `obi` and `vpin`; actually four are wired (`vwap_deviation`, `ema_crossover`).
- README claims `VPIN Breakout` is in its own file — it lives inside `obi_threshold.go` at line 87. Cosmetic but worth fixing.
- README's environment-variable table omits `LIVE_EXECUTION`, `KAFKA_INTENTS_TOPIC`, `KAFKA_FILLS_TOPIC`, `DATABASE_ENABLED`, `DATABASE_URL`, and all four EMA/VWAP strategy params.
- "No real exchange API connections" in Non-Goals contradicts the `LiveExecution=true` path that publishes to Sleipnir, which is itself a Binance gateway. The non-goal needs to be reframed: Huginn itself never opens an exchange socket; Sleipnir does, and Huginn only ever speaks to Sleipnir over Kafka.

### Strategy quality gaps

- **No calibration story.** OBI threshold 0.7, VPIN 0.5, VWAP 0.001 — these are folklore values in the configs. There is no script, notebook, or sweep CLI that picks them from historical data. The fetcher writes JSONL; the backtest consumes JSONL; there is no glue to grid-search.
- **No documented failure mode per strategy.** What happens to OBI mean-reversion in a regime change? When does EMA crossover whipsaw? Not in docstrings.
- **State leaks between backtest runs.** `NewOBIThreshold` etc. start `netPosition=0`, but the live process re-uses an in-memory strategy across recoveries — strategy `netPosition` is **not** recovered from the journal, only the portfolio is. After a restart, OBI/EMA/VWAP will happily re-build position past the throttle limit.
- **EMA warmup logic is off-by-one.** `s.count < s.SlowPeriod` will produce orders on the very first post-warmup sample where `prevFastEMA == prevSlowEMA == price` is possible from the initialization at line 63–64. The `strategy_test.go:158` test passes by setting up a specific sequence; a fuzzed sequence will flag a bogus crossover.
- **Concurrency contract is inconsistent.** `OBIThreshold`, `VPINBreakout`, `VWAPDeviation` mutate `netPosition`/`lastTrade` with no lock. The executor calls `OnFeature` from a single goroutine today (the dispatcher in `consumer.go`), but the interface doesn't document that.

### Risk management gaps

- **Daily loss limit is not daily.** `manager.go:97` compares `snap.RealizedPnL` (all-time) to `-DailyLossLimit`. No date roll. Once you hit the limit, you stay halted forever, even on a fresh trading day.
- **Drawdown gauge tracks peak across all time, not session.** Same issue as above — on a recovered portfolio, `peakValue` resets to `initialCash` (`manager.go:29`) even if the recovered portfolio is already at $120k. First drawdown after restart will misfire.
- **Volatility scaling uses recent fill prices, not feature prices.** If you stop fillling (because you halted), the ring buffer goes stale but never resets.
- **No feature-staleness circuit breaker.** If Muninn stops publishing, the strategy keeps quietly running on the last event. Recommend a "no event in N seconds" watchdog.
- **No per-instrument position limit** — only gross notional. A strategy that signals on multiple symbols will not be limited per symbol.
- **Risk evaluates the prospective fill but doesn't reserve cash.** Two concurrent strategy signals on the same instrument could both pass risk and overspend cash. Today, single-threaded dispatch hides this.
- **Mock-fill endpoint bypasses the strategy and risk-update gauges in a subtle way.** `/api/fills/mock` (`http.go:154`) calls `ApplyFill` but does not call `OnExecutionFill` — fills land in the portfolio but Prometheus `OrdersGeneratedTotal` never increments. Operator-only feature, but it makes metrics lie.

### Backtest vs. live divergence

- **Time semantics: OK.** Backtest dispatches directly to `exec.OnFeature(event)` with `event.EventTime`; fills inherit that time. Live mode also tags fills with `event.EventTime`. Consistent.
- **Slippage & fees parity: OK in paper mode.** Both paths call the same `simulateFill` (`executor.go:172–201`).
- **Risk parity: OK.** Same `riskManager.Evaluate(...)` runs in both paths.
- **Backtest does not run live mode.** `cmd/backtest/main.go:73` hardcodes `liveMode=false`. Fine, but worth documenting.
- **Daily equity sampling is fragile.** `engine.go:64` uses `EventTime.YearDay()` — wraps around year boundary; multi-year backtests will collapse Jan 1 of two different years into the same bucket.
- **No warmup vs. live divergence test.** A strategy run live for 3 days and a backtest of the same 3 days should agree on fills and PnL. There is no test that checks this.

### Production readiness gaps

- **Logging is structured (`slog` JSON) — good.** But spammy `slog.Info` in the inner loop (`portfolio.go:100`, `executor.go:125`, every strategy on every signal). At any real event rate this is unmanageable.
- **Metrics:** counters for features-consumed, orders-generated, fills-executed, plus four portfolio gauges. Missing: orders-rejected-by-risk (with reason label), feature-event-staleness histogram, end-to-end signal-to-fill latency histogram, strategy-feature-skipped counter, Kafka consumer lag.
- **OTel traces:** none.
- **Graceful shutdown:** present but not exercised in tests. `consumer.Run(ctx)` returns when context cancels; `srv.Stop` is best-effort.
- **Postgres migrations:** schema is inline `CREATE TABLE IF NOT EXISTS` (`postgres.go:51`). No versioning, no `goose`/`atlas`/`golang-migrate`.
- **Restart-from-journal correctness:** portfolio recovers, **strategies do not**, **risk peak does not**, **daily loss reset does not**. See gaps above.
- **No `internal/strategy/vpin_breakout.go`** — code colocated in `obi_threshold.go`, README implies separate file. Cosmetic; do it during cleanup.
- **No race detector in CI**, no coverage, no Go matrix (1.23 only), no lint (golangci-lint).

### Web UI assessment

- ~250 lines of TS, one `App.tsx`, no router, no test, no state-management library.
- Single SSE source-of-truth — good pattern.
- Hardcoded `http://localhost:8083`. Will not work behind a reverse proxy.
- Dep versions in `package.json` are not installable (TypeScript 6.x, Vite 8.x don't exist yet).
- Dockerfile builds via `npm ci` and serves with nginx. The nginx config is implicit (default), so SPA fallback routing doesn't work.
- It is a legitimate operator console (halt/resume, manual fill, live equity, fills tail) but absolutely not analytics-grade. It should be a phase of its own.

### Non-goals to make explicit

- **Not a feature-engineering library.** Features come from Muninn. Huginn never recomputes OBI/VPIN/VWAP from raw trades inside its hot path. (The `cmd/fetcher` is a one-off offline data preparation tool for backtests; it intentionally duplicates the formulas — that duplication is a known debt, not a feature.)
- **Not a multi-venue smart-order router.** All live execution goes through Sleipnir, which talks to a single venue. Huginn does not split orders across venues, manage inventory across venues, or arbitrage.
- **Not a portfolio-optimization library.** No mean-variance, no Black-Litterman, no factor model. Position sizing is per-strategy notional throttling, period.
- **Not a real exchange client.** Huginn never opens an exchange socket. Sleipnir does. Huginn's "live" mode means "live paper-or-real trading via Sleipnir."
- **Not a research notebook environment.** Analytics belong in muninn-py.
- **Not opinionated about strategy language.** Strategies are Go structs implementing the `Strategy` interface. No DSL, no Python embedding, no Lua hook — and there is no plan for one.

---

## Phase 0 — Unbreak `main` ✅

**Goal.** Get `go build ./...` and `npm ci && npm run build` green on a fresh clone.

**Deliverables.**
- Fix `cmd/huginn/main.go:146` to pass `exec` to `server.New`, or revert `server.New` to its 3-arg form. Pick one; the SSE handler doesn't currently use `executor`, so reverting is simpler and removes the unused field on `Server`.
- Pin `web/package.json` to versions that actually exist on npm (TypeScript 5.6.x, Vite 6.x, React 19.x as it stands).
- Add `go test -race ./...` and `go vet ./...` to CI, plus a `web` job that runs `npm ci && npm run build` and `npm run lint`.
- Make `docker-compose.yml` Sleipnir reference optional via a compose profile (`profiles: [live]`) so plain clones still come up.
- README correction pass: fix the strategy list, env-var table, and the "no exchange API connection" Non-Goal phrasing.

**Exit criteria.** Fresh clone: `go build ./... && go test -race ./... && (cd web && npm ci && npm run build)` all pass. CI is actually green, not just "passing because the workflow only checks `go vet`."

**Risks.** None — pure janitorial. If this phase is hard, the codebase has rot we haven't surfaced yet.

---

## Phase 1 — Restart correctness ✅

**Goal.** Huginn restarts the same way it left, end-to-end.

**Deliverables.**
- **Strategy state journal.** Add a small `Stateful` optional interface (`MarshalState() []byte` / `RestoreState([]byte) error`) on `Strategy`. Persist alongside portfolio in journal — JSONL gets a `{type: "strategy_state", ...}` record; Postgres gets a `strategy_state(strategy_name, state_blob, updated_at)` table. Implement for all four strategies.
- **Daily reset for risk.** Risk manager owns a UTC-day boundary. On crossing, reset realized-PnL-baseline for the daily-loss check (track `dayStartRealizedPnL`, compare `snap.RealizedPnL - dayStartRealizedPnL`).
- **Persist peak value.** Add `peak_value` to the journal so trailing drawdown survives restart.
- **Per-instrument position limit** in `RiskConfig`. Optional map `position_limit_per_instrument: { "BTC-USD": ..., "ETH-USD": ... }`. Falls back to gross.
- **Feature-staleness watchdog.** Risk manager auto-halts when `time.Since(lastFeatureEvent) > cfg.Risk.StalenessTimeout` (default 60 s); auto-resumes on next event if `auto_resume_after_staleness: true`.

**Exit criteria.** A new test (`risk/manager_recovery_test.go`) starts a Huginn, runs 10 fills crossing midnight, restarts the process, and asserts the daily-loss-limit window has rolled. Strategy state for OBI/VPIN/VWAP/EMA round-trips through both journal backends.

**Risks.** Strategy-state schema versioning. Use `{version: 1, ...}` from day one.

---

## Phase 2 — Strategy quality and calibration ✅

**Goal.** Every strategy has a documented failure mode, a calibration story, and dense unit tests.

**Deliverables.**
- **Per-strategy docstrings**: signal hypothesis, expected regime, known failure mode, parameter sensitivity. Add to each `*.go` file in `internal/strategy/`.
- **Extract `vpin_breakout.go`** into its own file. Cosmetic, but matches README.
- **Calibration CLI** (`cmd/calibrate`). Takes a JSONL feature file, a strategy name, and a parameter grid (e.g. `--threshold 0.5,0.6,0.7,0.8 --order-size 0.01`). Runs N backtests in parallel goroutines, emits CSV of `(params, sharpe, mdd, total_fills, realized_pnl, hit_rate)`. Persist to `data/calibration/<strategy>-<timestamp>.csv`.
- **Walk-forward backtest mode** for `cmd/backtest`: split data into train/test windows, calibrate on train, evaluate on test.
- **Hit-rate, turnover, average-hold-time** added to `internal/metrics/analysis.go`. Surface in backtest summary.
- **Property-based tests** (`testing/quick` or `gopter`) for each strategy: never emit an order with `Quantity <= 0`, never breach `maxPosition`, never reverse signs without crossing zero.
- **Document the concurrency contract** on the `Strategy` interface: "OnFeature is called from a single goroutine. Implementations need not be thread-safe unless `Stateful` is also implemented." Or, alternatively, require `Strategy` to be safe under a documented mutex — pick one and document.

**Exit criteria.** `cmd/calibrate --strategy obi --data data/historical.jsonl` produces a CSV. Each strategy file has a "Failure modes" docstring section. `go test ./internal/strategy/... -count=100` passes.

**Risks.** Calibration as a CLI invites hill-climbing on noise. Document explicitly that this is a *sanity sweep* — it is not a research tool. Real research happens in `muninn-py`.

---

## Phase 3 — Observability that doesn't lie ✅

**Goal.** A human looking at Grafana can tell whether Huginn is healthy without reading logs.

**Deliverables.**
- **New metrics.** `huginn_orders_rejected_total{reason=...}`, `huginn_feature_event_age_seconds` (histogram, computed `now - event.EventTime` at dispatch), `huginn_signal_to_fill_latency_seconds`, `huginn_kafka_consumer_lag{topic=...}`, `huginn_strategy_state_persisted_total`, `huginn_risk_halt_active` (gauge 0/1).
- **OpenTelemetry traces.** Span per `OnFeature` covering strategy → risk → executor → journal → portfolio. Optional OTLP exporter behind `OTEL_EXPORTER_OTLP_ENDPOINT`. _Deferred: OTel adds substantial dependency weight; the no-op path satisfies the risk note. Will be revisited in Phase 7 (release engineering) where a full dep audit is planned._
- **Log volume control.** Demote inner-loop `slog.Info` in `portfolio.ApplyFill`, `executor.OnFeature`, and strategy "Strategy signal" lines to `slog.Debug`; gate `Info`-level on a sampling counter or every-Nth-event flag.
- **A bundled Grafana dashboard JSON** in `deploy/grafana/huginn.json`: equity curve, realized vs unrealized PnL, fills/min, orders rejected by reason, feature age, halt status.
- **`/api/snapshot` endpoint** returning the current snapshot as plain JSON (the SSE stream's per-tick payload sans streaming) — needed for the dashboard and easier to scrape than SSE.

**Exit criteria.** `curl :8081/metrics` exposes the new metrics. The Grafana dashboard JSON loads against a default Prometheus scrape and shows non-empty panels in a backtest replay.

**Risks.** OTel pulls in significant deps. Make the exporter optional (no-op tracer if env unset).

---

## Phase 4 — Backtest fidelity ✅
**Goal.** Backtest results predict live paper-trading results within a documented tolerance.

**Deliverables.**
- ✅ **Fix the year-boundary bug** in `engine.go:64`: encode the daily key as `year*1000 + YearDay()` so Jan 1 of different years produces distinct equity samples. Regression tests in `internal/backtest/engine_test.go`.
- ✅ **Order-book-aware fill model.** Buys fill at `askPrice * (1 + slippage_bps)` and sells fill at `bidPrice * (1 - slippage_bps)` when the feature event carries `bidPrice`/`askPrice` (e.g. from `features.book.v1`). Falls back to `microPrice`/`value` when absent.
- ✅ **Latency model.** Optional `executor.fill_latency_ms` config (`EXECUTOR_FILL_LATENCY_MS` env var) that defers the fill timestamp; in backtest this changes which subsequent event triggers PnL marking. Zero (default) preserves the original behaviour.
- ✅ **Parity test.** A new `parity_test.go` runs the same 1000-event JSONL through (a) backtest engine, (b) executor driven by an in-memory channel mimicking the consumer. Asserts identical fill counts, identical realized PnL to 6 decimals.
- ✅ **Backtest report HTML.** Optional `--report report.html` flag emits a self-contained HTML with equity curve, drawdown, fills table, parameter echo. Useful for sharing on PRs.
- ✅ **Multi-strategy backtest.** `Engine.AddExecutor` registers additional strategy executors that receive every event alongside the primary one. All executors share the same portfolio and risk manager. `TestMultiStrategySharedPortfolio` verifies fill counts accumulate from both strategies.

**Exit criteria.** `parity_test.go` passes. The known year-boundary bug is regression-tested. A 1-week historical replay produces a report HTML in under 10 s.

**Risks.** Touch-fill model needs feature topics that may not yet be live in Muninn. Make it an opt-in.

---

## Phase 5 — Postgres-grade persistence ✅
**Goal.** Postgres mode is the recommended default, not a side path.

**Deliverables.**
- ✅ **Migrations.** No external dependency. `internal/journal/pg_migrations.go` implements a versioned migration ledger (`schema_migrations` table). Each version runs in its own transaction; a failure rolls back and the process exits loudly. Append-only — never edit existing entries.
- ✅ **Schema additions.** Migration v2 adds `trade_orders` (intent records, pre-fill, for intent→fill join) and `daily_pnl_snapshots` (one row per UTC day, upserted by the state-persister ticker).
- ✅ **Connection-pool tunables** in `DatabaseConfig`: `max_conns`, `min_conns`, `max_conn_lifetime`, `max_conn_idle_time` (env vars `DATABASE_MAX_CONNS`, `DATABASE_MIN_CONNS`, `DATABASE_MAX_CONN_LIFETIME`, `DATABASE_MAX_CONN_IDLE_TIME`). Passed to `NewPostgresWriter` via new `journal.PoolConfig` struct.
- ✅ **Postgres-backed risk recovery.** `PostgresWriter.AppendDailyPnLSnapshot` upserts today's `peakValue` and `dayStartRealizedPnL`. `LoadLatestDailyBaseline` reads it back. Boot path falls back to `SeedFromBaseline` when `strategy_state._risk` is absent. Tested in `TestRecoveryFallback_DrawdownGuard`.
- ✅ **Make Postgres the default** in `configs/default.yaml`. JSONL mode preserved for ephemeral demos (`DATABASE_ENABLED=false`).
- ✅ **Backup/restore runbook** in `docs/OPERATIONS.md`.

**Exit criteria.** `make db-migrate-up` and `make db-migrate-down` work. A `huginn` process started against a freshly migrated DB, then restarted, recovers portfolio + peak + daily baseline bit-for-bit.

**Risks.** Backwards compatibility with existing `trade_fills` tables. The first migration should be idempotent against the current schema.

---

## Phase 6 — Hardened Web UI
**Goal.** The dashboard is a real operator console: reload-safe, deployable behind a reverse proxy, with strategy management.

**Deliverables.**
- ✅ **Strategy control panel.** `GET/PUT /api/strategy/config` wired up — GET returns `executor.SystemConfig` (strategy name, threshold, order size, fast/slow periods, position limit); PUT calls `executor.UpdateConfig` and is auth-guarded.
- **Connection config from runtime, not literal.** Read `import.meta.env.VITE_API_BASE` so the prod build can be repointed without a rebuild.
- ✅ **Equity-curve persistence.** `/api/snapshot/history` returns the last N equity samples from a 720-point in-memory ring buffer (default ~6 h at 30 s sampling). Ring populated by `srv.RunEquitySampler`.
- **Strategy panel showing which strategy is active**, current threshold, current position, current PnL — values available via `GET /api/strategy/config`.
- ✅ **Auth.** `HUGINN_API_TOKEN` env var gates `/api/breaker/*`, `/api/fills/mock`, and `PUT /api/strategy/config`. Empty token disables auth (backward-compatible). CORS updated to allow `PUT` and `Authorization` header. Documented in `docs/OPERATIONS.md`.
- **Production nginx config** with `try_files $uri /index.html;` and CORS handled at the proxy layer, not in the Go server.
- **One Playwright smoke test**: load the page against a stub Huginn, assert equity panel renders.

**Exit criteria.** `docker-compose up` produces a UI that, behind a reverse proxy at `/`, shows live state, can halt+resume, and can update strategy threshold without restarting Huginn.

**Risks.** UI scope creep. Cap it at operator-grade — analytics live in `muninn-py`.

---

## Phase 7 — Release engineering
**Goal.** Huginn is installable, reproducible, documented, and clearly versioned.

**Deliverables.**
- **`docker buildx` multi-arch images** (`linux/amd64`, `linux/arm64`) published to GHCR on tag. Both `huginn` and `huginn-dashboard` images.
- **Versioning.** `internal/version` package with `Version`, `GitSHA`, `BuildTime`; expose at `/version` and in startup log.
- **`CONTRIBUTING.md`, `SECURITY.md`, `CODE_OF_CONDUCT.md`** matching the discipline of the Muninn server repo.
- **MkDocs site** under `docs/` covering: architecture, strategy authoring guide, risk model, ops runbook, calibration workflow, the four bundled strategies' failure modes.
- **Cross-link from Muninn's `companion-services` section** once published.
- **Lint/format in CI.** `golangci-lint` config with the same rule set the Muninn server uses.

**Exit criteria.** `docker pull ghcr.io/lgreene03/huginn:v0.1.0` works. The docs site is published on GitHub Pages and linked from the README.

**Risks.** Doc maintenance — keep it co-located with the code that needs it.

---

## Phase F — Future _(deferred / speculative)_

Tracked so ideas aren't lost; explicitly not scheduled.

- **Live trading mode for real.** The plumbing to Sleipnir exists. Moving from testnet to mainnet requires per-instrument kill-switches, two-person operational consent, real-money risk limits, and a dedicated incident response runbook. Not before Phases 1–3 are bulletproof in paper.
- **Strategy hot-reload from disk** (drop a `.so` plugin). Significant complexity, very little payoff over restarts.
- **Cross-strategy meta-allocator** that splits capital across the four strategies based on rolling Sharpe. Adjacent to portfolio optimization — explicit non-goal today, revisit if a real driver appears.
- **Replay-divergence diagnostics** — given a fills journal and the original features, deterministically replay and surface any divergence. Useful only once Phase 4 parity is rock-solid.
- **WebSocket consumer for muninn streaming features** when the server adds one. Not before.
- **Multi-venue support.** Requires a Sleipnir per venue; out of scope.

---

## Non-goals (explicit)

In the same spirit as Muninn's [NON_GOALS.md](https://github.com/lgreene03/muninn/blob/main/docs/steering/NON_GOALS.md):

- **Not a feature-engineering library.** Features come from Muninn. The `cmd/fetcher` is an offline data-prep tool for backtests, not a runtime computation path.
- **Not a multi-venue smart-order router.** One Sleipnir, one venue.
- **Not a portfolio-optimization library.** Position sizing is per-strategy throttling.
- **Not a real exchange client.** Huginn talks to Sleipnir over Kafka; Sleipnir talks to the venue.
- **Not a research notebook environment.** Analytics belong in `muninn-py`.
- **Not opinionated about strategy language.** Strategies are Go structs.
- **Not a UI framework or analytics dashboard.** The bundled `web/` is an operator console — halt, resume, tune, observe. Not a research surface.

---

## Phase ordering rationale

- **Phase 0 before everything**, because a roadmap on top of a red `main` is fiction.
- **Phase 1 before Phase 2**, because a calibration workflow that produces good numbers on a process that doesn't survive a restart is worse than no calibration workflow at all.
- **Phase 2 before Phase 3**, because metrics that lie about a strategy that fails silently is the worst combination — fix the strategies first, then make the observability honest.
- **Phase 3 before Phase 4**, because backtest fidelity is judged by observed-vs-replayed divergence — you need the observability to measure the divergence.
- **Phase 4 before Phase 5**, because Postgres-grade persistence on a system whose backtest doesn't match live execution just persists wrong numbers more durably.
- **Phase 5 before Phase 6**, because the UI promises in Phase 6 (history, equity persistence, strategy tuning audit) all depend on Postgres being the system of record.
- **Phase 7 last** — release a product, not a sketch.
- **Phase F never on its own schedule** — items move out only when a real driver appears.
