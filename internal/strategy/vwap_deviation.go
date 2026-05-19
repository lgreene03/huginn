package strategy

import (
	"fmt"
	"log/slog"

	"github.com/lgreene/huginn/internal/model"
)

// VWAPDeviation is a mean-reversion strategy driven by price deviation from VWAP.
//
// When price rises above VWAP by more than a threshold percentage, it assumes a
// short-term overvaluation and sells. When price falls below VWAP by more than
// a threshold percentage, it buys.
type VWAPDeviation struct {
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

		slog.Info("Strategy signal",
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

		slog.Info("Strategy signal",
			"strategy", s.Name(),
			"action", "BUY",
			"deviation", fmt.Sprintf("%.4f", deviation),
			"instrument", event.Instrument,
			"net_position", fmt.Sprintf("%.8f", s.netPosition),
		)
	}

	return orders
}
