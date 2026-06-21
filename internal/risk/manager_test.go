package risk

import (
	"testing"
	"time"

	"github.com/lgreene03/huginn/internal/config"
	"github.com/lgreene03/huginn/internal/model"
	"github.com/lgreene03/huginn/internal/portfolio"
)

func TestRiskManager(t *testing.T) {
	cfg := config.RiskConfig{
		MaxDrawdownPct:    0.10,
		DailyLossLimit:    5000.0,
		PositionLimitHard: 200000.0,
	}
	manager := NewManager(cfg, 100_000.0)

	baseSnap := portfolio.Snapshot{
		Timestamp:   time.Now(),
		Cash:        100_000.0,
		Positions:   make(map[string]portfolio.Position),
		RealizedPnL: 0.0,
		TotalValue:  100_000.0,
	}

	t.Run("Passes when within limits", func(t *testing.T) {
		fill := model.Fill{Side: model.Buy, Quantity: 1.0, FillPrice: 50000.0}
		if !manager.Evaluate(fill, baseSnap) {
			t.Errorf("Expected fill to be approved")
		}
	})

	t.Run("Rejects on Max Drawdown", func(t *testing.T) {
		snap := baseSnap
		snap.TotalValue = 85_000.0 // Below 90k threshold (100k - 10%)
		fill := model.Fill{Side: model.Buy, Quantity: 1.0, FillPrice: 50000.0}
		if manager.Evaluate(fill, snap) {
			t.Errorf("Expected fill to be rejected due to Max Drawdown")
		}
	})

	t.Run("Rejects on Daily Loss Limit", func(t *testing.T) {
		snap := baseSnap
		snap.RealizedPnL = -6000.0 // Below -5000 threshold
		fill := model.Fill{Side: model.Buy, Quantity: 1.0, FillPrice: 50000.0}
		if manager.Evaluate(fill, snap) {
			t.Errorf("Expected fill to be rejected due to Daily Loss Limit")
		}
	})

	t.Run("Rejects on Position Limit Hard", func(t *testing.T) {
		fill := model.Fill{Side: model.Buy, Quantity: 5.0, FillPrice: 50000.0} // 250k > 200k
		if manager.Evaluate(fill, baseSnap) {
			t.Errorf("Expected fill to be rejected due to Position Limit")
		}
	})

	t.Run("Rejects on Manual Circuit Breaker", func(t *testing.T) {
		manager.Halt()
		if !manager.IsHalted() {
			t.Errorf("Expected manager to be halted")
		}

		fill := model.Fill{Side: model.Buy, Quantity: 0.1, FillPrice: 50000.0}
		if manager.Evaluate(fill, baseSnap) {
			t.Errorf("Expected fill to be rejected when manual circuit breaker is active")
		}

		manager.Resume()
		if manager.IsHalted() {
			t.Errorf("Expected manager to be resumed")
		}

		if !manager.Evaluate(fill, baseSnap) {
			t.Errorf("Expected fill to be approved after resetting manual circuit breaker")
		}
	})
}

// TestPositionLimitShortSymmetry verifies the pre-trade position/notional cap
// applies to ABSOLUTE exposure (abs(Quantity)*price), so an over-cap SHORT is
// rejected exactly like an over-cap long, and an in-cap short is approved.
func TestPositionLimitShortSymmetry(t *testing.T) {
	cfg := config.RiskConfig{
		MaxDrawdownPct:    0.10,
		DailyLossLimit:    5000.0,
		PositionLimitHard: 200000.0,
	}

	baseSnap := portfolio.Snapshot{
		Timestamp:   time.Now(),
		Cash:        100_000.0,
		Positions:   make(map[string]portfolio.Position),
		RealizedPnL: 0.0,
		TotalValue:  100_000.0,
	}

	t.Run("Over-cap short from flat is rejected like an over-cap long", func(t *testing.T) {
		m := NewManager(cfg, 100_000.0)
		// 5 * 50000 = 250k > 200k cap. Same magnitude as the long-rejection case.
		buy := model.Fill{Instrument: "BTC", Side: model.Buy, Quantity: 5.0, FillPrice: 50000.0}
		sell := model.Fill{Instrument: "BTC", Side: model.Sell, Quantity: 5.0, FillPrice: 50000.0}
		if m.Evaluate(buy, baseSnap) {
			t.Fatalf("over-cap long should be rejected")
		}
		if m.Evaluate(sell, baseSnap) {
			t.Fatalf("over-cap short should be rejected just like the long")
		}
	})

	t.Run("In-cap short from flat is approved", func(t *testing.T) {
		m := NewManager(cfg, 100_000.0)
		// 3 * 50000 = 150k < 200k cap.
		sell := model.Fill{Instrument: "BTC", Side: model.Sell, Quantity: 3.0, FillPrice: 50000.0}
		if !m.Evaluate(sell, baseSnap) {
			t.Fatalf("in-cap short should be approved")
		}
	})

	t.Run("Adding to an existing short past the cap is rejected", func(t *testing.T) {
		m := NewManager(cfg, 100_000.0)
		snap := baseSnap
		snap.Positions = map[string]portfolio.Position{
			"BTC": {Quantity: -3.0}, // already short 3 BTC
		}
		// Selling 2 more => newQty = -5 => abs 5 * 50000 = 250k > 200k.
		sell := model.Fill{Instrument: "BTC", Side: model.Sell, Quantity: 2.0, FillPrice: 50000.0}
		if m.Evaluate(sell, snap) {
			t.Fatalf("growing a short beyond the absolute cap should be rejected")
		}
	})

	t.Run("Per-instrument cap is symmetric for shorts", func(t *testing.T) {
		perCfg := cfg
		perCfg.PositionLimitPerInstrument = map[string]float64{"BTC": 100_000.0}
		m := NewManager(perCfg, 100_000.0)
		// 3 * 50000 = 150k > 100k per-instrument cap.
		sell := model.Fill{Instrument: "BTC", Side: model.Sell, Quantity: 3.0, FillPrice: 50000.0}
		if m.Evaluate(sell, baseSnap) {
			t.Fatalf("over-cap short should be rejected by the per-instrument cap")
		}
		// 1 * 50000 = 50k < 100k is allowed.
		okSell := model.Fill{Instrument: "BTC", Side: model.Sell, Quantity: 1.0, FillPrice: 50000.0}
		if !m.Evaluate(okSell, baseSnap) {
			t.Fatalf("in-cap short should be approved by the per-instrument cap")
		}
	})
}

