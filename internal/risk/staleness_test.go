package risk

import (
	"testing"
	"time"
)

// The staleness watchdog exists to stop trading when the feature feed dies. It
// must measure when features ARRIVE. It used to measure the features' own
// EventTime, and norse-stack's obi-bridge stamps events with the close time of
// the still-open 5-minute candle: up to five minutes in the future. A dead feed
// then looked fresh until wall-clock overtook that future stamp, so huginn kept
// trading for up to five minutes past its configured timeout.

// staleTimeout is the documented production default (docs/risk-model.md).
const staleTimeout = 60 * time.Second

func staleManager(clock *time.Time) *Manager {
	cfg := baseCfg()
	cfg.StalenessTimeout = staleTimeout
	return newManagerWithClock(cfg, 100_000, func() time.Time { return *clock })
}

func check(m *Manager) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.checkStalenessLocked()
}

func TestStaleness_FutureStampedEventCannotBlindTheWatchdog(t *testing.T) {
	t.Parallel()
	clock := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	m := staleManager(&clock)

	// One event stamped five minutes ahead, then the feed dies.
	m.OnFeatureSeen(clock.Add(5*time.Minute), 0)
	clock = clock.Add(61 * time.Second)

	if !check(m) || m.HaltReason() != HaltStaleness {
		// The old event-time comparison gave a gap of 61s - 5m = -3m59s here.
		t.Fatalf("a feed silent for 61s must halt a 60s watchdog whatever its timestamps; halted=%v", m.IsHalted())
	}
}

func TestStaleness_HonestEventsBehaveExactlyAsBefore(t *testing.T) {
	t.Parallel()
	clock := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	m := staleManager(&clock)

	for i := 0; i < 20; i++ {
		m.OnFeatureSeen(clock, 0)
		clock = clock.Add(10 * time.Second)
		if check(m) {
			t.Fatalf("events every 10s must never trip a 60s watchdog (round %d)", i)
		}
	}
	clock = clock.Add(51 * time.Second) // 61s since the last arrival
	if !check(m) {
		t.Fatalf("expected a halt once 61s pass with no arrival")
	}
}

func TestStaleness_ArmedImmediatelyAfterRestore(t *testing.T) {
	t.Parallel()
	clock := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	m := staleManager(&clock)
	state, err := m.MarshalState()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}

	// Restart, possibly with open positions, into a feed that never comes back.
	restored := staleManager(&clock)
	if err := restored.RestoreState(state); err != nil {
		t.Fatalf("restore: %v", err)
	}
	clock = clock.Add(30 * time.Second)
	if check(restored) {
		t.Fatalf("must not halt before the timeout has passed since restore")
	}
	clock = clock.Add(31 * time.Second)
	if !check(restored) {
		t.Fatalf("no feature within 61s of a restore must halt; the watchdog must not wait for a first event")
	}
}

func TestStaleness_FreshStartWaitsForAFirstEvent(t *testing.T) {
	t.Parallel()
	// Unchanged behaviour: with no state and no features, no strategy can trade,
	// so there is nothing to halt.
	clock := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	m := staleManager(&clock)
	clock = clock.Add(10 * time.Minute)
	if check(m) {
		t.Fatalf("a fresh manager that has seen no feature must not halt")
	}
}
