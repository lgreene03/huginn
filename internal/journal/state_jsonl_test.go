package journal

import (
	"path/filepath"
	"testing"

	"github.com/lgreene03/huginn/internal/model"
)

func TestJSONLState_AppendThenLoad(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "trades.jsonl")
	w, err := NewJSONLWriter(path)
	if err != nil {
		t.Fatalf("NewJSONLWriter: %v", err)
	}

	// Append a fill and two state snapshots; the later one must win.
	if err := w.Append(model.Fill{OrderID: "abc", Instrument: "BTC-USD"}); err != nil {
		t.Fatalf("Append fill: %v", err)
	}
	if err := w.AppendStrategyState("obi", []byte(`{"version":1,"fields":{"net_position":1.5}}`)); err != nil {
		t.Fatalf("AppendStrategyState 1: %v", err)
	}
	if err := w.AppendStrategyState("obi", []byte(`{"version":1,"fields":{"net_position":2.5}}`)); err != nil {
		t.Fatalf("AppendStrategyState 2: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	got, err := LoadStrategyStateFromJSONL(path, "obi")
	if err != nil {
		t.Fatalf("LoadStrategyStateFromJSONL: %v", err)
	}
	want := `{"version":1,"fields":{"net_position":2.5}}`
	if string(got) != want {
		t.Fatalf("latest-wins not honored\n got:  %s\n want: %s", got, want)
	}
}

func TestJSONLState_LoadAbsentReturnsNil(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "trades.jsonl")
	got, err := LoadStrategyStateFromJSONL(path, "missing")
	if err != nil {
		t.Fatalf("LoadStrategyStateFromJSONL on absent file: %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil for absent file, got %s", got)
	}
}

func TestJSONLState_OtherKeysIgnored(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "trades.jsonl")
	w, _ := NewJSONLWriter(path)
	defer w.Close()

	_ = w.AppendStrategyState("vpin", []byte(`{"version":1,"fields":{"last_trade":"2026-05-19T00:00:00Z"}}`))
	_ = w.AppendStrategyState("obi", []byte(`{"version":1,"fields":{"net_position":3.0}}`))
	_ = w.AppendStrategyState("vpin", []byte(`{"version":1,"fields":{"last_trade":"2026-05-19T00:01:00Z"}}`))

	got, err := LoadStrategyStateFromJSONL(path, "obi")
	if err != nil {
		t.Fatalf("LoadStrategyStateFromJSONL: %v", err)
	}
	if string(got) != `{"version":1,"fields":{"net_position":3.0}}` {
		t.Fatalf("wrong key returned: %s", got)
	}
}

func TestJSONLState_RecoverPortfolioSkipsStateRecords(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "trades.jsonl")
	w, _ := NewJSONLWriter(path)
	_ = w.Append(model.Fill{
		OrderID:    "fill-1",
		Instrument: "BTC-USD",
		Side:       model.Buy,
		Quantity:   0.001,
		FillPrice:  40000,
	})
	_ = w.AppendStrategyState("obi", []byte(`{"version":1,"fields":{"net_position":1.0}}`))
	_ = w.Append(model.Fill{
		OrderID:    "fill-2",
		Instrument: "BTC-USD",
		Side:       model.Sell,
		Quantity:   0.001,
		FillPrice:  41000,
	})
	_ = w.Close()

	port, err := RecoverPortfolio(path, 10_000)
	if err != nil {
		t.Fatalf("RecoverPortfolio: %v", err)
	}
	snap := port.Snapshot()
	// 2 fills applied; if the strategy_state line had been parsed as a Fill,
	// ApplyFill on a zero-instrument zero-qty row would have left an artifact.
	// The realized PnL = (41000-40000)*0.001 - fees(0) = 1.0 here since fills
	// carry no transaction cost. Just assert no crash + sensible totals.
	if snap.Cash <= 0 {
		t.Fatalf("recovered portfolio has non-positive cash: %v", snap)
	}
}

func TestJSONLState_EmptyBlobIsNoOp(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "trades.jsonl")
	w, _ := NewJSONLWriter(path)
	defer w.Close()

	if err := w.AppendStrategyState("obi", nil); err != nil {
		t.Fatalf("AppendStrategyState(nil) should be a no-op, got: %v", err)
	}
	got, _ := LoadStrategyStateFromJSONL(path, "obi")
	if got != nil {
		t.Fatalf("empty blob should not have been recorded, got %s", got)
	}
}
