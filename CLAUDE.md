# CLAUDE.md

## What Is Huginn

Huginn is a quantitative strategy execution engine — the "thought" raven in the Norse stack. It consumes deterministic feature events from Muninn, runs pluggable trading strategies, and either paper-fills locally or publishes order intents to Sleipnir for live execution.

## Commands

```bash
# Run tests
go test ./...

# Run tests with race detector
go test -race ./...

# Lint
golangci-lint run

# Build
go build -o huginn ./cmd/huginn

# Run with default config
./huginn --config configs/default.yaml

# Run via Docker Compose (Huginn + Redpanda + Postgres)
docker compose up -d

# End-to-end smoke test
bash scripts/smoke.sh

# Backtest with HTML report (data/btc_test.jsonl is the committed fixture)
go run ./cmd/backtest --data data/btc_test.jsonl --report report.html
```

## Architecture

```
Redpanda (features.obi.v1) → Kafka Consumer → Executor
                                                  ├── Strategy (OnFeature → []Order)
                                                  ├── Risk Manager (drawdown, daily loss, position limits)
                                                  ├── Portfolio (FIFO avg-cost, PnL tracking)
                                                  └── Journal (Postgres or JSONL)
```

**Paper mode** (default): Executor simulates fills with configurable slippage/fees.
**Live mode** (`LIVE_EXECUTION=true`): Publishes intents to `executions.intents.v1`, consumes fills from `executions.fills.v1` via Sleipnir.

## Key Packages

- `cmd/huginn/` — Main entry point. Loads config, wires everything.
- `cmd/backtest/` — Offline JSONL replay with Sharpe/MDD report.
- `cmd/calibrate/` — Grid-search over strategy parameters.
- `cmd/walkforward/` — Anchored walk-forward validation (expanding train window, sliding test window).
- `cmd/research/` — Research/validation HTTP gateway (port `8094`, `POST`/`GET /api/research/runs`). Runs the same `internal/research` engine as `cmd/walkforward` but OUT of the live trading process: a standalone sidecar that replays a JSONL dataset (no Kafka/Postgres), executes walk-forward + PBO + Deflated-Sharpe async, and persists finished runs to `RESEARCH_RESULTS_DIR`. See the "Research gateway" section in README.md.
- `internal/strategy/` — Six strategies: `obi_threshold.go`, `vpin_breakout.go`, `vwap_deviation.go`, `ema_crossover.go`, `ou_reversion.go` (OU mean-reversion), `composite.go` (pluggable-alpha blend). OBI strategy includes regime-aware threshold adaptation. The pluggable alpha framework — `alpha.go` (interface), `alphas_bundled.go` (worked alphas), `composite.go` (weighted blend) — lets a new signal ship as one `Alpha` type plus one line of config; see `docs/ADDING_AN_ALPHA.md`.
- `internal/executor/` — Dual-mode executor (paper/live). Owns the OnFeature dispatch loop. Tracks signal-to-decision latency via Prometheus histogram.
- `internal/risk/` — Pre-trade risk: drawdown, daily loss, position limits, staleness watchdog.
- `internal/portfolio/` — Thread-safe position tracker with realized/unrealized PnL.
- `internal/journal/` — Pluggable persistence: JSONL writer, Postgres with migrations, NullWriter (for walk-forward isolation).
- `internal/kafka/` — Consumer (multi-topic fan-in), intent producer, fills consumer, price tick consumer (sub-second exit monitoring).
- `internal/server/` — HTTP: `/healthz`, `/readyz`, `/metrics`, `/api/snapshot`, `/api/stream` (SSE), `/api/breaker/*`. gRPC: `huginn.HuginnService/GetSnapshot` on port 50051 with reflection.
- `internal/config/` — YAML + envconfig. All fields have `envconfig` tags for env-var override.
- `web/` — React operator dashboard (equity curve, positions, fills, halt/resume).

## Configuration

YAML profiles live in `configs/`. Every YAML key has a corresponding env var (via `envconfig`):

- `KAFKA_BROKERS`, `KAFKA_TOPICS`, `KAFKA_GROUP_ID`
- `STRATEGY_NAME` (`obi`, `vpin`, `vwap_deviation`, `ema_crossover`, `ou`, `composite`)
- `STRATEGY_THRESHOLD`, `STRATEGY_ORDER_SIZE`
- `LIVE_EXECUTION` — publish intents to Sleipnir instead of paper-filling
- `KAFKA_PRICE_TOPIC` — enable real-time price feed for sub-second exit monitoring
- `GRPC_PORT` — enable gRPC server (default unset = disabled)
- `DATABASE_ENABLED`, `DATABASE_URL` — Postgres journal (recommended default)
- `SERVER_PORT` (default `8081`)

## Norse Stack Context

```
Muninn (features) → Huginn (strategy) → Sleipnir (execution) → Fill → Huginn (portfolio)
```

- Huginn consumes: `features.obi.v1` (or any `features.*` topic)
- Huginn consumes: `prices.realtime.v1` (sub-second price ticks for exit monitoring)
- Huginn produces: `executions.intents.v1` (when `LIVE_EXECUTION=true`)
- Huginn consumes: `executions.fills.v1` (fills back from Sleipnir)
- Huginn exposes: gRPC `huginn.HuginnService/GetSnapshot` on port 50051

## Testing

- Unit tests: `go test ./...` (no Docker needed)
- Smoke test: `bash scripts/smoke.sh` (boots Docker, pushes synthetic OBI event, verifies strategy fires)
- Cross-stack: `bash ../muninn/scripts/smoke-stack.sh` (full Norse pipeline)
