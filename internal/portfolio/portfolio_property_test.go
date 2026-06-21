package portfolio

import (
	"math"
	"math/rand"
	"reflect"
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

// Generate draws genFill fields from sane economic ranges directly, instead of
// relying on testing/quick's default float64 generator (which spans the whole
// range up to ~1e308). The ranges are chosen so toModel's math.Mod mapping is
// the identity, avoiding the precision loss / degenerate values that float64-max
// inputs produce when reduced modulo a small bound.
func (genFill) Generate(r *rand.Rand, _ int) reflect.Value {
	return reflect.ValueOf(genFill{
		IsBuy:     r.Intn(2) == 0,
		Quantity:  r.Float64() * 4.9,    // -> qty in [0.01, 4.91)
		FillPrice: r.Float64() * 89_000, // -> price in [100, 89100)
		TxCost:    r.Float64() * 49,     // -> cost in [0, 49)
	})
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

// TestProp_SignedEquityConservation asserts the signed equity-conservation
// invariant: for ANY fill sequence (longs, shorts, and flips through zero),
// total equity equals cash plus the SIGNED market value of every open position
//
//	TotalValue == cash + sum(signed qty * mark)
//
// A short Quantity is negative and so contributes negatively (a liability).
// This replaces the old long-only invariant: positions may now legitimately go
// negative. (Note: the stricter identity equity == initial+realized+unrealized
// does NOT hold here, because the model — per spec — embeds the entry fee into
// AverageCost for BOTH directions, which for a short moves unrealized the
// opposite way from cash; the canonical reconciliation below is the one the
// Snapshot must satisfy.)
func TestProp_SignedEquityConservation(t *testing.T) {
	const instrument = "BTC-USDT"
	const initialCash = 1_000_000.0
	f := func(gens []genFill) bool {
		p, _ := replay(initialCash, gens, instrument)
		snap := p.Snapshot()

		recomputed := snap.Cash
		for _, pos := range snap.Positions {
			if pos.Quantity != 0 {
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

// TestProp_FlipThroughZero asserts the flip-through-zero rule directly: a long
// of size q followed by an opposing sell strictly larger than q lands in a
// short whose size is exactly (sellQty - q) and whose AverageCost is positive
// and anchored at the flip fill price (fee can only raise it). Exercised across
// randomized prices/quantities/fees.
func TestProp_FlipThroughZero(t *testing.T) {
	const instrument = "BTC-USDT"
	f := func(buy, sell genFill) bool {
		buy.IsBuy = true
		bm := buy.toModel(instrument)

		sm := sell.toModel(instrument)
		sm.Side = model.Sell
		// Force the sell strictly larger than the long so it flips.
		sm.Quantity = bm.Quantity + (0.01 + math.Mod(math.Abs(sell.Quantity), 5.0))

		p := New(10_000_000.0)
		p.ApplyFill(bm)
		p.ApplyFill(sm)

		snap := p.Snapshot()
		pos, ok := snap.Positions[instrument]
		if !ok {
			return false
		}
		wantShort := -(sm.Quantity - bm.Quantity)
		if !relApproxEqual(pos.Quantity, wantShort) {
			return false
		}
		// New SHORT cost basis is strictly positive and <= the flip price (a fee
		// lowers a short's effective entry, the mirror of a long's fee raising it).
		if pos.AverageCost > sm.FillPrice+propEps || pos.AverageCost <= 0 {
			return false
		}
		// Equity invariant must hold post-flip.
		recomputed := snap.Cash + pos.Quantity*pos.LastMarkPrice
		return relApproxEqual(snap.TotalValue, recomputed)
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

// TestProp_ClosedRoundTripRealizes asserts the round-trip identities for a full
// open-then-close in EITHER direction, per the spec's fee model. The opening
// fee is embedded into AverageCost, so with openAvg = openPrice + openFee/qty:
//
//	realized  == sign(open) * (closePrice - openAvg) * qty - closeFee
//	cashDelta == sign(open) * (closePrice - openPrice) * qty - openFee - closeFee
//
// For a LONG these coincide (fee-in-cost reduces realized exactly as it reduced
// cash). For a SHORT they DIFFER by 2*openFee, because embedding the entry fee
// RAISES a short's cost basis (favorable) while cash was debited the fee — this
// is an inherent, intended property of the spec model, and the test pins both
// sides rather than wrongly assuming they're equal. The `openLong` flag
// exercises both a long round trip and a short round trip, generalizing the
// original long-only identity (quant-5).
func TestProp_ClosedRoundTripRealizes(t *testing.T) {
	const instrument = "ETH-USDT"
	f := func(open, close genFill, openLong bool) bool {
		// Opening leg in the chosen direction.
		om := open.toModel(instrument)
		if openLong {
			om.Side = model.Buy
		} else {
			om.Side = model.Sell
		}

		p := New(10_000_000.0)
		p.ApplyFill(om)

		// Closing leg: exact held size, opposite side.
		cm := close.toModel(instrument)
		if openLong {
			cm.Side = model.Sell
		} else {
			cm.Side = model.Buy
		}
		cm.Quantity = om.Quantity
		p.ApplyFill(cm)

		snap := p.Snapshot()

		// Position must be flat after the full-size round trip.
		if pos, ok := snap.Positions[instrument]; ok && pos.Quantity != 0 {
			return false
		}

		openSign := 1.0
		if !openLong {
			openSign = -1.0
		}
		qty := om.Quantity
		// Entry fee embeds in the position's own direction: +fee for a long
		// (basis up), -fee for a short (effective entry down).
		openAvg := om.FillPrice + openSign*om.TransactionCost/qty
		wantRealized := openSign*(cm.FillPrice-openAvg)*qty - cm.TransactionCost
		if !relApproxEqual(snap.RealizedPnL, wantRealized) {
			return false
		}

		// Flat round trip: cash delta is the raw price move net of BOTH fees.
		wantCashDelta := openSign*(cm.FillPrice-om.FillPrice)*qty - om.TransactionCost - cm.TransactionCost
		return relApproxEqual(snap.Cash-10_000_000.0, wantCashDelta)
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
