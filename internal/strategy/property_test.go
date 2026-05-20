package strategy

import (
	"math"
	"math/rand"
	"testing"
	"testing/quick"
	"time"

	"github.com/lgreene03/huginn/internal/model"
)

// Property-based tests covering invariants that must hold for every bundled
// strategy regardless of input. testing/quick generates random inputs; each
// strategy's invariants are stated as predicates returning true on success.
//
// Phase 2 of docs/ROADMAP.md committed to these. They catch fuzzed-sequence
// bugs that the hand-rolled tests in strategy_test.go can't surface.

// arbitraryFeatureValues constructs a sane FeatureEvent for the given
// strategy from a fuzzed payload. We bound the random inputs to ranges
// that exercise each strategy's signal logic without producing NaN/Inf.
func obiEvent(rng *rand.Rand, ts time.Time) model.FeatureEvent {
	return model.FeatureEvent{
		EventTime:  ts,
		Instrument: "BTC-USD",
		Values:     map[string]float64{"obi": rng.Float64()*2 - 1}, // [-1, +1]
	}
}

func vpinEvent(rng *rand.Rand, ts time.Time) model.FeatureEvent {
	return model.FeatureEvent{
		EventTime:  ts,
		Instrument: "BTC-USD",
		Values:     map[string]float64{"vpin": rng.Float64() * 2}, // [0, 2]
	}
}

func vwapEvent(rng *rand.Rand, ts time.Time) model.FeatureEvent {
	// price is VWAP * (1 + small deviation)
	vwap := 100.0 + rng.Float64()*10
	dev := rng.Float64()*0.01 - 0.005 // ±50 bps
	return model.FeatureEvent{
		EventTime:  ts,
		Instrument: "BTC-USD",
		Values: map[string]float64{
			"vwap":       vwap,
			"microPrice": vwap * (1 + dev),
		},
	}
}

func emaEvent(rng *rand.Rand, ts time.Time, lastPrice *float64) model.FeatureEvent {
	// Random-walk price so EMAs see a realistic stream.
	step := rng.NormFloat64() * 0.5
	*lastPrice = math.Max(1, *lastPrice+step)
	return model.FeatureEvent{
		EventTime:  ts,
		Instrument: "BTC-USD",
		Values:     map[string]float64{"microPrice": *lastPrice},
	}
}

// runProperty drives a strategy with N random events and asserts the
// invariants on every emitted order.
func runProperty(t *testing.T, mkStrategy func() Strategy, mkEvent func(*rand.Rand, time.Time) model.FeatureEvent, maxPosition float64) {
	t.Helper()
	f := func(seed int64) bool {
		rng := rand.New(rand.NewSource(seed))
		s := mkStrategy()
		t0 := time.Date(2026, 5, 19, 0, 0, 0, 0, time.UTC)

		for i := 0; i < 100; i++ {
			ts := t0.Add(time.Duration(i) * time.Second)
			ev := mkEvent(rng, ts)
			orders := s.OnFeature(ev)
			for _, o := range orders {
				// Invariant 1: quantity is strictly positive.
				if o.Quantity <= 0 {
					t.Logf("seed=%d ev#%d emitted Quantity=%.8f", seed, i, o.Quantity)
					return false
				}
				// Invariant 2: side string is canonical.
				ss := o.Side.String()
				if ss != "BUY" && ss != "SELL" {
					t.Logf("seed=%d ev#%d emitted non-canonical side %q", seed, i, ss)
					return false
				}
				// Invariant 3: order timestamp == event time (event-time
				// semantics — no wall-clock reads inside OnFeature).
				if !o.Timestamp.Equal(ev.EventTime) {
					t.Logf("seed=%d ev#%d order.Timestamp=%v != event.EventTime=%v",
						seed, i, o.Timestamp, ev.EventTime)
					return false
				}
				// Invariant 4: instrument echoes input.
				if o.Instrument != ev.Instrument {
					t.Logf("seed=%d ev#%d order.Instrument=%q != event.Instrument=%q",
						seed, i, o.Instrument, ev.Instrument)
					return false
				}
				// Invariant 5: reason is non-empty (UI + journal rely on it).
				if o.Reason == "" {
					t.Logf("seed=%d ev#%d empty Reason", seed, i)
					return false
				}
			}
		}
		_ = maxPosition // hook for caller-specific extensions
		return true
	}

	if err := quick.Check(f, &quick.Config{MaxCount: 30}); err != nil {
		t.Fatalf("property violated: %v", err)
	}
}

