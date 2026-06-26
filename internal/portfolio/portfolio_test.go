package portfolio

import (
	"testing"
	"time"

	"github.com/lgreene03/huginn/internal/model"
)

func TestBuyAndSellPnL(t *testing.T) {
	p := New(100_000.0)

	// Buy 0.5 BTC at 60,000
	p.ApplyFill(model.Fill{
		OrderID:         "fill-1",
		Instrument:      "BTC-USDT",
		Side:            model.Buy,
		Quantity:        0.5,
		FillPrice:       60_000.0,
		TransactionCost: 15.0,
		SlippageBps:     0.5,
		Timestamp:       time.Now(),
	})

	snap := p.Snapshot()
	expectedCash := 100_000.0 - (60_000.0 * 0.5) - 15.0
	if snap.Cash != expectedCash {
		t.Errorf("expected cash %.2f, got %.2f", expectedCash, snap.Cash)
	}

	pos, ok := snap.Positions["BTC-USDT"]
	if !ok {
		t.Fatal("expected BTC-USDT position to exist")
	}
	if pos.Quantity != 0.5 {
		t.Errorf("expected quantity 0.5, got %.8f", pos.Quantity)
	}
	// Cost basis is fee-inclusive (quant-5): (60000*0.5 + 15.0) / 0.5 = 60030.
	if !approxEqual(pos.AverageCost, 60_030.0) {
		t.Errorf("expected fee-inclusive avg cost 60030, got %.2f", pos.AverageCost)
	}

	// Sell 0.5 BTC at 61,000 — profit of 500
	p.ApplyFill(model.Fill{
		OrderID:         "fill-2",
		Instrument:      "BTC-USDT",
		Side:            model.Sell,
		Quantity:        0.5,
		FillPrice:       61_000.0,
		TransactionCost: 15.25,
		SlippageBps:     0.5,
		Timestamp:       time.Now(),
	})

	snap = p.Snapshot()
	// Realized PnL is NET of both legs' transaction costs (quant-5):
	// gross (61000-60000)*0.5 = 500, minus buy fee 15.0 minus sell fee 15.25.
	expectedRealizedPnL := (61_000.0-60_000.0)*0.5 - 15.0 - 15.25 // 469.75
	if !approxEqual(snap.RealizedPnL, expectedRealizedPnL) {
		t.Errorf("expected realized PnL %.2f, got %.2f", expectedRealizedPnL, snap.RealizedPnL)
	}

	pos, ok = snap.Positions["BTC-USDT"]
	if !ok {
		t.Fatal("expected BTC-USDT position to exist")
	}
	if pos.Quantity != 0.0 {
		t.Errorf("expected flat position, got %.8f", pos.Quantity)
	}
	if snap.TotalFills != 2 {
		t.Errorf("expected 2 fills, got %d", snap.TotalFills)
	}
}

func TestSnapshotTotalValue(t *testing.T) {
	p := New(50_000.0)

	p.ApplyFill(model.Fill{
		OrderID:         "fill-1",
		Instrument:      "ETH-USDT",
		Side:            model.Buy,
		Quantity:        10.0,
		FillPrice:       2_500.0,
		TransactionCost: 12.5,
		Timestamp:       time.Now(),
	})

	snap := p.Snapshot()
	// Cash: 50000 - 25000 - 12.5 = 24987.5
	// Cost basis is fee-inclusive (quant-5): avg cost = (25000 + 12.5)/10 = 2501.25.
	// Position market value: 10 * 2500 (last mark) = 25000.
	// Total equity = cash + market value = 24987.5 + 25000 = 49987.5,
	// i.e. the starting 50000 less the 12.5 fee just paid. (Equity must NOT be
	// cash + unrealized, which would wrongly report 24975.0 — as if buying an
	// asset at fair value instantly halved the account.)
	if !approxEqual(snap.TotalValue, 49_987.5) {
		t.Errorf("expected total value 49987.5, got %.2f", snap.TotalValue)
	}
	// Unrealized is still just the fee already incurred.
	if !approxEqual(snap.UnrealizedPnL, -12.5) {
		t.Errorf("expected unrealized -12.5, got %.2f", snap.UnrealizedPnL)
	}
}