func TestSeedFromBaseline(t *testing.T) {
	cfg := config.RiskConfig{MaxDrawdownPct: 0.20, PositionLimitHard: 500_000}
	m := NewManager(cfg, 100_000)

	// Initial peakValue is initialCash (100k).
	if got := m.PeakValue(); got != 100_000 {
		t.Fatalf("expected initial peakValue 100000, got %v", got)
	}
	if got := m.DayStartRealizedPnL(); got != 0 {
		t.Fatalf("expected initial dayStartRealizedPnL 0, got %v", got)
	}

	// Seed from a prior daily snapshot.
	m.SeedFromBaseline(120_000, 500.0)

	if got := m.PeakValue(); got != 120_000 {
		t.Fatalf("expected seeded peakValue 120000, got %v", got)
	}
	if got := m.DayStartRealizedPnL(); got != 500.0 {
		t.Fatalf("expected seeded dayStartRealizedPnL 500, got %v", got)
	}

	// A zero peakValue seed must not overwrite the existing value.
	m.SeedFromBaseline(0, 0)
	if got := m.PeakValue(); got != 120_000 {
		t.Fatalf("zero seed should not overwrite peakValue; got %v", got)
	}

	// dayStartRealizedPnL of zero IS a valid seed (could be start of day).
	if got := m.DayStartRealizedPnL(); got != 0 {
		t.Fatalf("expected dayStartRealizedPnL reset to 0, got %v", got)
	}
}

func TestRecoveryFallback_DrawdownGuard(t *testing.T) {
	// Simulate a restart where prior peak was $120k. Without SeedFromBaseline,
	// the risk manager would misfire because peakValue resets to initialCash.
	// DailyLossLimit is generous (50k) so it doesn't interfere with this test.
	cfg := config.RiskConfig{
		MaxDrawdownPct:    0.20,
		PositionLimitHard: 500_000,
		DailyLossLimit:    50_000,
	}
	initialCash := 100_000.0
	fill := model.Fill{Side: model.Buy, Quantity: 0.1, FillPrice: 50_000, Instrument: "BTC"}
	// RealizedPnL > dayStartRealizedPnL so intraday loss is zero/positive.
	snap := portfolio.Snapshot{TotalValue: 115_000, Cash: 115_000, RealizedPnL: 2_000}

	// Without seeding: peakValue = initialCash ($100k), $115k looks fine.
	m := NewManager(cfg, initialCash)
	if !m.Evaluate(fill, snap) {
		t.Fatal("baseline without seeding should approve fill")
	}

	// With seeding: peakValue = $120k; $115k is 4.2% below peak — within 20% limit.
	m2 := NewManager(cfg, initialCash)
	m2.SeedFromBaseline(120_000, 1_000)
	if !m2.Evaluate(fill, snap) {
		t.Fatal("seeded manager should approve fill at 4.2% drawdown from peak")
	}

	// At $94k (21.7% below $120k peak) the seeded manager should trip the breaker.
	snap2 := portfolio.Snapshot{TotalValue: 94_000, Cash: 94_000, RealizedPnL: 2_000}
	m3 := NewManager(cfg, initialCash)
	m3.SeedFromBaseline(120_000, 1_000)
	if m3.Evaluate(fill, snap2) {
		t.Fatal("seeded manager should reject fill exceeding drawdown limit")
	}
}
