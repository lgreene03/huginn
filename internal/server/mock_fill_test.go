package server

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lgreene03/huginn/internal/executor"
	"github.com/lgreene03/huginn/internal/journal"
	"github.com/lgreene03/huginn/internal/portfolio"
	"github.com/lgreene03/huginn/internal/strategy"
)

// TestMockFillHandler_JournalsThroughExecutor asserts that the operator-only
// /api/fills/mock endpoint applies the fill through the same executor path a
// real Sleipnir fill takes — so the mock fill is journaled and triggers
// strategy-state persistence — rather than mutating the portfolio in isolation.
//
// Regression guard for the audit finding: before this, a mock fill landed in
// the portfolio but never in the journal, so it was lost on restart and the
// journal diverged from the book.
func TestMockFillHandler_JournalsThroughExecutor(t *testing.T) {
	const initialCash = 100_000.0

	journalPath := filepath.Join(t.TempDir(), "journal.jsonl")
	jw, err := journal.NewJSONLWriter(journalPath)
	if err != nil {
		t.Fatalf("NewJSONLWriter: %v", err)
	}

	port := portfolio.New(initialCash)
	obi := strategy.NewOBIThreshold(0.7, 0.01, 10)
	// strategyKey "obi" enables strategy-state persistence on each fill.
	exec := executor.New(obi, port, jw, nil, executor.Config{}, false, nil, "obi")

	// nil riskMgr → the handler skips risk evaluation, isolating the apply path.
	srv := New(":0", port, nil, exec)

	body := bytes.NewBufferString(`{"instrument":"BTC-USD","side":"BUY","quantity":1,"price":100}`)
	req := httptest.NewRequest(http.MethodPost, "/api/fills/mock", body)
	rec := httptest.NewRecorder()

	srv.mockFillHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	// A BUY spends cash, so the book must reflect the fill.
	if snap := port.Snapshot(); snap.Cash >= initialCash {
		t.Errorf("cash = %.4f, want < %.1f (buy should debit cash)", snap.Cash, initialCash)
	}

	data, err := os.ReadFile(journalPath)
	if err != nil {
		t.Fatalf("read journal: %v", err)
	}
	contents := string(data)

	// The fill itself must be journaled, tagged with the mock ExecutionID so it
	// is deduplicable and carries provenance.
	if !strings.Contains(contents, "mock-exec-") {
		t.Errorf("journal missing mock fill ExecutionID; contents=%q", contents)
	}
	// Routing through OnExecutionFill must also persist strategy state.
	if !strings.Contains(contents, "strategy_state") {
		t.Errorf("journal missing strategy_state record; contents=%q", contents)
	}
}