func approxEqual(a, b float64) bool {
	const eps = 1e-9
	d := a - b
	if d < 0 {
		d = -d
	}
	return d < eps
}

// TestOverSellFlipsToShort verifies the signed-position model: selling MORE
// than is held no longer clips — it closes the long and flips the remainder
// into a short, realizing PnL only on the closed portion.
func TestOverSellFlipsToShort(t *testing.T) {
	p := New(100_000.0)

	// Buy 1.0 BTC at 50,000 (no fee, to isolate the flip arithmetic).
	p.ApplyFill(model.Fill{
		OrderID:    "buy-1",
		Instrument: "BTC-USDT",
		Side:       model.Buy,
		Quantity:   1.0,
		FillPrice:  50_000.0,
		Timestamp:  time.Now(),
	})

	// Sell 3.0 BTC at 51,000 with a 30.0 fee on the full order: 1.0 closes the
	// long (realizing PnL net of the prorated close fee), and 2.0 opens a short.
	p.ApplyFill(model.Fill{
		OrderID:         "sell-1",
		Instrument:      "BTC-USDT",
		Side:            model.Sell,
		Quantity:        3.0,
		FillPrice:       51_000.0,
		TransactionCost: 30.0,
		Timestamp:       time.Now(),
	})

	snap := p.Snapshot()

	pos := snap.Positions["BTC-USDT"]
	// Remainder of 2.0 opens a SHORT.
	if !approxEqual(pos.Quantity, -2.0) {
		t.Errorf("expected short position -2.0 after over-sell flip, got %.8f", pos.Quantity)
	}
	// New short cost basis embeds the remaining fee portion in the SHORT's own
	// direction (a fee lowers a short's effective entry): the close consumed
	// 1.0/3.0 of the 30.0 fee, leaving 20.0 spread over the 2.0 short:
	// 51000 - (30*(2/3))/2 = 51000 - 10 = 50990. (So a later cover realizes PnL
	// net of both legs' fees, exactly as a long round trip does.)
	if !approxEqual(pos.AverageCost, 50_990.0) {
		t.Errorf("expected short avg cost 50990, got %.4f", pos.AverageCost)
	}

	// Realized PnL is on the 1.0 closed, net of the prorated close fee:
	// (51000 - 50000) * 1.0 - (30.0 * 1.0/3.0) = 1000 - 10 = 990.
	expectedRealized := (51_000.0-50_000.0)*1.0 - (30.0 * 1.0 / 3.0)
	if !approxEqual(snap.RealizedPnL, expectedRealized) {
		t.Errorf("expected realized PnL %.4f, got %.4f", expectedRealized, snap.RealizedPnL)
	}

	// Cash reflects the full 3.0 sell proceeds, less the full fee:
	// 100000 - 50000 (buy) + 153000 (sell 3.0 @51000) - 30 (fee) = 202970.
	expectedCash := 100_000.0 - 50_000.0 + 51_000.0*3.0 - 30.0
	if !approxEqual(snap.Cash, expectedCash) {
		t.Errorf("expected cash %.4f, got %.4f", expectedCash, snap.Cash)
	}

	// The full fee is now a real cost (no clipping/scaling).
	if !approxEqual(snap.TotalCosts, 30.0) {
		t.Errorf("expected total costs 30.0, got %.4f", snap.TotalCosts)
	}

	// Equity invariant must hold through the flip.
	assertEquityInvariant(t, snap)
}

// assertEquityInvariant checks TotalValue == cash + sum(signed qty * mark).
func assertEquityInvariant(t *testing.T, snap Snapshot) {
	t.Helper()
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
	if !approxEqual(snap.TotalValue, recomputed) {
		t.Errorf("equity invariant violated: TotalValue %.6f != cash+sum(qty*mark) %.6f",
			snap.TotalValue, recomputed)
	}
}

