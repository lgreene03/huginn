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

// TestOverSellClipped verifies the long-only invariant (quant-6): selling more
// than is held clips to available inventory — no fabricated PnL on quantity not
// held and never a negative position.
func TestOverSellClipped(t *testing.T) {
	p := New(100_000.0)

	// Buy 1.0 BTC at 50,000 (no fee, to isolate the clip arithmetic).
	p.ApplyFill(model.Fill{
		OrderID:    "buy-1",
		Instrument: "BTC-USDT",
		Side:       model.Buy,
		Quantity:   1.0,
		FillPrice:  50_000.0,
		Timestamp:  time.Now(),
	})

	// Attempt to sell 3.0 BTC at 51,000 — only 1.0 is held, so 1.0 fills.
	// A 30.0 transaction cost on the full 3.0 order scales to the 1.0 that
	// actually executes (10.0).
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
	if pos.Quantity != 0.0 {
		t.Errorf("expected flat (non-negative) position after over-sell, got %.8f", pos.Quantity)
	}

	// Realized PnL is on the 1.0 actually held, net of the scaled exit fee:
	// (51000 - 50000) * 1.0 - (30.0 * 1.0/3.0) = 1000 - 10 = 990.
	expectedRealized := (51_000.0-50_000.0)*1.0 - (30.0 * 1.0 / 3.0)
	if !approxEqual(snap.RealizedPnL, expectedRealized) {
		t.Errorf("expected realized PnL %.4f (no fabricated PnL on unheld qty), got %.4f",
			expectedRealized, snap.RealizedPnL)
	}

	// Cash reflects only the 1.0 that actually filled, less the scaled fee:
	// 100000 - 50000 (buy) + 51000 (sell 1.0) - 10 (scaled fee) = 100990.
	expectedCash := 100_000.0 - 50_000.0 + 51_000.0 - (30.0 * 1.0 / 3.0)
	if !approxEqual(snap.Cash, expectedCash) {
		t.Errorf("expected cash %.4f, got %.4f", expectedCash, snap.Cash)
	}

	// Only the executed share of the fee counts toward total costs.
	if !approxEqual(snap.TotalCosts, 30.0*1.0/3.0) {
		t.Errorf("expected total costs %.4f, got %.4f", 30.0*1.0/3.0, snap.TotalCosts)
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
