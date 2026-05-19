package risk

import (
	"log/slog"
	"math"
	"sync"

	"github.com/lgreene/huginn/internal/config"
	"github.com/lgreene/huginn/internal/model"
	"github.com/lgreene/huginn/internal/portfolio"
)

// Manager is responsible for applying pre-trade risk controls.
type Manager struct {
	config      config.RiskConfig
	initialCash float64
	mu          sync.RWMutex
	halted      bool
}

// NewManager creates a new Risk Manager.
func NewManager(cfg config.RiskConfig, initialCash float64) *Manager {
	return &Manager{
		config:      cfg,
		initialCash: initialCash,
	}
}

// Halt manually halts strategy trading.
func (m *Manager) Halt() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.halted = true
	slog.Warn("Manual Circuit Breaker Activated")
}

// Resume manually resumes strategy trading.
func (m *Manager) Resume() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.halted = false
	slog.Info("Manual Circuit Breaker Reset")
}

// IsHalted returns the current status of the manual circuit breaker.
func (m *Manager) IsHalted() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.halted
}

// Evaluate checks if a prospective fill violates any risk parameters based on the current portfolio snapshot.
// It returns true if the fill is approved, or false if it is rejected.
func (m *Manager) Evaluate(fill model.Fill, snap portfolio.Snapshot) bool {
	// 0. Manual Circuit Breaker
	if m.IsHalted() {
		slog.Warn("Risk limit triggered: Manual Circuit Breaker Active",
			"order_id", fill.OrderID,
		)
		return false
	}

	// 1. Max Drawdown
	drawdownThreshold := m.initialCash * (1.0 - m.config.MaxDrawdownPct)
	if snap.TotalValue < drawdownThreshold {
		slog.Warn("Risk limit triggered: Max Drawdown",
			"total_value", snap.TotalValue,
			"threshold", drawdownThreshold,
			"order_id", fill.OrderID,
		)
		return false
	}

	// 2. Daily Loss Limit
	if snap.RealizedPnL < -m.config.DailyLossLimit {
		slog.Warn("Risk limit triggered: Daily Loss Limit",
			"realized_pnl", snap.RealizedPnL,
			"limit", -m.config.DailyLossLimit,
			"order_id", fill.OrderID,
		)
		return false
	}

	// 3. Position Limit
	currentPosQty := 0.0
	if pos, exists := snap.Positions[fill.Instrument]; exists {
		currentPosQty = pos.Quantity
	}

	var newQty float64
	if fill.Side == model.Buy {
		newQty = currentPosQty + fill.Quantity
	} else {
		newQty = currentPosQty - fill.Quantity
	}

	// Gross notional exposure
	grossNotional := math.Abs(newQty) * fill.FillPrice

	if grossNotional > m.config.PositionLimitHard {
		slog.Warn("Risk limit triggered: Position Limit",
			"instrument", fill.Instrument,
			"proposed_notional", grossNotional,
			"limit", m.config.PositionLimitHard,
			"order_id", fill.OrderID,
		)
		return false
	}

	return true
}