// TestShortOpen verifies opening a short from flat: cash receives proceeds,
// Quantity goes negative, AverageCost is the positive entry, and a falling mark
// produces positive unrealized PnL (the short profits as price drops).
func TestShortOpen(t *testing.T) {
	p := New(1_000.0)

	// Short 1 @ 100 from flat (no fee).
	p.ApplyFill(model.Fill{
		OrderID:    "short-1",
		Instrument: "X",
		Side:       model.Sell,
		Quantity:   1.0,
		FillPrice:  100.0,
		Timestamp:  time.Now(),
	})

	snap := p.Snapshot()
	if !approxEqual(snap.Cash, 1_100.0) {
		t.Errorf("expected cash 1100 after short proceeds, got %.4f", snap.Cash)
	}
	pos := snap.Positions["X"]
	if !approxEqual(pos.Quantity, -1.0) {
		t.Errorf("expected quantity -1, got %.8f", pos.Quantity)
	}
	if !approxEqual(pos.AverageCost, 100.0) {
		t.Errorf("expected avg cost 100, got %.4f", pos.AverageCost)
	}

	// At the entry mark (100) the short is flat in PnL and equity == start.
	if !approxEqual(snap.UnrealizedPnL, 0.0) {
		t.Errorf("expected unrealized 0 at entry mark, got %.4f", snap.UnrealizedPnL)
	}
	if !approxEqual(snap.TotalValue, 1_000.0) {
		t.Errorf("expected total value 1000 at entry mark, got %.4f", snap.TotalValue)
	}
	assertEquityInvariant(t, snap)

	// Add to the short at a lower price (90), moving the mark down. The short
	// now profits: average entry = (100 + 90)/2 = 95 over 2 units, mark 90 =>
	// unrealized = (90-95)*-2 = +10, equity = cash(1190) + (-2*90) = 1010.
	p.ApplyFill(model.Fill{
		OrderID: "short-2", Instrument: "X", Side: model.Sell,
		Quantity: 1.0, FillPrice: 90.0, Timestamp: time.Now(),
	})
	snap = p.Snapshot()
	pos = snap.Positions["X"]
	if !approxEqual(pos.Quantity, -2.0) {
		t.Errorf("expected quantity -2 after adding to short, got %.8f", pos.Quantity)
	}
	if !approxEqual(pos.AverageCost, 95.0) {
		t.Errorf("expected avg cost 95, got %.4f", pos.AverageCost)
	}
	if !approxEqual(snap.UnrealizedPnL, 10.0) {
		t.Errorf("expected unrealized +10 on falling mark, got %.4f", snap.UnrealizedPnL)
	}
	if !approxEqual(snap.TotalValue, 1_010.0) {
		t.Errorf("expected total value 1010, got %.4f", snap.TotalValue)
	}
	assertEquityInvariant(t, snap)
}

// TestShortCoverRealizes verifies covering a short with a buy realizes the
// favorable price move: short 1 @100, buy 1 @90 => realized +10, flat.
func TestShortCoverRealizes(t *testing.T) {
	p := New(1_000.0)

	p.ApplyFill(model.Fill{
		OrderID: "short-1", Instrument: "X", Side: model.Sell,
		Quantity: 1.0, FillPrice: 100.0, Timestamp: time.Now(),
	})
	p.ApplyFill(model.Fill{
		OrderID: "cover-1", Instrument: "X", Side: model.Buy,
		Quantity: 1.0, FillPrice: 90.0, Timestamp: time.Now(),
	})

	snap := p.Snapshot()
	// realized = sign(-1)*(90-100)*1 = +10.
	if !approxEqual(snap.RealizedPnL, 10.0) {
		t.Errorf("expected realized +10 on short cover, got %.4f", snap.RealizedPnL)
	}
	pos := snap.Positions["X"]
	if pos.Quantity != 0 {
		t.Errorf("expected flat after cover, got %.8f", pos.Quantity)
	}
	if !approxEqual(snap.TotalValue, 1_010.0) {
		t.Errorf("expected total value 1010, got %.4f", snap.TotalValue)
	}
}

