package portfolio

import (
	"math"
	"testing"
	"testing/quick"
	"time"

	"github.com/lgreene03/huginn/internal/model"
)

// These are property-based tests (stdlib testing/quick) over random valid fill
// sequences. They assert the structural invariants of the FIFO avg-cost ledger
// rather than hand-picked arithmetic, complementing the example-based cases in
// portfolio_test.go.

const propEps = 1e-6

// genFill is a single randomized fill drawn from sane economic ranges. Side is
// chosen by a bool because model.Side has no quick.Generator and unconstrained
// ints would mostly miss the two valid variants.
type genFill struct {
	IsBuy     bool
	Quantity  float64
	FillPrice float64
	TxCost    float64
}

// toModel converts a generated fill into a model.Fill against a fixed
// instrument. Quantity/price/cost are mapped into realistic positive ranges so
// the sequences exercise the ledger rather than degenerate zeros/NaNs.
func (g genFill) toModel(instrument string) model.Fill {
	side := model.Sell
	if g.IsBuy {
		side = model.Buy
	}
	// Map the raw [0,1)-ish quick floats into bounded positive ranges.
	qty := 0.01 + math.Mod(math.Abs(g.Quantity), 5.0)
	price := 100.0 + math.Mod(math.Abs(g.FillPrice), 90_000.0)
	cost := math.Mod(math.Abs(g.TxCost), 50.0)
	return model.Fill{
		OrderID:         "prop",
		Instrument:      instrument,
		Side:            side,
		Quantity:        qty,
		FillPrice:       price,
		TransactionCost: cost,
		Timestamp:       time.Now(),
	}
}

// replay applies a sequence of generated fills to a fresh portfolio and returns
// it plus the concrete model.Fills actually submitted (post range-mapping).
func replay(initialCash float64, gens []genFill, instrument string) (*Portfolio, []model.Fill) {
	p := New(initialCash)
	applied := make([]model.Fill, 0, len(gens))
	for _, g := range gens {
		f := g.toModel(instrument)
		p.ApplyFill(f)
		applied = append(applied, f)
	}
	return p, applied
}

func approxEqualEps(a, b, eps float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d <= eps
}

// relApproxEqual compares with a tolerance that scales with magnitude, so a
// long random sequence summing six-figure cash values isn't failed by ordinary
// float64 rounding noise.
func relApproxEqual(a, b float64) bool {
	scale := math.Max(math.Max(math.Abs(a), math.Abs(b)), 1.0)
	return math.Abs(a-b) <= 1e-6*scale
}

// TestProp_LongOnly asserts no fill sequence ever drives a position negative
// (sell-clipping enforces the long-only invariant, quant-6).
func TestProp_LongOnly(t *testing.T) {
	const instrument = "BTC-USDT"
	f := func(gens []genFill) bool {
		p, _ := replay(1_000_000.0, gens, instrument)
		snap := p.Snapshot()
		for _, pos := range snap.Positions {
			if pos.Quantity < 0 {
				return false
			}
		}
		return true
	}
	if err := quick.Check(f, &quick.Config{MaxCount: 2000}); err != nil {
		t.Error(err)
	}
}

// TestProp_TotalValueReconciles asserts the equity identity
// TotalValue == cash + sum(qty*mark) holds for any fill sequence. This is the
// canonical reconciliation the Snapshot comment (quant) warns must not drift.
func TestProp_TotalValueReconciles(t *testing.T) {
	const instrument = "BTC-USDT"
	f := func(gens []genFill) bool {
		p, _ := replay(1_000_000.0, gens, instrument)
		snap := p.Snapshot()

		recomputed := snap.Cash
		for _, pos := range snap.Positions {
			if pos.Quantity > 0 {
				mark := pos.LastMarkPrice
				if mark <= 0 {
					mark = pos.AverageCost
				}
				recomputed += pos.Quantity * mark
			}
		}
		return relApproxEqual(snap.TotalValue, recomputed)
	}
	if err := quick.Check(f, &quick.Config{MaxCount: 2000}); err != nil {
		t.Error(err)
	}
}

