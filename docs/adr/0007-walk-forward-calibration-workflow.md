# 0007. Walk-forward validation that reports confidence, not a PASS/FAIL verdict

- **Status:** Accepted
- **Date:** 2026-06-20
- **Deciders:** Project maintainer
- **Related:** [ADR-0002 — Pluggable strategy interface](0002-pluggable-strategy-interface.md), [ADR-0003 — Dual-mode executor](0003-dual-mode-paper-live-executor.md), [calibration.md](../calibration.md)

## Context

A strategy with tunable parameters (OBI/VPIN/VWAP thresholds, order size, EMA periods) can always be fitted to look good on the data it was tuned on. The hard question is whether an edge *generalises* to data the parameters never saw. The research workflow exists to answer that honestly, and it must reuse the production executor — a backtest that reimplements fills, costs, or risk would validate something other than what trades (ADR-0003).

There are two distinct jobs. **Calibration** (`cmd/calibrate`) sweeps a parameter grid over one dataset to see the response surface. **Walk-forward** (`cmd/walkforward`) is the generalisation test: split history into rolling folds, *select* parameters on each fold's training window by in-sample Sharpe, then score *only* the selected parameters on the never-seen test window.

The danger is the tempting output: a single PASS/FAIL or a headline OOS Sharpe. Because parameters are chosen as the best-of-N on each train window, the in-sample winner is upward-biased by multiple testing. A binary verdict would launder that bias into false confidence — exactly the failure mode that gets overfit strategies deployed.

## Decision

Two cooperating CLIs, both running the real `executor` with a `NullWriter` (so research never touches the journal or a venue), both reading the same JSONL `FeatureEvent` format the live consumer produces:

- `cmd/calibrate` — cartesian grid sweep over one window; emits the response surface.
- `cmd/walkforward` — anchored walk-forward (training window grows from the start, test window slides forward). For each fold, `selectBestOnTrain` picks the highest in-sample-Sharpe combo; `runFold` scores only that combo out-of-sample.

The walk-forward summary **deliberately refuses a binary verdict**. It reports the OOS distribution (mean ± std of fold Sharpes), the fraction of profitable OOS folds, the in-sample→out-of-sample Sharpe *decay* ratio per fold, a cross-fold Sharpe signal-to-noise number, and — repeatedly — the count of combos searched per fold, explicitly labeled as multiple-testing bias. When the grid degenerates to one combo (no flags passed), it warns that *no real selection happened*. The interpretation is left to the reader, by design.

## Consequences

**Easier.**

- Research results reflect the real engine: same strategy code, same slippage/cost/latency model, same portfolio accounting as live, differing only in the execution branch.
- The output makes overfitting visible — a high in-sample Sharpe that decays to near-zero OOS, or a high mean OOS Sharpe with huge cross-fold dispersion, is right there in the summary instead of hidden behind a green check.
- Adding a parameter to the sweep is a flag + a row in the grid expansion; the fold machinery is parameter-agnostic.

**Harder / cost.**

- There is no one-number answer to paste into a PR. That is the intended cost: the honest output requires a human (or a downstream filter) to read the distribution and the decay. Operators wanting automation must define their own bar on top of these numbers.
- Walk-forward only selects over `threshold` and `order_size` today; EMA crossover honours order size but its periods come from config, so its "selection" is degenerate unless those are added to the grid. Documented in the CLI.
- Each fold re-runs the full grid on its (growing) train window, so cost is O(folds × grid × train-size). Fine for offline JSONL; not an online operation.

## Alternatives Considered

- **A backtest engine separate from the live executor.** Rejected. Would validate a reimplementation, not the strategy that trades (see ADR-0003).
- **Emit a single PASS/FAIL or headline OOS Sharpe.** Rejected. Hides the multiple-testing bias inherent in best-of-N parameter selection and manufactures false confidence — the precise risk this workflow exists to surface.
- **Non-anchored (sliding) train window.** Considered. Anchored (expanding) training is the more common quant convention and uses all available history up to each test window; a fixed-width sliding window is a reasonable variant that could be added behind a flag later.

## References

- [`cmd/walkforward/main.go`](../../cmd/walkforward/main.go) — anchored fold layout, `selectBestOnTrain`, `sharpeDecayRatio`, and the deliberately verdict-free `printSummary`.
- [`cmd/calibrate/main.go`](../../cmd/calibrate/main.go) — the grid-sweep companion.
- [`internal/executor/executor.go`](../../internal/executor/executor.go) — the production executor reused with `NullWriter` and `liveMode=false`.
- [`docs/calibration.md`](../calibration.md) — operator-facing calibration workflow.
