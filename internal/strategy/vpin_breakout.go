package strategy

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/lgreene03/huginn/internal/model"
)

// VPINBreakout is a momentum strategy driven by VPIN (Volume-synchronized
// Probability of Informed Trading — Easley, López de Prado, O'Hara 2012).
//
// # Signal hypothesis
//
// When VPIN spikes above a threshold it indicates a disproportionate
// imbalance between buy- and sell-initiated volume in the recent volume
// bucket. The interpretation is that informed traders are unwinding a
// position; a directional move is likely. The strategy enters in the
// direction of the dominant flow (long when VPIN > threshold).
//
// # Expected regime
//
// Best in trending markets where a single side dominates flow for sustained
// periods. Underperforms in choppy chop where buy- and sell-initiated volume
// alternate quickly — VPIN spikes both directions and the cooldown gate
// becomes the dominant signal.
//
// # Known failure modes
//
//   - Single-sided trader (no symmetric SELL branch). A real research
//     deployment would also short on VPIN < -threshold; this implementation
//     only enters long.
//   - Cooldown is keyed on event time (FeatureEvent.EventTime), not wall
//     time. A restart spans the cooldown window if no new events arrive in
//     the meantime — see docs/STRATEGY_STATE_DESIGN.md §10.5.
//   - VPIN's underlying bucket cadence comes from Muninn. If muninn's volume
//     bucket size is mis-tuned (e.g. minutes-of-volume rather than
//     seconds-of-volume), VPIN spikes are too frequent and the cooldown
//     becomes a Bernoulli filter rather than a meaningful gate.
//
// # Parameter sensitivity
//
//   - Threshold: too low → fires constantly; too high → never fires.
//     Realistic range 0.4–0.7 on per-minute VPIN.
//   - cooldown: lower bound = the typical VPIN bucket period; upper bound =
//     the average hold time you'd be comfortable with in a drawdown.
//
// # State persisted across restarts
//
// `lastTrade` (the cooldown gate). Without it, a restart in the middle of
// a cooldown window would fire a duplicate entry on the next event.
type VPINBreakout struct {
	mu        sync.Mutex
	Threshold float64
	OrderSize float64
	cooldown  time.Duration
	lastTrade time.Time
}

// NewVPINBreakout creates a VPIN breakout strategy with a cooldown period.
func NewVPINBreakout(threshold, orderSize float64, cooldown time.Duration) *VPINBreakout {
	return &VPINBreakout{
		Threshold: threshold,
		OrderSize: orderSize,
		cooldown:  cooldown,
	}
}

func (s *VPINBreakout) Name() string {
	return fmt.Sprintf("VPINBreakout(%.2f)", s.Threshold)
}

func (s *VPINBreakout) OnFeature(event model.FeatureEvent) []model.Order {
	vpin, ok := event.Values["vpin"]
	if !ok {
		return nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// Cooldown check (event-time, not wall-clock — intentional, see design doc §10.5)
	if !s.lastTrade.IsZero() && event.EventTime.Sub(s.lastTrade) < s.cooldown {
		return nil
	}

	if vpin > s.Threshold {
		s.lastTrade = event.EventTime

		slog.Debug("Strategy signal",
			"strategy", s.Name(),
			"action", "BUY",
			"vpin", fmt.Sprintf("%.4f", vpin),
			"instrument", event.Instrument,
		)

		return []model.Order{{
			Instrument: event.Instrument,
			Side:       model.Buy,
			Quantity:   s.OrderSize,
			Reason:     fmt.Sprintf("VPIN=%.4f > threshold=%.2f, informed flow breakout", vpin, s.Threshold),
			Timestamp:  event.EventTime,
		}}
	}

	return nil
}

// vpinStateV1 is the persisted VPINBreakout state shape, schema version 1.
type vpinStateV1 struct {
	LastTrade time.Time `json:"last_trade"`
}

// MarshalState implements Stateful.
func (s *VPINBreakout) MarshalState() ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return MarshalEnvelope(1, vpinStateV1{LastTrade: s.lastTrade})
}

// RestoreState implements Stateful.
func (s *VPINBreakout) RestoreState(data []byte) error {
	version, fields, err := ParseEnvelope(data)
	if err != nil {
		return err
	}
	if version != 1 {
		return fmt.Errorf("%w: VPINBreakout got v%d", ErrStateVersionMismatch, version)
	}
	var f vpinStateV1
	if err := json.Unmarshal(fields, &f); err != nil {
		return fmt.Errorf("VPINBreakout: failed to unmarshal v1 fields: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastTrade = f.LastTrade
	return nil
}