// TestFlipLongToShort verifies long 1 @100, sell 2 @110 closes the long
// (realized +10) and opens a short of 1 @110.
func TestFlipLongToShort(t *testing.T) {
	p := New(1_000.0)

	p.ApplyFill(model.Fill{
		OrderID: "buy-1", Instrument: "X", Side: model.Buy,
		Quantity: 1.0, FillPrice: 100.0, Timestamp: time.Now(),
	})
	p.ApplyFill(model.Fill{
		OrderID: "sell-2", Instrument: "X", Side: model.Sell,
		Quantity: 2.0, FillPrice: 110.0, Timestamp: time.Now(),
	})

	snap := p.Snapshot()
	if !approxEqual(snap.RealizedPnL, 10.0) {
		t.Errorf("expected realized +10 closing the long, got %.4f", snap.RealizedPnL)
	}
	pos := snap.Positions["X"]
	if !approxEqual(pos.Quantity, -1.0) {
		t.Errorf("expected short -1 after flip, got %.8f", pos.Quantity)
	}
	if !approxEqual(pos.AverageCost, 110.0) {
		t.Errorf("expected new short avg cost 110, got %.4f", pos.AverageCost)
	}
	assertEquityInvariant(t, snap)
}

// TestFlipShortToLong verifies short 1 @100, buy 2 @90 closes the short
// (realized +10) and opens a long of 1 @90.
func TestFlipShortToLong(t *testing.T) {
	p := New(1_000.0)

	p.ApplyFill(model.Fill{
		OrderID: "short-1", Instrument: "X", Side: model.Sell,
		Quantity: 1.0, FillPrice: 100.0, Timestamp: time.Now(),
	})
	p.ApplyFill(model.Fill{
		OrderID: "buy-2", Instrument: "X", Side: model.Buy,
		Quantity: 2.0, FillPrice: 90.0, Timestamp: time.Now(),
	})

	snap := p.Snapshot()
	// realized = sign(-1)*(90-100)*1 = +10.
	if !approxEqual(snap.RealizedPnL, 10.0) {
		t.Errorf("expected realized +10 closing the short, got %.4f", snap.RealizedPnL)
	}
	pos := snap.Positions["X"]
	if !approxEqual(pos.Quantity, 1.0) {
		t.Errorf("expected long +1 after flip, got %.8f", pos.Quantity)
	}
	if !approxEqual(pos.AverageCost, 90.0) {
		t.Errorf("expected new long avg cost 90, got %.4f", pos.AverageCost)
	}
	assertEquityInvariant(t, snap)
}

// TestPartialCover verifies a short of 3 covered by a buy of 1 reduces the
// short to 2, realizes PnL on the 1 covered, and leaves AverageCost unchanged.
func TestPartialCover(t *testing.T) {
	p := New(10_000.0)

	p.ApplyFill(model.Fill{
		OrderID: "short-3", Instrument: "X", Side: model.Sell,
		Quantity: 3.0, FillPrice: 100.0, Timestamp: time.Now(),
	})
	p.ApplyFill(model.Fill{
		OrderID: "cover-1", Instrument: "X", Side: model.Buy,
		Quantity: 1.0, FillPrice: 90.0, Timestamp: time.Now(),
	})

	snap := p.Snapshot()
	// realized = sign(-3)*(90-100)*1 = +10.
	if !approxEqual(snap.RealizedPnL, 10.0) {
		t.Errorf("expected realized +10 on partial cover, got %.4f", snap.RealizedPnL)
	}
	pos := snap.Positions["X"]
	if !approxEqual(pos.Quantity, -2.0) {
		t.Errorf("expected short -2 after partial cover, got %.8f", pos.Quantity)
	}
	// Surviving short keeps its original entry basis.
	if !approxEqual(pos.AverageCost, 100.0) {
		t.Errorf("expected avg cost unchanged at 100, got %.4f", pos.AverageCost)
	}
	assertEquityInvariant(t, snap)
}

// TestEquityInvariantUnderShorts walks a mixed long/short sequence and checks
// the signed equity-conservation identity holds at the end.
func TestEquityInvariantUnderShorts(t *testing.T) {
	p := New(100_000.0)

	fills := []model.Fill{
		{OrderID: "1", Instrument: "A", Side: model.Sell, Quantity: 2.0, FillPrice: 500.0, TransactionCost: 1.0},
		{OrderID: "2", Instrument: "A", Side: model.Sell, Quantity: 1.0, FillPrice: 480.0, TransactionCost: 1.0},
		{OrderID: "3", Instrument: "B", Side: model.Buy, Quantity: 4.0, FillPrice: 100.0, TransactionCost: 2.0},
		{OrderID: "4", Instrument: "A", Side: model.Buy, Quantity: 5.0, FillPrice: 460.0, TransactionCost: 1.5},  // covers + flips A to long
		{OrderID: "5", Instrument: "B", Side: model.Sell, Quantity: 6.0, FillPrice: 110.0, TransactionCost: 2.5}, // flips B to short
	}
	for _, f := range fills {
		f.Timestamp = time.Now()
		p.ApplyFill(f)
	}

	snap := p.Snapshot()
	// Canonical signed invariant must hold through covers and flips, with fees.
	assertEquityInvariant(t, snap)
}

