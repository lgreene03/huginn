package portfolio

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/lgreene03/huginn/internal/model"
)

// quietLogs swaps the default slog logger for a discard handler for the
// duration of a benchmark. ApplyFill emits a slog.Debug line per fill; with the
// default handler at Info level it is already suppressed, but discarding keeps
// the measurement independent of the ambient log level.
func quietLogs(b *testing.B) {
	b.Helper()
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	b.Cleanup(func() { slog.SetDefault(prev) })
}

// BenchmarkApplyFill measures the cost of applying a single signed fill:
// cash/position math, the same-direction-add vs opposing-close branch, and the
// fills-slice append. Iterations alternate Buy/Sell on one instrument so both
// the open-and-add and close-and-flip code paths are exercised across the run.
//
// Note: ApplyFill appends every fill to an unbounded internal slice (the audit
// ledger), so the slice grows to b.N entries — amortised append cost is part of
// what this measures, matching production where the ledger accumulates.
func BenchmarkApplyFill(b *testing.B) {
	quietLogs(b)
	p := New(1_000_000_000)
	ts := time.Unix(1_700_000_000, 0)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		side := model.Buy
		if i%2 == 1 {
			side = model.Sell
		}
		p.ApplyFill(model.Fill{
			OrderID:         "bench-fill",
			Instrument:      "BTC-USDT",
			Side:            side,
			Quantity:        0.01,
			FillPrice:       60_000,
			TransactionCost: 0.6,
			SlippageBps:     0.5,
			Timestamp:       ts,
		})
	}
}
