package model

import (
	"encoding/json"
	"testing"
	"time"
)

func TestSideString(t *testing.T) {
	tests := []struct {
		side Side
		want string
	}{
		{Buy, "BUY"},
		{Sell, "SELL"},
		{Side(99), "UNKNOWN"},
	}
	for _, tt := range tests {
		if got := tt.side.String(); got != tt.want {
			t.Errorf("Side(%d).String() = %q, want %q", tt.side, got, tt.want)
		}
	}
}

func TestFeatureEventJSON(t *testing.T) {
	raw := `{
		"eventId": "evt-001",
		"eventTime": "2026-06-01T12:00:00Z",
		"featureName": "obi",
		"featureVersion": "v1",
		"instrument": "BTC-USDT",
		"windowStart": "2026-06-01T11:59:00Z",
		"windowEnd": "2026-06-01T12:00:00Z",
		"values": {"obi": -0.85, "micro_price": 67500.50}
	}`

	var ev FeatureEvent
	if err := json.Unmarshal([]byte(raw), &ev); err != nil {
		t.Fatalf("Unmarshal error: %v", err)
	}

	if ev.EventID != "evt-001" {
		t.Errorf("EventID = %q, want %q", ev.EventID, "evt-001")
	}
	if ev.FeatureName != "obi" {
		t.Errorf("FeatureName = %q, want %q", ev.FeatureName, "obi")
	}
	if ev.Instrument != "BTC-USDT" {
		t.Errorf("Instrument = %q, want %q", ev.Instrument, "BTC-USDT")
	}
	if ev.Values["obi"] != -0.85 {
		t.Errorf("Values[obi] = %v, want -0.85", ev.Values["obi"])
	}
	if ev.Values["micro_price"] != 67500.50 {
		t.Errorf("Values[micro_price] = %v, want 67500.50", ev.Values["micro_price"])
	}
	if ev.WindowEnd.Before(ev.WindowStart) {
		t.Error("WindowEnd should be after WindowStart")
	}
}

func TestFeatureEventRoundTrip(t *testing.T) {
	original := FeatureEvent{
		EventID:        "evt-002",
		EventTime:      time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC),
		FeatureName:    "vwap",
		FeatureVersion: "v1",
		Instrument:     "ETH-USDT",
		WindowStart:    time.Date(2026, 6, 1, 11, 59, 0, 0, time.UTC),
		WindowEnd:      time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC),
		Values:         map[string]float64{"vwap": 3500.25},
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal error: %v", err)
	}

	var decoded FeatureEvent
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal error: %v", err)
	}

	if decoded.EventID != original.EventID {
		t.Errorf("round-trip EventID = %q, want %q", decoded.EventID, original.EventID)
	}
	if decoded.Instrument != original.Instrument {
		t.Errorf("round-trip Instrument = %q, want %q", decoded.Instrument, original.Instrument)
	}
	if decoded.Values["vwap"] != original.Values["vwap"] {
		t.Errorf("round-trip Values[vwap] = %v, want %v", decoded.Values["vwap"], original.Values["vwap"])
	}
}

func TestFillFields(t *testing.T) {
	f := Fill{
		OrderID:         "ord-001",
		ExecutionID:     "exec-001",
		Instrument:      "BTC-USDT",
		Side:            Buy,
		Quantity:        0.5,
		FillPrice:       67500.0,
		TransactionCost: 3.375,
		SlippageBps:     1.0,
		Timestamp:       time.Now(),
	}

	if f.Side.String() != "BUY" {
		t.Errorf("Fill.Side.String() = %q, want %q", f.Side.String(), "BUY")
	}
	if f.Quantity != 0.5 {
		t.Errorf("Fill.Quantity = %v, want 0.5", f.Quantity)
	}
}
