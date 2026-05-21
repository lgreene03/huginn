# Risk Model

Huginn's risk manager (`internal/risk/manager.go`) applies four pre-trade controls before every fill, plus two circuit-breaker modes. All checks run synchronously in the hot path — rejection is instant.

---

## Controls

### 1. Peak-trailing drawdown

Tracks the running peak of total portfolio value (realized + unrealized PnL + cash). Rejects any order when:

```
(peak_value - total_value) / peak_value > max_drawdown_pct
```

The peak ratchets upward but never downward. Once the halt threshold is reached, the engine halts with `HaltReason = "drawdown"`. It does **not** auto-resume — a manual `POST /api/breaker/reset` is required.

**Survival across restarts.** `peakValue` is persisted in the `_risk` key of the `strategy_state` journal (Postgres or JSONL). On boot, it is restored before any fills are processed. If the `_risk` key is absent (e.g. first boot after migration), `daily_pnl_snapshots` provides a coarser baseline via `LoadLatestDailyBaseline`.

**Config.**

| Key | Env | Default | Description |
|---|---|---|---|
| `risk.max_drawdown_pct` | `RISK_MAX_DRAWDOWN_PCT` | 0.20 | Fraction of peak; 0.20 = 20% drawdown halt |

---

### 2. Daily loss limit

Compares the intraday realized PnL against a fixed limit. Rejects any order when:

```
(snap.RealizedPnL - day_start_realized_pnl) < -daily_loss_limit
```

The daily window rolls at UTC midnight. `dayStartRealizedPnL` is reset to the current `RealizedPnL` on each day boundary. This prevents loss accumulation across calendar days.

**Survival across restarts.** `dayStartRealizedPnL` is persisted alongside `peakValue` in the `_risk` journal key. On first boot or after `_risk` is absent, the `daily_pnl_snapshots` fallback provides the most recent UTC-day baseline.

**Config.**

| Key | Env | Default | Description |
|---|---|---|---|
| `risk.daily_loss_limit` | `RISK_DAILY_LOSS_LIMIT` | 0 (disabled) | Maximum allowed intraday loss |

A `DailyLossLimit` of 0 disables the check.

---

### 3. Gross position limit

Rejects any order that would cause the gross notional position to exceed the configured limit:

```
abs(position_after_order) > position_limit_hard
```

Evaluated as the potential position after the prospective order is filled.

**Config.**

| Key | Env | Default | Description |
|---|---|---|---|
| `risk.position_limit_hard` | `RISK_POSITION_LIMIT_HARD` | 0 (disabled) | Hard gross notional cap |

A `PositionLimitHard` of 0 disables the check.

---

### 4. Volatility-scaled position limit

Computes the coefficient of variation (CV) of the last 30 fill prices and scales the hard limit down when volatility is elevated:

```
cv = std(last_30_fill_prices) / mean(last_30_fill_prices)
effective_limit = position_limit_hard / max(1, cv * volatility_scale_factor)
```

This automatically tightens position limits during high-volatility regimes without manual intervention.

**Config.** `volatility_scale_factor` is embedded in `RiskConfig`. Default: 1.0 (no scaling until CV exceeds 1.0).

---

## Circuit breakers

### Manual halt/resume

Any authorized caller can halt the engine immediately:

```bash
# Halt
curl -X POST http://localhost:8081/api/breaker/trigger \
  -H "Authorization: Bearer $HUGINN_API_TOKEN"

# Resume
curl -X POST http://localhost:8081/api/breaker/reset \
  -H "Authorization: Bearer $HUGINN_API_TOKEN"
```

When halted, all new `OnFeature` calls return without emitting orders. Fills that are already in-flight from Sleipnir are still applied to the portfolio.

`haltReason` is set to `"manual"` and is visible in `/healthz` and `/metrics`.

### Feature-staleness watchdog

The risk manager tracks `lastFeatureEventTime` (from the event's `EventTime` field, not wall-clock). If no feature event arrives within `risk.staleness_timeout`, the engine auto-halts with `HaltReason = "feature_staleness"`.

If `risk.auto_resume_after_staleness` is `true`, the engine automatically resumes when the next feature event arrives.

**Config.**

| Key | Env | Default | Description |
|---|---|---|---|
| `risk.staleness_timeout` | `RISK_STALENESS_TIMEOUT` | 60s | Auto-halt if no event within this duration |
| `risk.auto_resume_after_staleness` | `RISK_AUTO_RESUME_AFTER_STALENESS` | false | Auto-resume on next event after staleness halt |

---

## Rejection reasons

The `huginn_orders_rejected_total` Prometheus counter has a `reason` label:

| Reason | Trigger |
|---|---|
| `drawdown` | Peak-trailing drawdown exceeded |
| `daily_loss` | Intraday realized PnL exceeded `DailyLossLimit` |
| `position_limit` | Gross position (or volatility-scaled limit) exceeded |
| `halted` | Manual circuit breaker or staleness watchdog |

---

## State persistence

The risk manager's durable state is persisted by the executor's `PersistStrategyState` ticker (every 30 s by default) and on clean shutdown.

**Postgres** (recommended): persisted in the `strategy_state` table under key `_risk` as a JSON blob:

```json
{
  "peak_value": 101234.56,
  "day_start_realized_pnl": 800.0,
  "last_feature_event_time": "2026-05-21T14:30:00Z",
  "halt_reason": ""
}
```

**JSONL** (fallback): appended as a `{"type":"strategy_state","key":"_risk",...}` record. On replay, the latest `_risk` record wins.

**daily_pnl_snapshots** (coarse fallback): one row per UTC day. Used when `_risk` is absent (e.g. first boot after a new deployment). Provides `peakValue` and `dayStartRealizedPnL` with one-day granularity.

---

## Dynamic updates

Risk limits can be updated without restart via `PUT /api/strategy/config`:

```bash
curl -X PUT http://localhost:8081/api/strategy/config \
  -H "Authorization: Bearer $HUGINN_API_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"max_drawdown_pct": 0.15, "daily_loss_limit": 500}'
```

Updates are applied atomically via `riskManager.UpdateLimits`. Existing halt state is not affected — a halted engine stays halted after a limit update.

---

## Operational notes

- **Never set `daily_loss_limit` below the worst single-session loss in calibration** — it will trigger on the first active day.
- **`max_drawdown_pct` should be at least 2× the maximum drawdown observed in calibration** to avoid spurious halts during normal variance.
- **After a drawdown halt**, review the fills log and portfolio before resuming. The halt does not reverse fills that already landed.
- **Staleness watchdog** requires `risk.staleness_timeout` to be longer than Muninn's typical inter-event gap at quiet hours. A 60 s default is safe for BTC-USDT; lower-liquidity instruments may need 120 s or more.
