package journal

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/lgreene03/huginn/internal/model"
)

func TestJournalRecovery(t *testing.T) {
	tempDir := t.TempDir()
	path := filepath.Join(tempDir, "test_trades.jsonl")

	w, err := NewJSONLWriter(path)
	if err != nil {
		t.Fatalf("Failed to create writer: %v", err)
	}

	fill1 := model.Fill{
		OrderID:         "1",
		Instrument:      "BTC-USD",
		Side:            model.Buy,
		Quantity:        1.5,
		FillPrice:       50000.0,
		TransactionCost: 10.0,
		Timestamp:       time.Now().Round(time.Millisecond),
	}
	fill2 := model.Fill{
		OrderID:         "2",
		Instrument:      "BTC-USD",
		Side:            model.Sell,
		Quantity:        0.5,
		FillPrice:       51000.0,
		TransactionCost: 5.0,
		Timestamp:       time.Now().Round(time.Millisecond),
	}

	if err := w.Append(fill1); err != nil {
		t.Fatalf("Failed to append fill1: %v", err)
	}
	if err := w.Append(fill2); err != nil {
		t.Fatalf("Failed to append fill2: %v", err)
	}
	_ = w.Close()

	port, err := RecoverPortfolio(path, 100_000.0)
	if err != nil {
		t.Fatalf("Failed to recover portfolio: %v", err)
	}

	snap := port.Snapshot()
	if snap.TotalFills != 2 {
		t.Errorf("Expected 2 fills, got %d", snap.TotalFills)
	}
	if _, ok := snap.Positions["BTC-USD"]; !ok {
		t.Errorf("Expected BTC-USD position to exist")
	} else if snap.Positions["BTC-USD"].Quantity != 1.0 {
		t.Errorf("Expected BTC-USD quantity 1.0, got %f", snap.Positions["BTC-USD"].Quantity)
	}
}