// TestEquityConservationFeeFree checks the stricter accounting identity
// equity == initial + realized + unrealized for a mixed long/short sequence
// with ZERO fees. (With nonzero fees this identity does not hold for shorts,
// because the spec embeds the entry fee into AverageCost for both directions —
// see TestProp_SignedEquityConservation; the canonical cash+sum(qty*mark)
// invariant is the one that always holds.)
func TestEquityConservationFeeFree(t *testing.T) {
	p := New(100_000.0)

	fills := []model.Fill{
		{OrderID: "1", Instrument: "A", Side: model.Sell, Quantity: 2.0, FillPrice: 500.0},
		{OrderID: "2", Instrument: "A", Side: model.Sell, Quantity: 1.0, FillPrice: 480.0},
		{OrderID: "3", Instrument: "B", Side: model.Buy, Quantity: 4.0, FillPrice: 100.0},
		{OrderID: "4", Instrument: "A", Side: model.Buy, Quantity: 5.0, FillPrice: 460.0},  // covers + flips A to long
		{OrderID: "5", Instrument: "B", Side: model.Sell, Quantity: 6.0, FillPrice: 110.0}, // flips B to short
	}
	for _, f := range fills {
		f.Timestamp = time.Now()
		p.ApplyFill(f)
	}

	snap := p.Snapshot()
	assertEquityInvariant(t, snap)

	rhs := 100_000.0 + snap.RealizedPnL + snap.UnrealizedPnL
	if !approxEqual(snap.TotalValue, rhs) {
		t.Errorf("fee-free equity-conservation violated: %.6f != initial+realized+unrealized %.6f",
			snap.TotalValue, rhs)
	}
}

// TestRoundTripRealizedNetOfBothFees verifies realized PnL is reduced by BOTH
// the buy-leg and sell-leg transaction costs (quant-5).
func TestRoundTripRealizedNetOfBothFees(t *testing.T) {
	p := New(100_000.0)

	const (
		qty       = 2.0
		buyPrice  = 30_000.0
		sellPrice = 31_000.0
		buyFee    = 20.0
		sellFee   = 25.0
	)

	p.ApplyFill(model.Fill{
		OrderID:         "buy-1",
		Instrument:      "ETH-USDT",
		Side:            model.Buy,
		Quantity:        qty,
		FillPrice:       buyPrice,
		TransactionCost: buyFee,
		Timestamp:       time.Now(),
	})
	p.ApplyFill(model.Fill{
		OrderID:         "sell-1",
		Instrument:      "ETH-USDT",
		Side:            model.Sell,
		Quantity:        qty,
		FillPrice:       sellPrice,
		TransactionCost: sellFee,
		Timestamp:       time.Now(),
	})

	snap := p.Snapshot()

	gross := (sellPrice - buyPrice) * qty // 2000
	netExpected := gross - buyFee - sellFee
	if !approxEqual(snap.RealizedPnL, netExpected) {
		t.Errorf("expected realized PnL %.4f (net of both fees), got %.4f",
			netExpected, snap.RealizedPnL)
	}
	// Sanity: realized must be strictly below gross by exactly the two fees.
	if !approxEqual(gross-snap.RealizedPnL, buyFee+sellFee) {
		t.Errorf("expected realized to be reduced by both fees (%.2f), reduced by %.2f",
			buyFee+sellFee, gross-snap.RealizedPnL)
	}

	// Round trip is flat, so realized PnL also equals the net cash change.
	expectedCashDelta := netExpected
	if !approxEqual(snap.Cash-100_000.0, expectedCashDelta) {
		t.Errorf("expected cash delta %.4f, got %.4f", expectedCashDelta, snap.Cash-100_000.0)
	}
}
