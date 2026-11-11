package metrics

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// TestEntriesBlocked_LabelSetIsClosedAndExportedAtZero pins the reason label
// set: exactly these six series, in gate order, all present from the first
// scrape. Adding a reason must be a deliberate change to this list.
func TestEntriesBlocked_LabelSetIsClosedAndExportedAtZero(t *testing.T) {
	want := []BlockReason{
		"below_threshold",
		"position_open",
		"max_position",
		"zero_size",
		"cost_hurdle",
		"risk_rejected",
	}
	if len(BlockReasons) != len(want) {
		t.Fatalf("BlockReasons has %d entries, want %d: %v", len(BlockReasons), len(want), BlockReasons)
	}
	for i := range want {
		if BlockReasons[i] != want[i] {
			t.Errorf("BlockReasons[%d] = %q, want %q", i, BlockReasons[i], want[i])
		}
	}
	if got := testutil.CollectAndCount(EntriesBlockedTotal); got != len(want) {
		t.Fatalf("huginn_entries_blocked_total exports %d series, want %d", got, len(want))
	}
}

func TestEntryBlocked_IncrementsOnlyItsReason(t *testing.T) {
	before := make(map[BlockReason]float64)
	for _, r := range BlockReasons {
		before[r] = testutil.ToFloat64(EntriesBlockedTotal.WithLabelValues(string(r)))
	}

	EntryBlocked(BlockPositionOpen)

	for _, r := range BlockReasons {
		want := 0.0
		if r == BlockPositionOpen {
			want = 1
		}
		if got := testutil.ToFloat64(EntriesBlockedTotal.WithLabelValues(string(r))) - before[r]; got != want {
			t.Errorf("reason %q moved by %v, want %v", r, got, want)
		}
	}
}

func TestOrderIntentIssued_SetsUnixSeconds(t *testing.T) {
	at := time.Date(2026, 6, 22, 14, 48, 45, 500_000_000, time.UTC)
	OrderIntentIssued(at)
	want := float64(at.Unix()) + 0.5
	if got := testutil.ToFloat64(LastOrderIntentTimestampSeconds); got != want {
		t.Fatalf("huginn_last_order_intent_timestamp_seconds = %v, want %v", got, want)
	}
}
