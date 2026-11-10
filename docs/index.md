# Huginn

> *Named after Odin's raven of "thought."*

Huginn is a **quantitative strategy execution engine** that consumes deterministic features from [Muninn](https://github.com/lgreene03/muninn) and executes paper-trading strategies with full crash-recovery, risk controls, and live operator tooling.

---

## Why Huginn exists

[Muninn](https://github.com/lgreene03/muninn) solves the *computation* problem: one deterministic pipeline that works identically live and in replay. Huginn solves the *decision* problem: what to do with those features.

The split is deliberate. Huginn never recomputes OBI, VPIN, or VWAP from raw trades — that is Muninn's job. Huginn treats every feature event as an immutable fact about the market and applies a strategy to it.

---

## What you can do with it

| Goal | How |
|---|---|
| Run a bundled strategy on live features | `configs/default.yaml` + `docker-compose up` |
| Add a new signal (pluggable alpha) | [docs/ADDING_AN_ALPHA.md](ADDING_AN_ALPHA.md) |
| Backtest a strategy on historical features | `cmd/backtest --strategy obi --data data/historical.jsonl` |
| Calibrate strategy parameters | `cmd/calibrate --strategy obi --data data/historical.jsonl --threshold 0.5,0.6,0.7` |
| Watch equity, PnL, and fills live | `GET /api/snapshot`, the `GET /api/stream` SSE feed, or the console in `norse-stack/console` |
| Tune parameters without restarting | `PUT /api/strategy/config` (auth-gated) |
| Halt/resume the engine | `POST /api/breaker/trigger` and `/api/breaker/reset` (auth-gated), or the `norse-stack` console's halt control |

---

## Quick start

```bash
# Clone and bring up the full stack (Huginn + Redpanda + Postgres)
git clone https://github.com/lgreene03/huginn.git
cd huginn
docker-compose up -d

# Verify the engine is healthy
curl http://localhost:8081/healthz

# Tail logs
docker-compose logs -f huginn
```

Build and run directly:

```bash
go build -o huginn ./cmd/huginn
./huginn --config configs/default.yaml
```

---

## Companion services

| Service | Role |
|---|---|
| [Muninn](https://github.com/lgreene03/muninn) | Feature engine — computes OBI, VPIN, VWAP from Binance feeds |
| [Sleipnir](https://github.com/lgreene03/sleipnir) | Order gateway — bridges Huginn's order intents to the venue |
| [muninn-py](https://github.com/lgreene03/muninn-py) | Python research SDK — analytics and notebooks |

Huginn occupies the middle tier: it receives features from Muninn over Redpanda and optionally sends order intents to Sleipnir over Kafka. In paper mode (default), fills are simulated locally.

---

## Current status

**Phase 7 complete.** Phases 0–7 are fully delivered:

- Strategies: OBI Threshold, VPIN Breakout, EMA Crossover, VWAP Deviation
- Risk controls: drawdown, daily loss, position limits, staleness watchdog
- Persistence: Postgres-backed journal with versioned migrations and crash recovery
- Backtest engine: parity-tested against live executor, HTML report output
- Calibration CLI: parameter grid sweep with CSV output
- Observability: Prometheus metrics, Grafana dashboard JSON, SSE equity stream
- Operator console: none bundled; the bundled web app was retired on 2026-11-10 and the console lives in `norse-stack/console`
- Release engineering: multi-arch Docker images, versioned builds, lint CI

See [Roadmap](ROADMAP.md) for phase details and [Architecture](architecture.md) for internals.
