package strategy

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/lgreene03/huginn/internal/model"
)

// quietLogs swaps the default slog logger for a discard handler for the
// duration of a benchmark and restores it afterwards. The bundled strategies
// emit slog.Info lines on every signal; without this the benchmark would be
// dominated by log-formatting I/O rather than the signal-to-decision compute we
// want to measure.
func quietLogs(b *testing.B) {
	b.Helper()
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	b.Cleanup(func() { slog.SetDefault(prev) })
}

// benchFeatureEvent builds a representative OBI feature event carrying the full
// set of signal-layer values the OBIThreshold entry path reads, so the
// benchmark exercises every filter rather than bailing out early on a missing
// key.
func benchFeatureEvent(obi float64) model.FeatureEvent {
	return model.FeatureEvent{
		EventID:     "bench-evt",
		EventTime:   time.Unix(1_700_000_000, 0),
		FeatureName: "obi",
		Instrument:  "BTC-USDT",
		Values: map[string]float64{
			"obi":           obi,
			"midPrice":      60_000,
			"momentum":      0.0001,
			"momentum1m":    0.0001,
			"momentum15m":   0.0001,
			"volatility":    0.008,
			"fearGreed":     50,
			"volumeRatio":   1.2,
			"mlScore":       0.5,
			"mlReady":       0,
			"newsSentiment": 0.0,
			"fundingRate":   0.0,
			"oiChange":      0.0,
		},
	}
}

// BenchmarkOBIOnFeature_Signal measures the signal-to-decision hot path when
// the event clears the threshold and an entry order is produced (the most
// expensive branch: all entry filters run, a position opens, an order is
// formatted). Each iteration uses a fresh strategy so per-instrument throttling
// does not suppress the entry after the first hit.
func BenchmarkOBIOnFeature_Signal(b *testing.B) {
	quietLogs(b)
	event := benchFeatureEvent(0.95) // > threshold => SELL entry
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s := NewOBIThreshold(0.7, 0.01, 0.1)
		_ = s.OnFeature(event)
	}
}

// BenchmarkOBIOnFeature_NoSignal measures the common case: an event in the
// dead zone that produces no order. This is the per-event cost the dispatcher
// pays for the overwhelming majority of feature events. The same strategy
// instance is reused since no state mutates on a no-signal event.
func BenchmarkOBIOnFeature_NoSignal(b *testing.B) {
	quietLogs(b)
	s := NewOBIThreshold(0.7, 0.01, 0.1)
	event := benchFeatureEvent(0.30) // inside dead zone => no order
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = s.OnFeature(event)
	}
}
