package strategy

import (
	"errors"
	"testing"
	"time"

	"github.com/lgreene/huginn/internal/model"
)

// roundTrip drives the contract described in docs/STRATEGY_STATE_DESIGN.md §9:
// Strategy A runs N events, marshals; B is built fresh, restores, runs M more.
// Strategy C runs all N+M from scratch. Final marshaled state of (A then B)
// must equal C's marshaled state.
//
// The function takes a factory so each test produces a fresh strategy three
// times, identically configured. The events slice is split via `splitAt`.
func roundTripStateful[T interface {
	Stateful
	OnFeature(event model.FeatureEvent) []model.Order
}](t *testing.T, factory func() T, events []model.FeatureEvent, splitAt int) {
	t.Helper()
	if splitAt < 0 || splitAt > len(events) {
		t.Fatalf("invalid splitAt=%d for %d events", splitAt, len(events))
	}

	a := factory()
	for _, ev := range events[:splitAt] {
		a.OnFeature(ev)
	}
	blob, err := a.MarshalState()
	if err != nil {
		t.Fatalf("A.MarshalState: %v", err)
	}

	b := factory()
	if err := b.RestoreState(blob); err != nil {
		t.Fatalf("B.RestoreState: %v", err)
	}
	for _, ev := range events[splitAt:] {
		b.OnFeature(ev)
	}
	bFinal, err := b.MarshalState()
	if err != nil {
		t.Fatalf("B.MarshalState: %v", err)
	}

	c := factory()
	for _, ev := range events {
		c.OnFeature(ev)
	}
	cFinal, err := c.MarshalState()
	if err != nil {
		t.Fatalf("C.MarshalState: %v", err)
	}

	if string(bFinal) != string(cFinal) {
		t.Fatalf("split-vs-continuous state diverged\n  split-then-restore: %s\n  continuous:         %s", bFinal, cFinal)
	}
}

func obiEvents(n int, threshold float64) []model.FeatureEvent {
	events := make([]model.FeatureEvent, n)
	t0 := time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC)
	for i := range events {
		// Alternate +0.9 / -0.9 OBI to ping-pong positions through the threshold.
		obi := 0.9
		if i%2 == 1 {
			obi = -0.9
		}
		_ = threshold
		events[i] = model.FeatureEvent{
			EventTime:  t0.Add(time.Duration(i) * time.Second),
			Instrument: "BTC-USD",
			Values:     map[string]float64{"obi": obi},
		}
	}
	return events
}

func TestOBIThreshold_RoundTripStateful(t *testing.T) {
	t.Parallel()
	factory := func() *OBIThreshold { return NewOBIThreshold(0.7, 0.01, 10.0) }
	roundTripStateful(t, factory, obiEvents(20, 0.7), 7)
}

func TestVPINBreakout_RoundTripStateful(t *testing.T) {
	t.Parallel()
	factory := func() *VPINBreakout { return NewVPINBreakout(0.5, 0.01, 500*time.Millisecond) }
	t0 := time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC)
	events := make([]model.FeatureEvent, 12)
	for i := range events {
		events[i] = model.FeatureEvent{
			EventTime:  t0.Add(time.Duration(i) * 200 * time.Millisecond),
			Instrument: "BTC-USD",
			Values:     map[string]float64{"vpin": 0.7},
		}
	}
	roundTripStateful(t, factory, events, 5)
}

func TestVWAPDeviation_RoundTripStateful(t *testing.T) {
	t.Parallel()
	factory := func() *VWAPDeviation { return NewVWAPDeviation(0.001, 0.01, 10.0) }
	t0 := time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC)
	events := make([]model.FeatureEvent, 15)
	for i := range events {
		price := 100.0
		if i%2 == 0 {
			price = 100.5 // > VWAP -> sell
		} else {
			price = 99.5 // < VWAP -> buy
		}
		events[i] = model.FeatureEvent{
			EventTime:  t0.Add(time.Duration(i) * time.Second),
			Instrument: "BTC-USD",
			Values:     map[string]float64{"vwap": 100.0, "microPrice": price},
		}
	}
	roundTripStateful(t, factory, events, 6)
}

func TestEMACrossover_RoundTripStateful(t *testing.T) {
	t.Parallel()
	factory := func() *EMACrossover { return NewEMACrossover(3, 7, 0.01, 10.0) }
	t0 := time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC)
	// Price walk that produces deterministic crossovers.
	prices := []float64{100, 101, 102, 101, 100, 99, 100, 101, 102, 103, 104, 103, 102, 101, 100, 99, 98, 97, 98, 99}
	events := make([]model.FeatureEvent, len(prices))
	for i, p := range prices {
		events[i] = model.FeatureEvent{
			EventTime:  t0.Add(time.Duration(i) * time.Second),
			Instrument: "BTC-USD",
			Values:     map[string]float64{"microPrice": p},
		}
	}
	roundTripStateful(t, factory, events, 11)
}

func TestRestoreState_VersionMismatch(t *testing.T) {
	t.Parallel()
	s := NewOBIThreshold(0.7, 0.01, 10.0)
	bad := []byte(`{"version":999,"fields":{}}`)
	if err := s.RestoreState(bad); !errors.Is(err, ErrStateVersionMismatch) {
		t.Fatalf("expected ErrStateVersionMismatch, got %v", err)
	}
}

func TestRestoreState_EmptyData(t *testing.T) {
	t.Parallel()
	s := NewOBIThreshold(0.7, 0.01, 10.0)
	if err := s.RestoreState(nil); !errors.Is(err, ErrStateEmpty) {
		t.Fatalf("expected ErrStateEmpty, got %v", err)
	}
}
