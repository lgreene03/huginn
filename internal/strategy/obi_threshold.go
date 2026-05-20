package strategy

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"

	"github.com/lgreene03/huginn/internal/model"
)

// OBIThreshold is a mean-reversion strategy driven by Order Book Imbalance.
//
// # Signal hypothesis
//
// OBI = (bid_qty − ask_qty) / (bid_qty + ask_qty), bounded in [−1, +1].
// When the book is heavily lopsided to the buy side (OBI > +threshold), the
// strategy interprets this as transient adverse selection pressure that will
// revert as the displayed quote is consumed and replenished. The mirror logic
// fires on the sell side.
//
// # Expected regime
//
// Best in liquid order-book regimes with stable depth where short-term
// imbalances genuinely mean-revert. Classic HFT market-making signal.
//
// # Known failure modes
//
//   - Regime change. In a sustained directional move, OBI stays pegged at
//     the threshold and the strategy keeps fading into it — accumulating
//     a position that the throttle eventually clamps but at growing MTM
//     loss. The risk manager's drawdown trip is the eventual stop.
//   - Spoofed depth. Adversarial flickers in displayed liquidity bias OBI
//     without representing real interest; the strategy reacts to noise.
//   - Throttle saturation. Once `netPosition` hits `±maxPosition`, the
//     strategy stops trading the direction it's loaded in. Subsequent
//     reversion moves leave money on the table until the throttle releases.
//
// # Parameter sensitivity
//
//   - Threshold: the dominant parameter. Realistic range 0.5–0.8. Lower
//     values trade more often; higher values are more selective but suffer
//     when extreme imbalances are themselves rare.
//   - maxPosition (derived from `OrderSize × 10` at boot): the throttle ceiling.
//
// # State persisted across restarts
//
// `netPosition`. Without it, the throttle gate resets on every restart and
// the strategy can rebuild position past the configured ceiling.
type OBIThreshold struct {
	mu          sync.Mutex
	Threshold   float64 // e.g. 0.7
	OrderSize   float64 // e.g. 0.01 BTC
	maxPosition float64 // maximum net position before throttling
	netPosition float64 // tracks current net exposure
}

// NewOBIThreshold creates a threshold-based OBI strategy.
func NewOBIThreshold(threshold, orderSize, maxPosition float64) *OBIThreshold {
	return &OBIThreshold{
		Threshold:   threshold,
		OrderSize:   orderSize,
		maxPosition: maxPosition,
	}
}

func (s *OBIThreshold) Name() string {
	return fmt.Sprintf("OBIThreshold(%.2f)", s.Threshold)
}

func (s *OBIThreshold) OnFeature(event model.FeatureEvent) []model.Order {
	obi, ok := event.Values["obi"]
	if !ok {
		return nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	var orders []model.Order

	if obi > s.Threshold && s.netPosition > -s.maxPosition {
		// Extreme buy pressure → expect reversion → sell
		orders = append(orders, model.Order{
			Instrument: event.Instrument,
			Side:       model.Sell,
			Quantity:   s.OrderSize,
			Reason:     fmt.Sprintf("OBI=%.4f > threshold=%.2f, mean-reversion sell", obi, s.Threshold),
			Timestamp:  event.EventTime,
		})
		s.netPosition -= s.OrderSize

		slog.Debug("Strategy signal",
			"strategy", s.Name(),
			"action", "SELL",
			"obi", fmt.Sprintf("%.4f", obi),
			"instrument", event.Instrument,
			"net_position", fmt.Sprintf("%.8f", s.netPosition),
		)

	} else if obi < -s.Threshold && s.netPosition < s.maxPosition {
		// Extreme sell pressure → expect reversion → buy
		orders = append(orders, model.Order{
			Instrument: event.Instrument,
			Side:       model.Buy,
			Quantity:   s.OrderSize,
			Reason:     fmt.Sprintf("OBI=%.4f < threshold=%.2f, mean-reversion buy", obi, -s.Threshold),
			Timestamp:  event.EventTime,
		})
		s.netPosition += s.OrderSize

		slog.Debug("Strategy signal",
			"strategy", s.Name(),
			"action", "BUY",
			"obi", fmt.Sprintf("%.4f", obi),
			"instrument", event.Instrument,
			"net_position", fmt.Sprintf("%.8f", s.netPosition),
		)
	}

	return orders
}

// obiStateV1 is the persisted OBIThreshold state shape, schema version 1.
type obiStateV1 struct {
	NetPosition float64 `json:"net_position"`
}

// MarshalState implements Stateful.
func (s *OBIThreshold) MarshalState() ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return MarshalEnvelope(1, obiStateV1{NetPosition: s.netPosition})
}

// RestoreState implements Stateful.
func (s *OBIThreshold) RestoreState(data []byte) error {
	version, fields, err := ParseEnvelope(data)
	if err != nil {
		return err
	}
	if version != 1 {
		return fmt.Errorf("%w: OBIThreshold got v%d", ErrStateVersionMismatch, version)
	}
	var f obiStateV1
	if err := json.Unmarshal(fields, &f); err != nil {
		return fmt.Errorf("OBIThreshold: failed to unmarshal v1 fields: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.netPosition = f.NetPosition
	return nil
}
