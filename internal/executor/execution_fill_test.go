package executor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lgreene03/huginn/internal/journal"
	"github.com/lgreene03/huginn/internal/model"
	"github.com/lgreene03/huginn/internal/portfolio"
	"github.com/lgreene03/huginn/internal/strategy"
)

// TestOnExecutionFill_JournalsFillAndStrategyState asserts that a live
// execution fill (the path every Sleipnir fill takes through the fills
// consumer) is applied to the book, journaled with its ExecutionID, and
// triggers strategy-state persistence.
//
// This assertion used to live in internal/server/mock_fill_test.go, where it
// was reached through the /api/fills/mock endpoint. That endpoint was removed
// with huginn's retired web app (decision D6, goal G0.16), so the guard now
// calls OnExecutionFill directly and keeps covering the real fill path.
func TestOnExecutionFill_JournalsFillAndStrategyState(t *testing.T) {
	const initialCash = 100_000.0

	journalPath := filepath.Join(t.TempDir(), "journal.jsonl")
	jw, err := journal.NewJSONLWriter(journalPath)
	if err != nil {
		t.Fatalf("NewJSONLWriter: %v", err)
	}
	t.Cleanup(func() { _ = jw.Close() })

	port := portfolio.New(initialCash)
	obi := strategy.NewOBIThreshold(0.7, 0.01, 10)
	// strategyKey "obi" enables strategy-state persistence on each fill.
	exec := New(obi, port, jw, nil, Config{}, false, nil, "obi")

	exec.OnExecutionFill(context.Background(), model.Fill{
		OrderID:         "order-1",
		ExecutionID:     "sleipnir-exec-1",
		Instrument:      "BTC-USD",
		Side:            model.Buy,
		Quantity:        1,
		FillPrice:       100,
		TransactionCost: 0.05,
		Timestamp:       time.Now(),
	})

	// A BUY spends cash, so the book must reflect the fill.
	if snap := port.Snapshot(); snap.Cash >= initialCash {
		t.Errorf("cash = %.4f, want < %.1f (buy should debit cash)", snap.Cash, initialCash)
	}

	data, err := os.ReadFile(journalPath)
	if err != nil {
		t.Fatalf("read journal: %v", err)
	}
	contents := string(data)

	// The fill itself must be journaled with its ExecutionID, so it is
	// deduplicable on replay and carries provenance.
	if !strings.Contains(contents, "sleipnir-exec-1") {
		t.Errorf("journal missing fill ExecutionID; contents=%q", contents)
	}
	// OnExecutionFill must also persist strategy state.
	if !strings.Contains(contents, "strategy_state") {
		t.Errorf("journal missing strategy_state record; contents=%q", contents)
	}
}
