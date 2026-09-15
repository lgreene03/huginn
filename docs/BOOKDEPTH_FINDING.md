# Binance `bookDepth` dumps cannot reconstruct the live `obi`

**Status: P1 kill criterion fired. Track A of the parity roadmap stops here.**

Date: 2026-09-11. Measured, not assumed: every number below came from a real
dump or a real API response, and the commands to reproduce them are inline.

## Why this was investigated

`norse-stack`'s README headlines **deterministic replay parity**: that a
backtest and a live run share one computation path. At the *engine* level that
is true and enforced. At the *signal* level it is not:

| | live (`services/obi-bridge/bridge.py:268`) | backtest (`huginn/cmd/fetcher`) |
|---|---|---|
| `obi` | `(bid_vol - ask_vol) / total` from **L2 book depth** | `(BuyVol - SellVol) / total` from **executed trades** |

Two different quantities, book *state* versus realised *flow*, sharing one name.
Phase P1 of `norse-stack/docs/ROADMAP.md` set out to obtain real historical book
depth so that a replay could compute the same quantity the live path computes.

It cannot, for the reasons below.

## What the dumps actually contain

`https://data.binance.vision/data/futures/um/daily/bookDepth/<SYMBOL>/<SYMBOL>-bookDepth-<YYYY-MM-DD>.zip`
exists and downloads cleanly. `BTCUSDT-bookDepth-2026-09-01.zip` is 564,591
bytes zipped and 2,013,161 bytes expanded, which is already the first signal:
the same day of `aggTrades` is roughly three orders of magnitude larger.

The CSV is four columns with a header row:

```
timestamp,percentage,depth,notional
2026-09-01 00:00:04,-5.00,10267.92200000,788466627.08110000
2026-09-01 00:00:04,-4.00,8967.16600000,690917971.29840000
2026-09-01 00:00:04,-3.00,6391.67300000,495652738.52610000
2026-09-01 00:00:04,-2.00,5172.11200000,402217876.47380000
2026-09-01 00:00:04,-1.00,2393.25400000,187143664.21670000
2026-09-01 00:00:04,-0.20,813.69100000,63843569.41780000
2026-09-01 00:00:04,0.20,410.56800000,32283149.59130000
2026-09-01 00:00:04,1.00,1895.92700000,149678592.00540000
2026-09-01 00:00:04,2.00,4832.06100000,383685577.93930000
2026-09-01 00:00:04,3.00,5397.92300000,429238407.34130000
2026-09-01 00:00:04,4.00,6219.64400000,496091150.93760000
2026-09-01 00:00:04,5.00,7103.90400000,568607291.79200000
```

So, precisely:

- **Percentage bands, not price levels.** Each snapshot is 12 rows: bands at
  ±0.2%, ±1%, ±2%, ±3%, ±4% and ±5% of mid. Negative is the bid side.
- **`depth` is cumulative from the mid**, not per band. The −5.00 row implies an
  average fill price of 76,789.31 (`notional / depth`). A disjoint −5%..−4% band
  would have to average between 74,622 and 75,408, so it is not disjoint; a
  cumulative 0..−5% band is the only reading consistent with the number.
- **One snapshot every ~30 seconds.** Exactly 2880 snapshots per day
  (`00:00:04`, `00:00:31`, `00:01:02`, ...), 34,560 rows in total.
- **No prices whatsoever.** There is no best bid, best ask, spread or mid
  anywhere in the file. Only an *implied* average price per band, via
  `notional / depth`.
- **The schema is not stable across history.** The 2023 through mid-2025 dumps
  carry only **10** bands (±1% to ±5%) with **no ±0.2% band at all**, and render
  the percentage as `-5` rather than `-5.00`. The 12-band form appears between
  2025-06-15 and 2026-01-15.

## Why that cannot reconstruct `compute_obi`

The live path sums the **top 10 discrete price levels** of each side
(`BOOK_LEVELS = 10`, `bridge.py:47`), polled every 5 seconds from
`api.binance.com/api/v3/depth`. Four independent obstacles, each individually
fatal:

**1. The bands are ~100x too wide.** Measured against a live 100-level BTCUSDT
book at mid 77,361.17:

| cut of the book | span either side of mid |
|---|---|
| top 10 levels (what the live path uses) | 0.0024% bid / 0.0015% ask |
| top 20 levels | 0.0038% / 0.0036% |
| top 100 levels | 0.0173% / 0.0202% |
| **narrowest band the dump offers** | **0.20%** |

