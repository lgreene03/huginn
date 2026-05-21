// Package risk applies pre-trade controls (drawdown, daily-loss, position
// limits) and surface-level breaker switches. State that must survive a
// restart (peakValue, dayStartRealizedPnL, lastFeatureEvent) is captured by
// the Stateful methods and persisted via the journal alongside strategy
// state. See docs/STRATEGY_STATE_DESIGN.md for the wider context.
package risk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sync"
	"time"

	"github.com/lgreene03/huginn/internal/config"
	"github.com/lgreene03/huginn/internal/metrics"
	"github.com/lgreene03/huginn/internal/model"
	"github.com/lgreene03/huginn/internal/portfolio"
)

// HaltReason is a typed label so observability and tests can distinguish a
// manual circuit-breaker from a drawdown trip or a staleness auto-halt.
type HaltReason string

const (
	HaltNone      HaltReason = ""
	HaltManual    HaltReason = "manual"
	HaltDrawdown  HaltReason = "drawdown"
	HaltStaleness HaltReason = "feature_staleness"
)

// Manager is responsible for applying pre-trade risk controls.
type Manager struct {
	config      config.RiskConfig
	initialCash float64

	mu           sync.RWMutex
	halted       bool
	haltReason   HaltReason
	peakValue    float64
	recentPrices []float64
	now          func() time.Time
	// dayStart is the UTC midnight that defines the current daily-loss window.
	dayStart             time.Time
	dayStartRealizedPnL  float64
	lastFeatureEventTime time.Time // event-time (from Muninn), not wall-clock
}

// NewManager creates a new Risk Manager.
func NewManager(cfg config.RiskConfig, initialCash float64) *Manager {
	return newManagerWithClock(cfg, initialCash, time.Now)
}

// newManagerWithClock is the test seam — production callers use NewManager.
func newManagerWithClock(cfg config.RiskConfig, initialCash float64, now func() time.Time) *Manager {
	t := now()
	return &Manager{
		config:      cfg,
		initialCash: initialCash,
		peakValue:   initialCash,
		now:         now,
		dayStart:    startOfUTCDay(t),
	}
}

func startOfUTCDay(t time.Time) time.Time {
	u := t.UTC()
	return time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
}

// Halt manually halts strategy trading.
func (m *Manager) Halt() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.halted = true
	m.haltReason = HaltManual
	m.updateHaltGauges()
	slog.Warn("Manual Circuit Breaker Activated")
}

// Resume manually resumes strategy trading. Clears any halt reason.
func (m *Manager) Resume() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.halted = false
	m.haltReason = HaltNone
	m.updateHaltGauges()
	slog.Info("Manual Circuit Breaker Reset")
}

// IsHalted returns the current status of the manual circuit breaker.
func (m *Manager) IsHalted() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.halted
}

// HaltReason returns the typed reason for the current halt, or HaltNone.
func (m *Manager) HaltReason() HaltReason {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.haltReason
}

// UpdateLimits dynamically updates the hard position limit in a thread-safe manner.
func (m *Manager) UpdateLimits(positionLimit float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.config.PositionLimitHard = positionLimit
	slog.Info("Risk manager limits updated dynamically", "position_limit_hard", positionLimit)
}

// OnFeatureSeen records that a fresh feature event was just observed. The
// staleness watchdog and the auto-resume-from-staleness path both consult
// this. Pass event time, not wall time — consistent with the rest of the
// engine's event-time semantics.
func (m *Manager) OnFeatureSeen(eventTime time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lastFeatureEventTime = eventTime
	if m.halted && m.haltReason == HaltStaleness && m.config.AutoResumeAfterStaleness {
		m.halted = false
		m.haltReason = HaltNone
		m.updateHaltGauges()
		slog.Info("Staleness halt auto-cleared by fresh feature event", "event_time", eventTime)
	}
}

// RunStalenessWatchdog runs a background loop that halts the manager when
// no feature event has been observed within cfg.StalenessTimeout (compared
// against wall-clock, since a stalled producer means events stop arriving in
// real time). Zero StalenessTimeout disables the watchdog entirely. Cancel
// via ctx.
func (m *Manager) RunStalenessWatchdog(ctx context.Context) {
	if m.config.StalenessTimeout <= 0 {
		return
	}
	// Check at one-quarter of the timeout (bounded below by 1s) so the WARN
	// fires reasonably close to the actual threshold without busy-spinning.
	tick := m.config.StalenessTimeout / 4
	if tick < time.Second {
		tick = time.Second
	}
	ticker := time.NewTicker(tick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.mu.Lock()
			if m.lastFeatureEventTime.IsZero() {
				m.mu.Unlock()
				continue
			}
			gap := m.now().Sub(m.lastFeatureEventTime)
			if gap > m.config.StalenessTimeout && (!m.halted || m.haltReason != HaltStaleness) {
				m.halted = true
				m.haltReason = HaltStaleness
				m.updateHaltGauges()
				metrics.OrdersRejectedTotal.WithLabelValues("feature_staleness").Inc()
				slog.Warn("Risk auto-halt: feature event staleness",
					"last_event", m.lastFeatureEventTime,
					"gap", gap.String(),
					"threshold", m.config.StalenessTimeout.String(),
				)
			}
			m.mu.Unlock()
		}
	}
}

