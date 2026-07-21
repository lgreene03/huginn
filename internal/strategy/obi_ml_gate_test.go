package strategy

import (
	"testing"
	"time"

	"github.com/lgreene03/huginn/internal/model"
)

// mlGateEvent builds an OBI entry event that clears every non-ML filter but
// carries a DEGENERATE ML score: the model reports ready (mlReady=1) with a
// near-constant low confidence (0.08), which is exactly the shipped, untrained
// model's behaviour. Whether this blocks the entry depends solely on the ML gate.
func mlGateEvent(obi float64, t time.Time) model.FeatureEvent {
	return model.FeatureEvent{
		EventID:     "mlgate-evt",
		EventTime:   t,
		FeatureName: "obi",
		Instrument:  "BTC-USDT",
		Values: map[string]float64{
			"obi":         obi,
			"midPrice":    60_000,
			"momentum":    0.0001,
			"momentum1m":  0.0001,
			"momentum15m": 0.0001,
			"volatility":  0.008,
			"fearGreed":   50,
			"volumeRatio": 1.2,
			"mlScore":     0.08, // degenerate: below the 0.35 floor
			"mlReady":     1,    // model reports trained
			"fundingRate": 0.0,
			"oiChange":    0.0,
		},
	}
}

// TestOBIMLGate_OffByDefault_DoesNotBlock is the honesty guard: with the ML gate
// OFF (the default), the untrained near-constant model MUST NOT block an entry
// that clears every real filter. A degenerate model shaping live trades is the
// exact foot-gun this default prevents.
func TestOBIMLGate_OffByDefault_DoesNotBlock(t *testing.T) {
	quietLogsT(t)
	base := time.Unix(1_700_000_000, 0)

	s := NewOBIThreshold(0.7, 0.001, 0.1) // default params: mlGateEnabled=false
	orders := s.OnFeature(mlGateEvent(0.95, base))
	if len(orders) != 1 || orders[0].Side != model.Sell {
		t.Fatalf("gate off: a degenerate ML score must not block the entry; got %+v", orders)
	}
}

// TestOBIMLGate_OnBlocksLowConfidence asserts the gate still works when
// EXPLICITLY enabled: with mlGateEnabled=true and the 0.35 floor, the same
// degenerate 0.08 score blocks the entry. So the capability is preserved for a
// trained model; it is just not on by default.
func TestOBIMLGate_OnBlocksLowConfidence(t *testing.T) {
	quietLogsT(t)
	base := time.Unix(1_700_000_000, 0)

	p := DefaultOBIParams()
	p.MLGateEnabled = true
	p.MLMinConfidence = 0.35
	s := NewOBIThresholdWithParams(0.7, 0.001, 0.1, p)

	orders := s.OnFeature(mlGateEvent(0.95, base))
	if len(orders) != 0 {
		t.Fatalf("gate on: a 0.08 score below the 0.35 floor must block the entry; got %+v", orders)
	}
}
