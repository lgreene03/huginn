package strategy

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"

	"github.com/lgreene03/huginn/internal/model"
)

// VWAPDeviation is a mean-reversion strategy driven by price deviation from VWAP.
//
// # Signal hypothesis
//
// VWAP (volume-weighted average price) is treated as a soft fair-value
// anchor. When the current micro-price diverges by more than `ThresholdPct`,
// the strategy takes the contrarian side expecting reversion to the anchor.
//
// # Expected regime
//
// Best in range-bound markets where intraday price action genuinely
// oscillates around VWAP. Common workhorse for execution algorithms and
// short-horizon stat-arb.
//
// # Known failure modes
//
//   - Trending regimes. A sustained directional move drags VWAP along behind
//     price; deviation never closes and the strategy keeps fading. The risk
//     manager's drawdown trip is the eventual stop.
//   - Open vs. session VWAP. Muninn currently emits a rolling VWAP; if it
//     ever switches to session-VWAP semantics the threshold needs retuning
//     because the deviation distribution changes.
//   - Threshold floor. ThresholdPct below ~5 bps is below typical spread +
//     transaction cost in liquid crypto — every trade is a guaranteed loser.
//
// # Parameter sensitivity
//
//   - ThresholdPct: realistic range 0.05% – 0.5% in liquid crypto. Below
//     this, costs dominate. Above this, the strategy almost never fires.
//   - maxPosition: throttle ceiling (typically `OrderSize × 10` at boot).
//
// # State persisted across restarts
//
// `netPosition`. Without it the throttle gate resets on every restart.
type VWAPDeviation struct {
	mu           sync.Mutex
	ThresholdPct float64 // e.g. 0.001 for 0.1% deviation
	OrderSize    float64 // e.g. 0.01 BTC
	maxPosition  float64 // maximum net position before throttling
	netPosition  float64 // tracks current net exposure
}

// NewVWAPDeviation creates a new VWAP deviation mean-reversion strategy.
func NewVWAPDeviation(thresholdPct, orderSize, maxPosition float64) *VWAPDeviation {
	return &VWAPDeviation{
		ThresholdPct: thresholdPct,
		OrderSize:    orderSize,
		maxPosition:  maxPosition,
	}
}

func (s *VWAPDeviation) Name() string {
	return fmt.Sprintf("VWAPDeviation(%.4f)", s.ThresholdPct)
}

func (s *VWAPDeviation) OnFeature(event model.FeatureEvent) []model.Order {
	vwap, hasVwap := event.Values["vwap"]
	price, hasPrice := event.Values["microPrice"]
	if !hasPrice {
		price, hasPrice = event.Values["value"]
	}
	if !hasVwap || !hasPrice || vwap <= 0 {
		return nil
	}

	deviation := (price - vwap) / vwap

	s.mu.Lock()
	defer s.mu.Unlock()

	var orders []model.Order

	if deviation > s.ThresholdPct && s.netPosition > -s.maxPosition {
		// Overvalued -> expect reversion -> sell
		orders = append(orders, model.Order{
			Instrument: event.Instrument,
			Side:       model.Sell,
			Quantity:   s.OrderSize,
			Reason:     fmt.Sprintf("Price=%.2f > VWAP=%.2f (dev=%.4f > threshold=%.4f), mean-reversion sell", price, vwap, deviation, s.ThresholdPct),
			Timestamp:  event.EventTime,
		})
		s.netPosition -= s.OrderSize

		slog.Debug("Strategy signal",
			"strategy", s.Name(),
			"action", "SELL",
			"deviation", fmt.Sprintf("%.4f", deviation),
			"instrument", event.Instrument,
			"net_position", fmt.Sprintf("%.8f", s.netPosition),
		)

	} else if deviation < -s.ThresholdPct && s.netPosition < s.maxPosition {
		// Undervalued -> expect reversion -> buy
		orders = append(orders, model.Order{
			Instrument: event.Instrument,
			Side:       model.Buy,
			Quantity:   s.OrderSize,
			Reason:     fmt.Sprintf("Price=%.2f < VWAP=%.2f (dev=%.4f < threshold=%.4f), mean-reversion buy", price, vwap, deviation, -s.ThresholdPct),
			Timestamp:  event.EventTime,
		})
		s.netPosition += s.OrderSize

		slog.Debug("Strategy signal",
			"strategy", s.Name(),
			"action", "BUY",
			"deviation", fmt.Sprintf("%.4f", deviation),
			"instrument", event.Instrument,
			"net_position", fmt.Sprintf("%.8f", s.netPosition),
		)
	}

	return orders
}

// vwapStateV1 is the persisted VWAPDeviation state shape, schema version 1.
type vwapStateV1 struct {
	NetPosition float64 `json:"net_position"`
}

// MarshalState implements Stateful.
func (s *VWAPDeviation) MarshalState() ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return MarshalEnvelope(1, vwapStateV1{NetPosition: s.netPosition})
}

// RestoreState implements Stateful.
func (s *VWAPDeviation) RestoreState(data []byte) error {
	version, fields, err := ParseEnvelope(data)
	if err != nil {
		return err
	}
	if version != 1 {
		return fmt.Errorf("%w: VWAPDeviation got v%d", ErrStateVersionMismatch, version)
	}
	var f vwapStateV1
	if err := json.Unmarshal(fields, &f); err != nil {
		return fmt.Errorf("VWAPDeviation: failed to unmarshal v1 fields: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.netPosition = f.NetPosition
	return nil
}
