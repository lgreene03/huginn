# Calibration Workflow

Huginn's calibration CLI (`cmd/calibrate`) sweeps a parameter grid across a historical JSONL feature file and emits a CSV of per-parameter-set performance metrics. It is a **sanity sweep**, not a research tool. Real alpha research belongs in [muninn-py](https://github.com/lgreene03/muninn-py).

---

## Quick start

```bash
# 1. Fetch historical features (BTC-USDT, last 7 days)
./bin/fetcher --symbol BTCUSDT --days 7 --out data/historical.jsonl

# 2. Run a grid sweep for OBI Threshold
./bin/calibrate \
  --strategy obi \
  --data data/historical.jsonl \
  --threshold 0.5,0.6,0.7,0.8 \
  --order-size 0.01,0.05 \
  --out data/calibration/obi-$(date +%Y%m%d).csv

# 3. Inspect the results
column -t -s, data/calibration/obi-20260521.csv | head -20
```

---

## Output format

The CSV has one row per parameter combination:

```
strategy,threshold,order_size,fast_period,slow_period,cooldown_ms,sharpe,mdd,total_fills,realized_pnl,hit_rate,turnover,avg_hold_ms
obi,0.5,0.01,,,, 0.42, 0.18, 312, 182.4, 0.51, 6.2, 4200
obi,0.6,0.01,,,, 0.71, 0.12, 214, 241.8, 0.53, 4.3, 5100
...
```

| Column | Description |
|---|---|
| `sharpe` | Annualized Sharpe ratio (252 trading days) |
| `mdd` | Maximum drawdown as fraction of peak equity |
| `total_fills` | Total fill count across the run |
| `realized_pnl` | Cumulative realized PnL in USDT |
| `hit_rate` | Fraction of trades that closed with positive PnL |
| `turnover` | Average daily gross turnover as multiple of initial capital |
| `avg_hold_ms` | Average time between entry and next opposing fill (ms) |

Parameters not applicable to a strategy are left blank.

---

## Strategy-specific parameters

### `obi`
```bash
--threshold 0.5,0.6,0.7,0.8       # OBI signal threshold
--order-size 0.01,0.05,0.1         # Order quantity
```

### `vpin`
```bash
--threshold 0.4,0.5,0.6,0.7       # VPIN signal threshold
--order-size 0.01,0.05             # Order quantity
--cooldown-ms 1000,5000,15000      # Re-entry cooldown in ms
```

### `ema_crossover`
```bash
--fast-period 8,12,20              # Fast EMA period
--slow-period 21,26,50             # Slow EMA period
--order-size 0.01,0.05             # Order quantity
```

### `vwap_deviation`
```bash
--threshold 0.001,0.002,0.005      # Deviation threshold (fraction, e.g. 0.001 = 0.1%)
--order-size 0.01,0.05             # Order quantity
```

---

## Walk-forward mode

To avoid overfitting a threshold on the entire dataset:

```bash
./bin/calibrate \
  --strategy obi \
  --data data/historical.jsonl \
  --threshold 0.5,0.6,0.7,0.8 \
  --walk-forward \
  --train-pct 0.7 \
  --out data/calibration/obi-wf-$(date +%Y%m%d).csv
```

`--walk-forward` splits the data into train (70%) and test (30%) windows, calibrates on train, and evaluates on test. The CSV reports both `train_sharpe` and `test_sharpe`. Choose the parameter set with the best `test_sharpe` — not `train_sharpe`.

---

## Parallel execution

The calibrate CLI runs each parameter combination in its own goroutine. On a 10-core machine sweeping 4×4 = 16 combinations over 1 week of 1-second features (~600k events), the sweep completes in under 30 seconds.

```bash
# Control parallelism (default: GOMAXPROCS)
./bin/calibrate --strategy vpin --data ... --workers 4
```

---

## Interpreting results

**What to look for:**

- **Sharpe > 1.0** in walk-forward test window suggests meaningful signal.
- **MDD < 0.15** (15%) keeps the strategy within the default risk limits.
- **Hit rate 0.50–0.65** is typical for mean-reversion strategies. Higher is not always better — it may indicate too-tight thresholds with tiny wins.
- **Total fills > 50** per sweep day provides statistical significance. Fewer fills → high variance Sharpe.

**Red flags:**

- `test_sharpe` significantly lower than `train_sharpe` → overfitting. Widen parameter increments or shrink the grid.
- `total_fills < 10` → threshold too restrictive; the strategy barely trades.
- `mdd > max_drawdown_pct` in config → the calibrated parameter set will trip the risk halt in live trading.

---

## After calibration

1. Update `configs/default.yaml` with the chosen parameter values.
2. Run a confirming backtest: `./bin/backtest --strategy obi --data data/historical.jsonl --report data/reports/obi-confirm.html`
3. Review the HTML report for equity curve shape, drawdown, and fills distribution.
4. Set conservative risk limits: `risk.max_drawdown_pct` ≥ 2× the calibrated `mdd`, `risk.daily_loss_limit` calibrated against the worst single-day loss in the test window.

---

## Fetching historical data

`cmd/fetcher` downloads Binance `aggTrades` and computes windowed OBI/VPIN/VWAP features, writing a JSONL file suitable for both the backtest engine and the calibration CLI.

```bash
# Default: last 7 days of BTC-USDT 1-second features
./bin/fetcher --symbol BTCUSDT --days 7 --out data/historical.jsonl

# ETH-USDT, 30 days
./bin/fetcher --symbol ETHUSDT --days 30 --out data/eth-30d.jsonl
```

The feature schema matches Muninn's `FeatureComputedEvent`, so a backtest replayed through the calibration CLI produces the same execution as a live Huginn run receiving features from Muninn — modulo replay semantics (no live fills, no Kafka latency).