// Evaluate checks if a prospective fill violates any risk parameters based on
// the current portfolio snapshot. Returns true if the fill is approved.
func (m *Manager) Evaluate(fill model.Fill, snap portfolio.Snapshot) bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	// 0. Halt check (any reason: manual, drawdown, staleness)
	if m.halted {
		slog.Warn("Risk limit triggered: halted",
			"reason", string(m.haltReason),
			"order_id", fill.OrderID,
		)
		metrics.OrdersRejectedTotal.WithLabelValues(string(m.haltReason)).Inc()
		return false
	}

	// 1. Peak value and Trailing Stop-Loss Check
	if snap.TotalValue > m.peakValue {
		m.peakValue = snap.TotalValue
	}

	trailingStopThreshold := m.peakValue * (1.0 - m.config.MaxDrawdownPct)
	if snap.TotalValue < trailingStopThreshold {
		m.halted = true
		m.haltReason = HaltDrawdown
		m.updateHaltGauges()
		slog.Warn("Risk limit triggered: Trailing Stop Loss Circuit Breaker Activated",
			"total_value", snap.TotalValue,
			"peak_value", m.peakValue,
			"threshold", trailingStopThreshold,
			"drawdown_pct", fmt.Sprintf("%.2f%%", (m.peakValue-snap.TotalValue)/m.peakValue*100.0),
			"order_id", fill.OrderID,
		)
		metrics.OrdersRejectedTotal.WithLabelValues("drawdown").Inc()
		return false
	}

	// 2. Daily Loss Limit (windowed on UTC day boundary).
	// Roll the window forward if the day has changed.
	today := startOfUTCDay(m.now())
	if today.After(m.dayStart) {
		slog.Info("Risk: daily loss window rolled",
			"prev_day", m.dayStart,
			"new_day", today,
			"new_baseline_realized_pnl", snap.RealizedPnL,
		)
		m.dayStart = today
		m.dayStartRealizedPnL = snap.RealizedPnL
	}
	intradayPnL := snap.RealizedPnL - m.dayStartRealizedPnL
	if intradayPnL < -m.config.DailyLossLimit {
		slog.Warn("Risk limit triggered: Daily Loss Limit",
			"intraday_pnl", intradayPnL,
			"limit", -m.config.DailyLossLimit,
			"order_id", fill.OrderID,
		)
		metrics.OrdersRejectedTotal.WithLabelValues("daily_loss").Inc()
		return false
	}

	// 3. Volatility tracking and Volatility-adjusted Position Limit
	m.recentPrices = append(m.recentPrices, fill.FillPrice)
	if len(m.recentPrices) > 30 {
		m.recentPrices = m.recentPrices[1:]
	}

	var volPct float64
	n := len(m.recentPrices)
	if n >= 10 {
		sum := 0.0
		for _, p := range m.recentPrices {
			sum += p
		}
		mean := sum / float64(n)
		varianceSum := 0.0
		for _, p := range m.recentPrices {
			diff := p - mean
			varianceSum += diff * diff
		}
		stdDev := math.Sqrt(varianceSum / float64(n))
		if mean > 0 {
			volPct = stdDev / mean
		}
	}
	volScale := 1.0
	if volPct > 0 {
		volScale = 1.0 / (1.0 + 100.0*volPct)
	}

	// 4. Position Limit Check.
	// Per-instrument override takes precedence when configured; otherwise the
	// vol-scaled gross hard limit applies.
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
	grossNotional := math.Abs(newQty) * fill.FillPrice

	if perInstrument, ok := m.config.PositionLimitPerInstrument[fill.Instrument]; ok && perInstrument > 0 {
		if grossNotional > perInstrument {
			slog.Warn("Risk limit triggered: Per-Instrument Position Limit",
				"instrument", fill.Instrument,
				"proposed_notional", grossNotional,
				"limit", perInstrument,
				"order_id", fill.OrderID,
			)
			metrics.OrdersRejectedTotal.WithLabelValues("position_limit").Inc()
			return false
		}
	} else {
		effectivePositionLimit := m.config.PositionLimitHard * volScale
		if grossNotional > effectivePositionLimit {
			slog.Warn("Risk limit triggered: Position Limit Throttled",
				"instrument", fill.Instrument,
				"proposed_notional", grossNotional,
				"limit", m.config.PositionLimitHard,
				"effective_limit", effectivePositionLimit,
				"volScale", fmt.Sprintf("%.4f", volScale),
				"volPct", fmt.Sprintf("%.4f%%", volPct*100.0),
				"order_id", fill.OrderID,
			)
			metrics.OrdersRejectedTotal.WithLabelValues("position_limit").Inc()
			return false
		}
	}

	return true
}

