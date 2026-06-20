package kafka

import (
	"testing"
	"time"
)

func TestProgress_FreshThenStale(t *testing.T) {
	p := NewProgress()

	// A freshly seeded tracker is not stale within a generous window.
	if p.Stale(time.Hour) {
		t.Fatal("freshly seeded progress should not be stale")
	}

	// Backdate the last-advance well beyond a short window: now stale.
	p.lastUnixNano.Store(time.Now().Add(-10 * time.Second).UnixNano())
	if !p.Stale(time.Second) {
		t.Fatal("progress 10s old should be stale against a 1s window")
	}

	// Mark advances the clock; no longer stale.
	p.Mark()
	if p.Stale(time.Second) {
		t.Fatal("progress should be fresh immediately after Mark")
	}
}

func TestProgress_DisabledWindow(t *testing.T) {
	p := NewProgress()
	p.lastUnixNano.Store(time.Now().Add(-time.Hour).UnixNano())
	// A non-positive window disables the check (feature off).
	if p.Stale(0) {
		t.Fatal("zero window should disable staleness")
	}
	if p.Stale(-time.Second) {
		t.Fatal("negative window should disable staleness")
	}
}

func TestProgress_NilSafe(t *testing.T) {
	var p *Progress
	// Methods must be safe on a nil receiver (consumer built without progress).
	p.Mark()
	if p.Stale(time.Second) {
		t.Fatal("nil progress should never be stale")
	}
	if !p.Last().IsZero() {
		t.Fatal("nil progress Last should be zero time")
	}
}
