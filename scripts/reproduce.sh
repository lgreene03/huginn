#!/usr/bin/env bash
# Reproduce the published walk-forward edge verdict from the committed fixture.
#
# The numbers in docs/RESULTS.md are not hand-typed claims. This regenerates
# them from data/btc_test.jsonl with the exact documented command and FAILS if
# they drift, so "no measured out-of-sample edge (0/4 folds, PBO=1.00)" is a
# verifiable, one-command result rather than a marketing line.
#
#   make reproduce        # or: bash scripts/reproduce.sh
#
# Exit status is 0 only when every headline number regenerates exactly.
set -uo pipefail
cd "$(dirname "$0")/.."

DATA="data/btc_test.jsonl"

# --- The pinned verdict (docs/RESULTS.md). Update ONLY with a deliberate,
#     documented result change, never to make a red reproduce go green. ---
EXPECT_OOS="0/4"
EXPECT_PNL="-146.1090"
EXPECT_PBO="1.0000"
CMD_ARGS="--data ${DATA} --folds 4 --thresholds 0.5,0.6,0.7,0.8"

if [ ! -f "${DATA}" ]; then
  echo "reproduce: fixture ${DATA} not found (run from the huginn repo root)." >&2
  exit 2
fi
if ! command -v go >/dev/null 2>&1; then
  echo "reproduce: Go toolchain not found; cannot regenerate the verdict." >&2
  exit 2
fi

echo "Reproducing the walk-forward verdict:"
echo "  go run ./cmd/walkforward ${CMD_ARGS}"
echo

OUT="$(go run ./cmd/walkforward ${CMD_ARGS} 2>&1)"
status=$?
if [ ${status} -ne 0 ]; then
  echo "${OUT}"
  echo "reproduce: walk-forward run failed (exit ${status})." >&2
  exit 1
fi

oos="$(printf '%s\n' "${OUT}" | grep -oE 'OOS folds profitable: [0-9]+/[0-9]+' | grep -oE '[0-9]+/[0-9]+' | head -1)"
pnl="$(printf '%s\n' "${OUT}" | grep -oE 'Total OOS PnL: +-?[0-9]+\.[0-9]+' | grep -oE '\-?[0-9]+\.[0-9]+' | head -1)"
pbo="$(printf '%s\n' "${OUT}" | grep -oE 'PBO: [0-9]+\.[0-9]+' | grep -oE '[0-9]+\.[0-9]+' | head -1)"

printf '  %-22s %-12s (expected %s)\n' "OOS folds profitable:" "${oos:-<none>}" "${EXPECT_OOS}"
printf '  %-22s %-12s (expected %s)\n' "Total OOS PnL:"        "${pnl:-<none>}" "${EXPECT_PNL}"
printf '  %-22s %-12s (expected %s)\n' "PBO (CSCV):"           "${pbo:-<none>}" "${EXPECT_PBO}"
echo

fail=0
[ "${oos}" = "${EXPECT_OOS}" ] || { echo "DRIFT: OOS folds profitable changed (${oos} != ${EXPECT_OOS})"; fail=1; }
[ "${pnl}" = "${EXPECT_PNL}" ] || { echo "DRIFT: total OOS PnL changed (${pnl} != ${EXPECT_PNL})"; fail=1; }
[ "${pbo}" = "${EXPECT_PBO}" ] || { echo "DRIFT: PBO changed (${pbo} != ${EXPECT_PBO})"; fail=1; }

if [ "${fail}" -ne 0 ]; then
  echo
  echo "REPRODUCE FAILED: the published verdict no longer regenerates from the fixture."
  echo "If a code change intentionally altered the result, update docs/RESULTS.md and the"
  echo "EXPECT_* values above in the same commit. Otherwise, something regressed."
  exit 1
fi

echo "REPRODUCE OK: docs/RESULTS.md regenerates exactly."
echo "  No measured out-of-sample edge: 0/4 OOS folds profitable, PBO=1.00, OOS PnL -146.11."
echo "  (This is the honest headline, verified end-to-end from the committed data.)"
