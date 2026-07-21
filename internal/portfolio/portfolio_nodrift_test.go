package portfolio

import (
	"math"
	"testing"

	"github.com/lgreene03/huginn/internal/model"
)

// TestNoDrift_LongHorizonRoundTrips is the money-integrity guard for the float64
// ledger: over a long deterministic run of identical round-trips it checks the
// accounting against a CLOSED-FORM ground truth, not just internal consistency,
// so accumulated floating-point error cannot silently drift the books.
//
// A buy of q at price p (fee f) followed by a sell of q at the same p closes the
// position flat and moves cash by exactly -2f (the two legs' fees; the notional
// nets out). After N such round-trips the analytic truth is:
//
//	position    == flat (0)
//	cash        == initial - 2*f*N
//	realizedPnL == -2*f*N   (== -totalCosts)
//	equity      == initial + realizedPnL   (unrealized == 0)
//
// We assert each to a tolerance that scales as 1e-6 per fill, i.e. accumulated
// error must stay below a micro-unit per operation. Rationale for float64 over a
// decimal type is documented in portfolio.go; this test is what makes that choice
// safe to rely on.
func TestNoDrift_LongHorizonRoundTrips(t *testing.T) {
	const (
		initial = 1_000_000.0
		price   = 60_000.0
		qty     = 0.01
		fee     = 0.05
		n       = 20_000 // 40,000 fills
	)
	p := New(initial)

	for i := 0; i < n; i++ {
		p.ApplyFill(model.Fill{
			Instrument: "BTC-USDT", Side: model.Buy,
			Quantity: qty, FillPrice: price, TransactionCost: fee,
		})
		p.ApplyFill(model.Fill{
			Instrument: "BTC-USDT", Side: model.Sell,
			Quantity: qty, FillPrice: price, TransactionCost: fee,
		})
	}

	snap := p.Snapshot()
	fills := 2 * n
	tol := 1e-6 * float64(fills) // bounded accumulation: < 1e-6 per fill

	wantCash := initial - 2*fee*float64(n)
	if got := snap.Cash; math.Abs(got-wantCash) > tol {
		t.Errorf("cash drifted: got %.10f want %.10f (|err|=%.2e > %.2e)", got, wantCash, math.Abs(got-wantCash), tol)
	}
	wantRealized := -2 * fee * float64(n)
	if got := snap.RealizedPnL; math.Abs(got-wantRealized) > tol {
		t.Errorf("realizedPnL drifted: got %.10f want %.10f", got, wantRealized)
	}
	// Position must be flat: no open positions survive an exact round-trip set.
	for _, pos := range snap.Positions {
		if math.Abs(pos.Quantity) > 1e-9 {
			t.Errorf("position not flat after balanced round-trips: %+v", pos)
		}
	}
	if math.Abs(snap.UnrealizedPnL) > tol {
		t.Errorf("unrealized should be ~0 with no open position, got %.10f", snap.UnrealizedPnL)
	}
	// Equity reconciliation: with no open position, equity == initial + realized.
	if got := snap.TotalValue; math.Abs(got-(initial+wantRealized)) > tol {
		t.Errorf("equity reconciliation failed: got %.10f want %.10f", got, initial+wantRealized)
	}
	// realizedPnL == -totalCosts for pure same-price round-trips.
	if math.Abs(snap.RealizedPnL+snap.TotalCosts) > tol {
		t.Errorf("realizedPnL (%.6f) should equal -totalCosts (%.6f)", snap.RealizedPnL, snap.TotalCosts)
	}
}