// updateHaltGauges syncs the Prometheus halt gauges to the current state.
// Must be called with m.mu held (write or read — see callers).
func (m *Manager) updateHaltGauges() {
	if m.halted {
		metrics.RiskHaltActive.Set(1)
	} else {
		metrics.RiskHaltActive.Set(0)
	}
	// Zero out every reason then set the active one (if any).
	for _, r := range []HaltReason{HaltManual, HaltDrawdown, HaltStaleness} {
		metrics.RiskHaltReason.WithLabelValues(string(r)).Set(0)
	}
	if m.halted && m.haltReason != HaltNone {
		metrics.RiskHaltReason.WithLabelValues(string(m.haltReason)).Set(1)
	}
}

// GetPositionLimitHard returns the current configured hard position limit in a thread-safe manner.
func (m *Manager) GetPositionLimitHard() float64 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.config.PositionLimitHard
}

// PeakValue returns the highest portfolio total value seen since the last
// reset. Needed by the executor's daily PnL snapshot writer.
func (m *Manager) PeakValue() float64 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.peakValue
}

// DayStartRealizedPnL returns the realized-PnL baseline at the start of the
// current daily loss window. Needed by the executor's daily PnL snapshot writer.
func (m *Manager) DayStartRealizedPnL() float64 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.dayStartRealizedPnL
}

// SeedFromBaseline seeds the risk manager's peakValue and dayStartRealizedPnL
// from a daily_pnl_snapshots row. Used as a fallback boot path when the
// strategy_state._risk blob is absent — provides best-effort recovery of the
// two most critical risk fields without requiring a full state blob.
func (m *Manager) SeedFromBaseline(peakValue, dayStartRealizedPnL float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if peakValue > 0 {
		m.peakValue = peakValue
	}
	m.dayStartRealizedPnL = dayStartRealizedPnL
	slog.Info("Risk manager seeded from daily_pnl_snapshots fallback",
		"peak_value", peakValue,
		"day_start_realized_pnl", dayStartRealizedPnL,
	)
}

// ----- Persistence (matches strategy.Stateful semantics; the executor calls
// MarshalState/RestoreState alongside the strategy's). -----

// riskStateV1 is the persisted Manager state, schema version 1.
type riskStateV1 struct {
	PeakValue            float64   `json:"peak_value"`
	DayStart             time.Time `json:"day_start"`
	DayStartRealizedPnL  float64   `json:"day_start_realized_pnl"`
	LastFeatureEventTime time.Time `json:"last_feature_event_time"`
}

type riskEnvelopeV1 struct {
	Version int             `json:"version"`
	Fields  json.RawMessage `json:"fields"`
}

// ErrRiskStateVersionMismatch is returned by RestoreState when the persisted
// envelope version is newer than this binary supports.
var ErrRiskStateVersionMismatch = errors.New("risk: unsupported state version")

// MarshalState returns a versioned snapshot of the manager's restart-critical
// state. Safe to call concurrently with Evaluate.
func (m *Manager) MarshalState() ([]byte, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	fields, err := json.Marshal(riskStateV1{
		PeakValue:            m.peakValue,
		DayStart:             m.dayStart,
		DayStartRealizedPnL:  m.dayStartRealizedPnL,
		LastFeatureEventTime: m.lastFeatureEventTime,
	})
	if err != nil {
		return nil, err
	}
	return json.Marshal(riskEnvelopeV1{Version: 1, Fields: fields})
}

// RestoreState applies a previously-persisted snapshot. An empty data slice
// is a no-op (no prior state, fresh start).
func (m *Manager) RestoreState(data []byte) error {
	if len(data) == 0 {
		return nil
	}
	var env riskEnvelopeV1
	if err := json.Unmarshal(data, &env); err != nil {
		return fmt.Errorf("risk: failed to parse envelope: %w", err)
	}
	if env.Version != 1 {
		return fmt.Errorf("%w: got v%d", ErrRiskStateVersionMismatch, env.Version)
	}
	var f riskStateV1
	if err := json.Unmarshal(env.Fields, &f); err != nil {
		return fmt.Errorf("risk: failed to parse v1 fields: %w", err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if f.PeakValue > 0 {
		m.peakValue = f.PeakValue
	}
	if !f.DayStart.IsZero() {
		m.dayStart = f.DayStart
	}
	m.dayStartRealizedPnL = f.DayStartRealizedPnL
	m.lastFeatureEventTime = f.LastFeatureEventTime
	return nil
}
