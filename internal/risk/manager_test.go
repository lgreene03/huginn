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