The narrowest available band is about 100x wider than the region the live signal
measures. Even 100 levels does not reach 0.02%, still an order of magnitude
inside the finest band. And before late 2025 the narrowest band is ±1%, roughly
400x wider. The two quantities do not merely differ in precision; they are
measuring different populations of resting orders. On that same snapshot the
top-10 sums were 0.9679 bid / 1.9824 ask (`obi` = −0.3439), whereas the volume
within 0.2% of mid was at least 9.91 bid / 12.23 ask, a full order of magnitude
more liquidity and a different imbalance.

**2. The per-level distribution is discarded, irreversibly.** A cumulative band
total is a projection. Nothing in the file says how the 813.691 BTC inside the
−0.2% band is distributed across the several thousand ticks it spans, so there
is no arithmetic that recovers the top 10 levels from it. This is lost
information, not missing precision, so no amount of interpolation fixes it.

**3. Wrong instrument.** The live path reads the **spot** book
(`api.binance.com/api/v3/depth`). `bookDepth` is published only for USD-M
futures. `data.binance.vision` publishes **no order-book product for spot at
all**: the spot daily prefixes are `aggTrades`, `klines` and `trades` only. So
the substitution is perpetual-for-spot, which is a different instrument with a
different book, and it is not optional.

**4. Wrong cadence.** The live path polls at 5-second intervals
(`POLL_INTERVAL`, `bridge.py:46`); the dump snapshots at ~30 seconds. A replay
cannot reproduce an event-time book state it never observed.

The closest alternative product, `bookTicker`, is level-1 only
(`best_bid_price,best_bid_qty,best_ask_price,best_ask_qty`) and its BTCUSDT
coverage does not extend to 2026-09-01. One level cannot reconstruct ten either.

## The verdict

**Book depth as published is not reconstructable into the live `obi`.**

Per `ROADMAP.md`, the correct response is to stop rather than substitute another
proxy and call it parity. Doing so would recreate the precise defect the roadmap
exists to remove, and would be worse than the current state because it would
carry a parity claim on top. Phases P2 to P5 are blocked on a data source that
does not exist in the public dumps.

If book-true parity is still wanted, the realistic options are to record live
spot L2 diffs forward from now and accept a waiting period before there is
enough history to run a walk-forward gate, or to buy per-level historical L2
from a commercial vendor. Both are decisions about cost and calendar, not
engineering, so they are escalated rather than chosen here.

Nothing about this invalidates `EDGE_VERDICT.md`. It sharpens it: that verdict
is about **signed taker flow**, and should say so. The live signal remains one
that has never been through the walk-forward gate.

## What shipped anyway, and why

`fetcher --source bookdepth` loads the dumps and emits the bands as-is. It is
deliberately **not** wired into the trade aggregator and emits **no** `obi`,
`vpin`, `microPrice` or `vwap` field. It exists so the finding above is
falsifiable by re-running it rather than taken on trust, and so the next person
who wonders whether these dumps are usable can look instead of guessing.
`TestBookDepthSnapshotCarriesNoDerivedFeatures` fails the build if that output
ever grows a feature-shaped field.

Records are tagged `BTC-USD-PERP`, not `BTC-USD`, so a perpetual snapshot can
never be mistaken for the spot instrument the live path actually trades.

## Reproducibility evidence

The mechanical half of the P1 exit criterion does pass. Two runs over the same
dated range:

```
go run ./cmd/fetcher -source bookdepth -symbol BTCUSDT \
  -start 2026-09-01 -end 2026-09-03 -output r1.jsonl
go run ./cmd/fetcher -source bookdepth -symbol BTCUSDT \
  -start 2026-09-01 -end 2026-09-03 -output r2.jsonl
```

produced 8,640 snapshots from 103,680 rows across 3 days, and:

```
28e8670ddb15e63c68bf005de22236255dd4562f9ee01c69f925452db837b2bc  r1.jsonl
28e8670ddb15e63c68bf005de22236255dd4562f9ee01c69f925452db837b2bc  r2.jsonl
cmp r1.jsonl r2.jsonl  ->  exit 0
```

Byte-identical. Determinism is by construction: days are visited in order, rows
are read in file order, and bands are sorted by percentage before emission, so
no map iteration order reaches the output.

That the loader is reproducible does not make the data sufficient. Both facts
are reported together on purpose.