// TestProp_EquityConservation asserts the deeper accounting identity: total
// equity equals initial cash plus realized plus unrealized PnL. Cash already
// paid out each position's (fee-inclusive) cost basis on the buy, so
//
//	cash + sum(qty*mark) == initialCash + realized + unrealized
//
// must hold for every reachable state, including mid-sequence open inventory.
func TestProp_EquityConservation(t *testing.T) {
	const instrument = "BTC-USDT"
	const initialCash = 1_000_000.0
	f := func(gens []genFill) bool {
		p, _ := replay(initialCash, gens, instrument)
		snap := p.Snapshot()
		lhs := snap.TotalValue // cash + market value of open positions
		rhs := initialCash + snap.RealizedPnL + snap.UnrealizedPnL
		return relApproxEqual(lhs, rhs)
	}
	if err := quick.Check(f, &quick.Config{MaxCount: 2000}); err != nil {
		t.Error(err)
	}
}

// TestProp_AverageCostFeeInclusive asserts the buy-side cost basis is always
// fee-inclusive and consistent: after a sequence of buys only (no sells to
// reset it), AverageCost*Quantity equals the summed notional plus all buy fees
// (quant-5). Using buys-only isolates the accumulation rule from sell resets.
func TestProp_AverageCostFeeInclusive(t *testing.T) {
	const instrument = "BTC-USDT"
	f := func(gens []genFill) bool {
		// Force every fill to a buy so the position only accumulates.
		buys := make([]genFill, len(gens))
		for i, g := range gens {
			g.IsBuy = true
			buys[i] = g
		}
		p, applied := replay(10_000_000.0, buys, instrument)
		snap := p.Snapshot()

		if len(applied) == 0 {
			// No fills: no position expected.
			_, exists := snap.Positions[instrument]
			return !exists
		}

		var wantNotionalPlusFees, wantQty float64
		for _, f := range applied {
			wantNotionalPlusFees += f.FillPrice*f.Quantity + f.TransactionCost
			wantQty += f.Quantity
		}
		pos := snap.Positions[instrument]
		if !relApproxEqual(pos.Quantity, wantQty) {
			return false
		}
		gotBasis := pos.AverageCost * pos.Quantity
		return relApproxEqual(gotBasis, wantNotionalPlusFees)
	}
	if err := quick.Check(f, &quick.Config{MaxCount: 2000}); err != nil {
		t.Error(err)
	}
}

// TestProp_ClosedRoundTripRealizes asserts that a single buy followed by a sell
// of the full held quantity realizes exactly price-delta*qty minus both legs'
// fees, for randomized prices/quantities/fees. This is the round-trip identity
// (quant-5) exercised across the input space rather than one example.
func TestProp_ClosedRoundTripRealizes(t *testing.T) {
	const instrument = "ETH-USDT"
	// Two independent fills: a buy, then a full sell of whatever was bought.
	f := func(buy, sell genFill) bool {
		buy.IsBuy = true
		bm := buy.toModel(instrument)

		p := New(10_000_000.0)
		p.ApplyFill(bm)

		// Sell exactly the held quantity at the (randomized) sell price/fee.
		sm := sell.toModel(instrument)
		sm.Side = model.Sell
		sm.Quantity = bm.Quantity
		p.ApplyFill(sm)

		snap := p.Snapshot()

		// Position must be flat after the full-size round trip.
		if pos, ok := snap.Positions[instrument]; ok && pos.Quantity != 0 {
			return false
		}

		gross := (sm.FillPrice - bm.FillPrice) * bm.Quantity
		wantRealized := gross - bm.TransactionCost - sm.TransactionCost
		if !relApproxEqual(snap.RealizedPnL, wantRealized) {
			return false
		}

		// Flat round trip: cash delta equals realized PnL exactly.
		return relApproxEqual(snap.Cash-10_000_000.0, wantRealized)
	}
	if err := quick.Check(f, &quick.Config{MaxCount: 2000}); err != nil {
		t.Error(err)
	}
}

// TestProp_TotalFillsCounted asserts every submitted fill is counted (the
// counter is a simple structural invariant: it must equal the sequence length).
func TestProp_TotalFillsCounted(t *testing.T) {
	const instrument = "BTC-USDT"
	f := func(gens []genFill) bool {
		p, applied := replay(1_000_000.0, gens, instrument)
		return p.Snapshot().TotalFills == len(applied)
	}
	if err := quick.Check(f, &quick.Config{MaxCount: 1000}); err != nil {
		t.Error(err)
	}
}
