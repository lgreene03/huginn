# Huginn — Quantitative Strategy Execution Engine

> *Named after Odin's raven of "thought."*
> *Huginn* consumes deterministic features from **Muninn** ("memory") and executes paper-trading strategies.

## Architecture

```mermaid
graph TD
    M[Muninn Feature Engine] -->|Redpanda Topics| K[Kafka Consumer]
    K --> E[Executor]
    E <--> S[Strategy Interface]
    E --> R[Risk Manager]
    E --> P[Portfolio Tracker]
    E --> J[Trade Journal]
```

Huginn is a **downstream companion** to [Muninn](https://github.com/lgreene/muninn). It strictly adheres to Muninn's architectural principle: *Muninn observes and computes; Huginn thinks and acts.*

| Layer | Responsibility |
|---|---|
| **Kafka Consumer** | Multi-topic fan-in consumer that subscribes to Muninn's Redpanda topics |
| **Strategy** | Pluggable interface (`OnFeature → []Order`) implementing quantitative signal logic |
| **Risk Manager** | Pre-trade risk controls (Max Drawdown, Daily Loss Limit, Position Limits) |
| **Executor** | Simulates order fills with configurable slippage and transaction costs |
| **Portfolio** | Thread-safe position tracker with realized/unrealized PnL accounting |
| **Trade Journal** | Append-only JSONL persistent storage for crash recovery |

## Strategies

### OBI Threshold (Mean-Reversion)
Monitors Order Book Imbalance. When extreme buy pressure is detected (OBI > threshold), it sells expecting reversion. Vice versa for extreme sell pressure.

### VPIN Breakout (Momentum)
Monitors Volume-Synchronized Probability of Informed Trading. When VPIN exceeds the threshold, it enters in the direction of informed flow with a configurable cooldown.

## Docker Quick Start

The easiest way to run Huginn is via Docker Compose, which spins up the engine alongside a local Redpanda broker:

```bash
# Start Huginn and Redpanda
docker-compose up -d

# Check Huginn logs
docker-compose logs -f huginn

# Verify health status and portfolio snapshot
curl http://localhost:8081/healthz
```

## Configuration

Huginn is configured via YAML profiles (e.g., `configs/default.yaml`). You can override any value using the corresponding environment variable.

| YAML Key | Environment Variable | Description |
|---|---|---|
| `kafka.brokers` | `KAFKA_BROKERS` | List of Redpanda/Kafka brokers |
| `kafka.topics` | `KAFKA_TOPICS` | List of feature topics to consume |
| `kafka.group_id` | `KAFKA_GROUP_ID` | Kafka consumer group ID |
| `strategy.name` | `STRATEGY_NAME` | Strategy to run (`obi`, `vpin`) |
| `strategy.threshold` | `STRATEGY_THRESHOLD` | Signal activation threshold |
| `strategy.order_size` | `STRATEGY_ORDER_SIZE` | Order size per signal |
| `executor.transaction_cost_bps` | `EXECUTOR_TX_COST_BPS` | Simulated transaction cost (basis points) |
| `executor.slippage_bps` | `EXECUTOR_SLIPPAGE_BPS` | Simulated slippage (basis points) |
| `capital.initial_cash` | `CAPITAL_INITIAL_CASH` | Initial capital (USDT) |
| `risk.max_drawdown_pct` | `RISK_MAX_DRAWDOWN_PCT` | Maximum drawdown percentage (e.g. 0.20 for 20%) |
| `risk.daily_loss_limit` | `RISK_DAILY_LOSS_LIMIT` | Maximum daily loss allowed |
| `risk.position_limit_hard` | `RISK_POSITION_LIMIT_HARD` | Hard position limit (gross notional) |
| `server.port` | `SERVER_PORT` | Port for observability server (default `8081`) |

You can specify a config file via the CLI:
```bash
./huginn --config configs/aggressive.yaml
```

## Testing

```bash
go test ./...
go vet ./...
```

## Non-Goals

Huginn is a **paper-trading simulator**, not a live execution engine:
- No real exchange API connections
- No real order routing
- No wallet or custody management
- No financial advice

This is a portfolio demonstration project showcasing quantitative strategy engineering.
