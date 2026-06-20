package risk

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/lgreene03/huginn/internal/config"
	"github.com/lgreene03/huginn/internal/model"
	"github.com/lgreene03/huginn/internal/portfolio"
)

func baseCfg() config.RiskConfig {
	return config.RiskConfig{
		MaxDrawdownPct:    0.10,
		DailyLossLimit:    5000.0,
		PositionLimitHard: 200_000.0,
	}
}

func baseSnap(realizedPnL, totalValue float64) portfolio.Snapshot {
	return portfolio.Snapshot{
		Timestamp:   time.Now(),
		Cash:        100_000.0,
		Positions:   map[string]portfolio.Position{},
		RealizedPnL: realizedPnL,
		TotalValue:  totalValue,
	}
}

func TestDailyLossLimit_UsesIntradayWindow(t *testing.T) {
	t.Parallel()
	// Pin "now" to noon on day-1.
	day1Noon := time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC)
	clock := day1Noon
	m := newManagerWithClock(baseCfg(), 100_000, func() time.Time { return clock })

	// Pre-existing realized PnL of -4000 (within today's allowance).
	fill := model.Fill{Side: model.Buy, Quantity: 0.1, FillPrice: 100, Instrument: "BTC-USD"}
	if !m.Evaluate(fill, baseSnap(-4_000, 100_000)) {
		t.Fatalf("expected pass: -4000 intraday < -5000 limit")
	}

	// Push intraday loss to -5500: should reject.
	if m.Evaluate(fill, baseSnap(-5_500, 100_000)) {
		t.Fatalf("expected reject: -5500 intraday breaches -5000")
	}

	// Roll the clock to next-day midnight; baseline rolls forward, so the
	// same realized PnL becomes intraday=0 and the trade should pass again.
	clock = day1Noon.Add(24 * time.Hour)
	if !m.Evaluate(fill, baseSnap(-5_500, 100_000)) {
		t.Fatalf("expected pass after UTC-day rollover (intraday should reset to 0)")
	}
}

func TestPeakValue_PersistsAcrossRestart(t *testing.T) {
	t.Parallel()
	clock := time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC)
	a := newManagerWithClock(baseCfg(), 100_000, func() time.Time { return clock })

	// Drive peakValue up to 130k via Evaluate's high-water mark.
	fill := model.Fill{Side: model.Buy, Quantity: 0.001, FillPrice: 1, Instrument: "BTC-USD"}
	if !a.Evaluate(fill, baseSnap(0, 130_000)) {
		t.Fatalf("expected pass while ratcheting peak")
	}

	blob, err := a.MarshalState()
	if err != nil {
		t.Fatalf("MarshalState: %v", err)
	}

	// Fresh manager with initialCash=100k would otherwise reset peakValue to
	// 100k and not detect a drawdown from the prior peak.
	b := newManagerWithClock(baseCfg(), 100_000, func() time.Time { return clock })
	if err := b.RestoreState(blob); err != nil {
		t.Fatalf("RestoreState: %v", err)
	}
	// At total_value=110k vs restored peak=130k, drawdown=15.4% > 10% → reject.
	if b.Evaluate(fill, baseSnap(0, 110_000)) {
		t.Fatalf("expected drawdown trip against restored peakValue=130k")
	}
}

func TestPerInstrumentPositionLimit(t *testing.T) {
	t.Parallel()
	cfg := baseCfg()
	cfg.PositionLimitPerInstrument = map[string]float64{
		"BTC-USD": 50_000.0, // tighter than the 200k gross
	}
	clock := time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC)
	m := newManagerWithClock(cfg, 100_000, func() time.Time { return clock })

	// 60k notional in BTC-USD breaches the per-instrument 50k cap.
	btc := model.Fill{Side: model.Buy, Quantity: 1.2, FillPrice: 50_000, Instrument: "BTC-USD"}
	if m.Evaluate(btc, baseSnap(0, 100_000)) {
		t.Fatalf("expected reject: 60k > 50k per-instrument cap")
	}
	// ETH-USD has no override → falls back to gross (200k), 60k notional passes.
	eth := model.Fill{Side: model.Buy, Quantity: 20, FillPrice: 3_000, Instrument: "ETH-USD"}
	if !m.Evaluate(eth, baseSnap(0, 100_000)) {
		t.Fatalf("expected pass on ETH-USD (60k < 200k gross)")
	}
}

func TestStalenessWatchdog_HaltsAndAutoResumes(t *testing.T) {
	t.Parallel()
	clock := time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC)
	cfg := baseCfg()
	cfg.StalenessTimeout = 2 * time.Second
	cfg.AutoResumeAfterStaleness = true

	m := newManagerWithClock(cfg, 100_000, func() time.Time { return clock })
	m.OnFeatureSeen(clock, 0)

	// Advance the clock past the threshold and run one watchdog tick manually
	// rather than spinning the timer — keeps the test deterministic.
	clock = clock.Add(10 * time.Second)

	// Inline check duplicates the watchdog body so we don't sleep for ticks.
	m.mu.Lock()
	if m.now().Sub(m.lastFeatureEventTime) > m.config.StalenessTimeout {
		m.halted = true
		m.haltReason = HaltStaleness
	}
	m.mu.Unlock()

	if !m.IsHalted() || m.HaltReason() != HaltStaleness {
		t.Fatalf("expected staleness halt, got halted=%v reason=%s", m.IsHalted(), m.HaltReason())
	}

	// Fresh feature should auto-clear.
	m.OnFeatureSeen(clock, 0)
	if m.IsHalted() {
		t.Fatalf("expected auto-resume on fresh feature event")
	}
}

func TestStalenessWatchdog_ManualHaltNotAutoCleared(t *testing.T) {
	t.Parallel()
	clock := time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC)
	cfg := baseCfg()
	cfg.StalenessTimeout = 1 * time.Second
	cfg.AutoResumeAfterStaleness = true

	m := newManagerWithClock(cfg, 100_000, func() time.Time { return clock })
	m.Halt() // manual breaker

	m.OnFeatureSeen(clock.Add(time.Second), 0)
	if !m.IsHalted() {
		t.Fatalf("manual halt must not be cleared by a fresh feature event")
	}
	if m.HaltReason() != HaltManual {
		t.Fatalf("expected HaltManual to persist, got %s", m.HaltReason())
	}
}

func TestStalenessWatchdog_DisabledWhenTimeoutZero(t *testing.T) {
	t.Parallel()
	// Sanity: RunStalenessWatchdog returns immediately when timeout is zero.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	m := NewManager(baseCfg(), 100_000) // StalenessTimeout == 0
	m.RunStalenessWatchdog(ctx)         // must return without blocking
}

func TestRestoreState_VersionMismatch(t *testing.T) {
	t.Parallel()
	m := NewManager(baseCfg(), 100_000)
	bad := []byte(`{"version":999,"fields":{}}`)
	if err := m.RestoreState(bad); !errors.Is(err, ErrRiskStateVersionMismatch) {
		t.Fatalf("expected ErrRiskStateVersionMismatch, got %v", err)
	}
}

func TestRestoreState_EmptyIsNoOp(t *testing.T) {
	t.Parallel()
	m := NewManager(baseCfg(), 100_000)
	before := m.peakValue
	if err := m.RestoreState(nil); err != nil {
		t.Fatalf("nil restore should be a no-op: %v", err)
	}
	if m.peakValue != before {
		t.Fatalf("peakValue mutated on nil restore: was %f now %f", before, m.peakValue)
	}
}
