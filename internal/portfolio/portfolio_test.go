package portfolio

import (
	"testing"
	"time"

	"github.com/lgreene/huginn/internal/model"
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
	if pos.AverageCost != 60_000.0 {
		t.Errorf("expected avg cost 60000, got %.2f", pos.AverageCost)
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
	expectedRealizedPnL := (61_000.0 - 60_000.0) * 0.5 // 500.0
	if snap.RealizedPnL != expectedRealizedPnL {
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
	// Unrealized: (2500 - 2500) * 10 = 0
	// Total: 24987.5
	if snap.TotalValue != 24_987.5 {
		t.Errorf("expected total value 24987.5, got %.2f", snap.TotalValue)
	}
}