func TestProperty_OBIThreshold(t *testing.T) {
	t.Parallel()
	runProperty(t,
		func() Strategy { return NewOBIThreshold(0.7, 0.01, 0.10) },
		obiEvent,
		0.10,
	)
}

func TestProperty_VPINBreakout(t *testing.T) {
	t.Parallel()
	runProperty(t,
		func() Strategy { return NewVPINBreakout(0.5, 0.01, 500*time.Millisecond) },
		vpinEvent,
		0,
	)
}

func TestProperty_VWAPDeviation(t *testing.T) {
	t.Parallel()
	runProperty(t,
		func() Strategy { return NewVWAPDeviation(0.001, 0.01, 0.10) },
		vwapEvent,
		0.10,
	)
}

func TestProperty_EMACrossover(t *testing.T) {
	t.Parallel()
	// EMA needs a price walk, not independent samples. Wrap the generator to
	// carry state across calls per seed.
	f := func(seed int64) bool {
		rng := rand.New(rand.NewSource(seed))
		s := NewEMACrossover(3, 7, 0.01, 0.10)
		t0 := time.Date(2026, 5, 19, 0, 0, 0, 0, time.UTC)
		price := 100.0

		for i := 0; i < 100; i++ {
			ts := t0.Add(time.Duration(i) * time.Second)
			ev := emaEvent(rng, ts, &price)
			orders := s.OnFeature(ev)
			for _, o := range orders {
				if o.Quantity <= 0 {
					t.Logf("seed=%d ev#%d Quantity=%.8f", seed, i, o.Quantity)
					return false
				}
				ss := o.Side.String()
				if ss != "BUY" && ss != "SELL" {
					return false
				}
				if !o.Timestamp.Equal(ev.EventTime) {
					return false
				}
			}
		}
		return true
	}
	if err := quick.Check(f, &quick.Config{MaxCount: 30}); err != nil {
		t.Fatalf("EMA property violated: %v", err)
	}
}

// TestProperty_OBIThrottleHolds checks the position-throttle invariant
// explicitly. OBIThreshold must never move netPosition past ±maxPosition,
// even under adversarial OBI sequences.
func TestProperty_OBIThrottleHolds(t *testing.T) {
	t.Parallel()
	maxPos := 0.05
	orderSize := 0.01
	f := func(seed int64) bool {
		rng := rand.New(rand.NewSource(seed))
		s := NewOBIThreshold(0.5, orderSize, maxPos)
		t0 := time.Date(2026, 5, 19, 0, 0, 0, 0, time.UTC)
		for i := 0; i < 200; i++ {
			ts := t0.Add(time.Duration(i) * time.Second)
			s.OnFeature(obiEvent(rng, ts))
		}
		// netPosition must stay within (-(maxPos+orderSize), +(maxPos+orderSize)).
		// The throttle is checked BEFORE incrementing, so position can step one
		// orderSize beyond the threshold but no further.
		s.mu.Lock()
		got := s.netPosition
		s.mu.Unlock()
		if math.Abs(got) > maxPos+orderSize+1e-9 {
			t.Logf("seed=%d netPosition=%.8f exceeded maxPos+orderSize=%.8f",
				seed, got, maxPos+orderSize)
			return false
		}
		return true
	}
	if err := quick.Check(f, &quick.Config{MaxCount: 30}); err != nil {
		t.Fatalf("OBI throttle violated: %v", err)
	}
}
