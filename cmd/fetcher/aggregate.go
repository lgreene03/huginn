package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/lgreene03/huginn/internal/model"
)

// aggregator folds a stream of trades into fixed windows. Both ingest paths
// (REST paging and bulk data.binance.vision dumps) feed the SAME aggregator, so
// a fixture built from bulk dumps is directly comparable to one built from the
// API rather than subtly different in its feature maths.
type aggregator struct {
	window         time.Duration
	windows        map[time.Time]*WindowData
	minWindowStart time.Time
}

func newAggregator(window time.Duration) *aggregator {
	return &aggregator{
		window:  window,
		windows: make(map[time.Time]*WindowData),
	}
}

// add folds one trade into its window. isBuyerMaker follows Binance's
// convention: when the buyer is the maker the aggressor was the seller, so the
// volume is taker sell.
func (a *aggregator) add(ts time.Time, price, qty float64, isBuyerMaker bool) {
	winStart := ts.Truncate(a.window)

	if a.minWindowStart.IsZero() || winStart.Before(a.minWindowStart) {
		a.minWindowStart = winStart
	}

	w, ok := a.windows[winStart]
	if !ok {
		w = &WindowData{StartTime: winStart, EndTime: winStart.Add(a.window)}
		a.windows[winStart] = w
	}

	w.PriceSum += price * qty
	if isBuyerMaker {
		w.SellVol += qty // Taker sell
	} else {
		w.BuyVol += qty // Taker buy
	}
}

// write emits one FeatureEvent per populated window, in chronological order.
//
// NOTE ON THE FEATURES: obi here is signed TAKER FLOW imbalance derived from
// executed trades, which is not the same quantity as the live obi-bridge's
// order-book imbalance (bid depth vs ask depth). vpin is currently |obi| and
// microPrice is vwap, so neither carries information independent of the other
// columns. This is preserved deliberately for comparability with the existing
// fixture; see docs/EDGE_VERDICT.md before drawing conclusions about "OBI".
func (a *aggregator) write(writer io.Writer, instrument string, end time.Time) error {
	slog.Info("Writing computed metrics to JSONL file...", "total_windows", len(a.windows))

	encoder := json.NewEncoder(writer)
	var writtenCount int

	for cur := a.minWindowStart; !cur.After(end); cur = cur.Add(a.window) {
		w, ok := a.windows[cur]
		if !ok {
			continue
		}

		totalVol := w.BuyVol + w.SellVol
		var vwap, obi, vpin float64
		if totalVol > 0 {
			vwap = w.PriceSum / totalVol
			obi = (w.BuyVol - w.SellVol) / totalVol
			vpin = obi
			if vpin < 0 {
				vpin = -vpin
			}
		}

		event := model.FeatureEvent{
			EventID:        fmt.Sprintf("hist-%s-%d", instrument, w.StartTime.Unix()),
			EventTime:      w.EndTime,
			FeatureName:    "market_features",
			FeatureVersion: "v1",
			Instrument:     instrument,
			WindowStart:    w.StartTime,
			WindowEnd:      w.EndTime,
			Values: map[string]float64{
				"obi":        obi,
				"vpin":       vpin,
				"microPrice": vwap,
				"vwap":       vwap,
				"volume":     totalVol,
			},
		}

		if err := encoder.Encode(event); err != nil {
			return fmt.Errorf("failed to encode feature event: %w", err)
		}
		writtenCount++
	}

	slog.Info("Finished serialization", "written_events", writtenCount)
	return nil
}
