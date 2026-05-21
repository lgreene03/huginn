# Huginn — Operations Runbook

Operational procedures for a running Huginn instance. For architecture, see `README.md`. For deployment, see `docker-compose.yml`.

---

## Database

### Schema migrations

Huginn uses an internal versioned migration system (`internal/journal/pg_migrations.go`). Migrations run automatically on boot when `DATABASE_ENABLED=true`. There is no separate migrate step — the process self-migrates.

**To verify which migrations have been applied:**

```sql
SELECT version, applied_at FROM schema_migrations ORDER BY version;
```

**Adding a migration:** append a new entry to `pgMigrations` in `internal/journal/pg_migrations.go`. Never edit or remove existing entries.

### Backup and restore

**Backup (pg_dump):**

```bash
# Full logical backup
pg_dump -h localhost -U postgres huginn > huginn_$(date +%Y%m%d_%H%M%S).sql

# Fills only (smallest backup for recovery purposes)
pg_dump -h localhost -U postgres -t trade_fills huginn > fills_$(date +%Y%m%d_%H%M%S).sql

# Strategy state only
pg_dump -h localhost -U postgres -t strategy_state huginn > state_$(date +%Y%m%d_%H%M%S).sql
```

**Restore:**

```bash
# Stop huginn before restoring
docker-compose stop huginn

# Restore into a fresh database
createdb -h localhost -U postgres huginn_restore
psql -h localhost -U postgres huginn_restore < huginn_20260101_120000.sql

# Start huginn against the restored database
DATABASE_URL="postgres://postgres:postgres@localhost:5432/huginn_restore?sslmode=disable" \
  docker-compose up -d huginn
```

After restore, huginn will re-run any missing migrations before accepting traffic.

### Connection pool tuning

```yaml
# configs/default.yaml
database:
  max_conns: 10          # upper bound on pooled connections
  min_conns: 2           # warm connections kept alive during idle
  max_conn_lifetime: 1h  # rotate connections before they hit server-side timeout
  max_conn_idle_time: 5m # evict idle connections faster than max_conn_lifetime
```

Or via environment variables: `DATABASE_MAX_CONNS`, `DATABASE_MIN_CONNS`, `DATABASE_MAX_CONN_LIFETIME`, `DATABASE_MAX_CONN_IDLE_TIME`.

---

## Risk manager

### Manual halt

```bash
curl -X POST http://localhost:8081/api/breaker/trigger
```

Huginn rejects all new strategy signals immediately. The halt is in-memory and resets on process restart.

### Resume

```bash
curl -X POST http://localhost:8081/api/breaker/reset
```

### Check halt status

```bash
curl http://localhost:8081/api/snapshot | jq .halted
```

---

## Monitoring

### Key metrics to watch

| Metric | Alert threshold | What it means |
|--------|-----------------|---------------|
| `huginn_risk_halt_active` | > 0 for > 5 min | Risk circuit breaker is open |
| `huginn_feature_event_age_seconds` p99 | > 30 s | Muninn pipeline is stale |
| `huginn_kafka_consumer_lag{topic=features}` | > 500 | Consumer is falling behind |
| `huginn_orders_rejected_total{reason=drawdown}` | rate > 0 | Drawdown limit reached |
| `huginn_portfolio_total_value` | < initial_cash × 0.80 | 20 % drawdown — review |

### Grafana dashboard

The bundled dashboard is provisioned at `deploy/grafana/huginn.json`. Import it into any Grafana instance pointed at the Prometheus scrape endpoint (`/metrics` on port `8081`).

---

## Strategy live-tuning

Read current parameters:

```bash
curl http://localhost:8081/api/strategy/config
```

Update threshold without restart:

```bash
curl -X PUT http://localhost:8081/api/strategy/config \
  -H 'Content-Type: application/json' \
  -d '{"threshold": 0.75, "order_size": 0.001}'
```

Changes take effect on the next feature event and are lost on restart. To make permanent, update `configs/default.yaml`.

---

## Log levels

Huginn defaults to `INFO` level. Set `HUGINN_LOG_LEVEL=DEBUG` for inner-loop signal and fill lines (high volume — only for short-duration debugging).

---

## Graceful restart

```bash
# Send SIGTERM — huginn drains in-flight events and persists strategy state
docker-compose restart huginn
```

The process logs `"Huginn shutting down gracefully"` and then `"Strategy state persisted on shutdown"` before exiting. If these lines do not appear, the process was killed (SIGKILL) and the last ~5 s of state may be replayed on next boot.
