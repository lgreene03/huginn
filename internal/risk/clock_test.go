package risk

import (
	"testing"
	"time"

	"github.com/lgreene03/huginn/internal/config"
	"github.com/lgreene03/huginn/internal/model"
)

// clockFunc adapts a plain function to Clock, for tests that move time by hand.
// It ignores observed event times, as the wall clock does.
type clockFunc func() time.Time

func (f clockFunc) Now() time.Time  { return f() }
func (clockFunc) Observe(time.Time) {}

// newManagerWithClock builds a manager on a hand-driven clock.
func newManagerWithClock(cfg config.RiskConfig, initialCash float64, now func() time.Time) *Manager {
	return NewManager(cfg, initialCash, clockFunc(now))
}

// TestEventClock_DailyWindowRollsOnEventTime shows a replay's daily loss window
// following the events: the limit binds within an event day and resets at the
// next UTC midnight in the data, however little wall time has passed. Total
// value is held at 100,000 so only the daily loss check can reject.
func TestEventClock_DailyWindowRollsOnEventTime(t *testing.T) {
	t.Parallel()
	m := NewManager(baseCfg(), 100_000, NewEventClock()) // 5,000 daily limit
	fill := model.Fill{Side: model.Buy, Quantity: 0.1, FillPrice: 100, Instrument: "BTC-USD"}
	day1 := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)

	// The first evaluation opens the replay's first day at a baseline of 0.
	m.OnFeatureSeen(day1, 0)
	if !m.Evaluate(fill, baseSnap(0, 100_000)) {
		t.Fatal("day 1: the first fill of a fresh replay should pass")
	}
	if !m.Evaluate(fill, baseSnap(-4_000, 100_000)) {
		t.Fatal("day 1: -4,000 is inside the 5,000 limit and should pass")
	}
	if m.Evaluate(fill, baseSnap(-5_500, 100_000)) {
		t.Fatal("day 1: -5,500 breaches the 5,000 limit and should be rejected")
	}

	// The next event is a minute past midnight: a new day in the data.
	m.OnFeatureSeen(day1.Add(14*time.Hour+time.Minute), 0)
	if !m.Evaluate(fill, baseSnap(-5_500, 100_000)) {
		t.Fatal("day 2: the window should have rolled on event time, resetting the intraday loss to 0")
	}
	if m.Evaluate(fill, baseSnap(-11_000, 100_000)) {
		t.Fatal("day 2: a further -5,500 within the new day should be rejected")
	}
}

// TestEventClock_NeverRunsBackwards pins that a late, out-of-order event cannot
// move a replay's clock back into a day that has already closed.
func TestEventClock_NeverRunsBackwards(t *testing.T) {
	t.Parallel()
	c := NewEventClock()
	if !c.Now().IsZero() {
		t.Fatalf("a fresh event clock reads %v, want the zero time", c.Now())
	}
	later := time.Date(2026, 6, 2, 0, 1, 0, 0, time.UTC)
	c.Observe(later)
	c.Observe(later.Add(-2 * time.Minute)) // a straggler from the previous day
	if got := c.Now(); !got.Equal(later) {
		t.Fatalf("clock reads %v after a late event, want %v", got, later)
	}
}

// TestWallClock_IgnoresEventTime pins the live side: event timestamps, however
// far ahead, do not roll a live manager's daily loss window. Only the host's
// UTC midnight does, so a producer stamping events in the future cannot reset
// the day's loss allowance.
func TestWallClock_IgnoresEventTime(t *testing.T) {
	t.Parallel()
	m := NewManager(baseCfg(), 100_000, WallClock())
	fill := model.Fill{Side: model.Buy, Quantity: 0.1, FillPrice: 100, Instrument: "BTC-USD"}

	if m.Evaluate(fill, baseSnap(-5_500, 100_000)) {
		t.Fatal("-5,500 breaches the 5,000 limit and should be rejected")
	}
	m.OnFeatureSeen(time.Now().Add(72*time.Hour), 0)
	if m.Evaluate(fill, baseSnap(-5_500, 100_000)) {
		t.Fatal("an event stamped three days ahead rolled a live manager's daily window")
	}
}

// TestNewManager_RequiresClock pins that there is no implicit default clock.
func TestNewManager_RequiresClock(t *testing.T) {
	t.Parallel()
	defer func() {
		if recover() == nil {
			t.Fatal("NewManager with a nil Clock should panic")
		}
	}()
	NewManager(baseCfg(), 100_000, nil)
}
