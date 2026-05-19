package strategy

import (
	"fmt"
	"log/slog"
	"time"

	"github.com/lgreene/huginn/internal/model"
)

// OBIThreshold is a mean-reversion strategy driven by Order Book Imbalance.
//
// When OBI exceeds +threshold (extreme buy pressure), the strategy assumes a
// short-term reversion is likely and sells. When OBI drops below -threshold
// (extreme sell pressure), it buys. This is a classic market-making signal
// used by HFT desks to capture adverse-selection-aware spreads.
type OBIThreshold struct {
	Threshold    float64 // e.g. 0.7
	OrderSize    float64 // e.g. 0.01 BTC
	maxPosition  float64 // maximum net position before throttling
	netPosition  float64 // tracks current net exposure
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

		slog.Info("Strategy signal",
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

		slog.Info("Strategy signal",
			"strategy", s.Name(),
			"action", "BUY",
			"obi", fmt.Sprintf("%.4f", obi),
			"instrument", event.Instrument,
			"net_position", fmt.Sprintf("%.8f", s.netPosition),
		)
	}

	return orders
}

// VPINBreakout is a momentum strategy driven by VPIN (order flow toxicity).
//
// When VPIN exceeds a threshold, it signals that informed traders are active
// and a directional move is likely. The strategy enters in the direction of
// the dominant flow.
type VPINBreakout struct {
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

	// Cooldown check
	if !s.lastTrade.IsZero() && event.EventTime.Sub(s.lastTrade) < s.cooldown {
		return nil
	}

	if vpin > s.Threshold {
		s.lastTrade = event.EventTime

		slog.Info("Strategy signal",
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
