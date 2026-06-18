package feed

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/lgreene03/huginn/internal/model"
)

func TestSSEDecoder(t *testing.T) {
	d := &sseDecoder{}

	// A comment (keepalive) line yields nothing.
	if _, ok := d.push(":keepalive"); ok {
		t.Fatal("comment line should not complete a frame")
	}
	// event + data, then blank line completes the frame.
	if _, ok := d.push("event: feature"); ok {
		t.Fatal("event line should not complete a frame")
	}
	if _, ok := d.push("data: {\"a\":1}"); ok {
		t.Fatal("data line should not complete a frame")
	}
	frame, ok := d.push("")
	if !ok {
		t.Fatal("blank line after data should complete a frame")
	}
	if frame.event != "feature" || frame.data != `{"a":1}` {
		t.Fatalf("unexpected frame: %+v", frame)
	}

	// Multi-line data is joined with newlines; absent event defaults to "message".
	d.push("data: line1")
	d.push("data: line2")
	frame, ok = d.push("")
	if !ok {
		t.Fatal("expected completed frame for multi-line data")
	}
	if frame.event != "message" || frame.data != "line1\nline2" {
		t.Fatalf("unexpected multi-line frame: %+v", frame)
	}

	// A lone blank line (no data buffered) yields nothing.
	if _, ok := d.push(""); ok {
		t.Fatal("blank line with no data should not complete a frame")
	}
}

func TestDecodeFeatureEvent_ScalarBridgesToNamedKey(t *testing.T) {
	data := `{
		"eventId": "01890000-0000-7000-8000-000000000001",
		"eventTime": "2026-06-04T12:00:00Z",
		"featureName": "obi.1m",
		"featureVersion": "v1",
		"value": 0.83,
		"windowStart": "2026-06-04T11:59:00Z",
		"windowEnd": "2026-06-04T12:00:00Z"
	}`
	ev, err := decodeFeatureEvent(data)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if ev.FeatureName != "obi.1m" || ev.FeatureVersion != "v1" {
		t.Fatalf("unexpected name/version: %+v", ev)
	}
	if got := ev.Values["value"]; got != 0.83 {
		t.Fatalf("value key = %v, want 0.83", got)
	}
	if got := ev.Values["obi"]; got != 0.83 {
		t.Fatalf("leading-segment key obi = %v, want 0.83", got)
	}
	if !ev.EventTime.Equal(time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("unexpected eventTime: %v", ev.EventTime)
	}
}

func TestDecodeFeatureEvent_MapValuesPreservedOverScalar(t *testing.T) {
	// When both a map entry and a scalar exist for the same key, the explicit
	// map entry wins; the scalar still lands under "value".
	data := `{
		"eventId": "01890000-0000-7000-8000-000000000002",
		"eventTime": "2026-06-04T12:00:00Z",
		"featureName": "vwap.1m",
		"featureVersion": "v1",
		"value": 1.0,
		"values": {"vwap": 101.5, "microPrice": 101.6},
		"windowStart": "2026-06-04T11:59:00Z",
		"windowEnd": "2026-06-04T12:00:00Z"
	}`
	ev, err := decodeFeatureEvent(data)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if ev.Values["vwap"] != 101.5 {
		t.Fatalf("map entry vwap should win: got %v", ev.Values["vwap"])
	}
	if ev.Values["microPrice"] != 101.6 {
		t.Fatalf("map entry microPrice = %v", ev.Values["microPrice"])
	}
	if ev.Values["value"] != 1.0 {
		t.Fatalf("scalar should still appear under value: got %v", ev.Values["value"])
	}
}

func TestDecodeFeatureEvent_Malformed(t *testing.T) {
	if _, err := decodeFeatureEvent("{not json"); err == nil {
		t.Fatal("expected error on malformed JSON")
	}
}

// writeFeatureFrame emits one SSE feature frame and flushes it so the client
// reads it immediately rather than at connection close.
func writeFeatureFrame(t *testing.T, w http.ResponseWriter, name string, value float64) {
	t.Helper()
	_, _ = fmt.Fprintf(w, "event: feature\n")
	_, _ = fmt.Fprintf(w, "data: {\"eventId\":\"id-%s\",\"eventTime\":\"2026-06-04T12:00:00Z\",\"featureName\":\"%s\",\"featureVersion\":\"v1\",\"value\":%v,\"windowStart\":\"2026-06-04T11:59:00Z\",\"windowEnd\":\"2026-06-04T12:00:00Z\"}\n\n", name, name, value)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

func TestSSESource_StreamsFeatureEvents(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// A keepalive comment, then two feature frames.
		_, _ = fmt.Fprintf(w, ": keepalive\n\n")
		writeFeatureFrame(t, w, "obi.1m", 0.7)
		writeFeatureFrame(t, w, "vpin.1m", 0.5)
		// Hold the connection open like a real stream until the client leaves.
		<-r.Context().Done()
	}))
	defer srv.Close()

	events := make(chan model.FeatureEvent, 8)
	src := NewSSESource(SSEConfig{BaseURL: srv.URL}, func(ev model.FeatureEvent) {
		events <- ev
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- src.Run(ctx) }()

	got := make(map[string]float64)
	for range 2 {
		select {
		case ev := <-events:
			got[ev.FeatureName] = ev.Values["value"]
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for feature events")
		}
	}
	if got["obi.1m"] != 0.7 || got["vpin.1m"] != 0.5 {
		t.Fatalf("unexpected events: %+v", got)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned error on clean shutdown: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after ctx cancel")
	}
}

func TestSSESource_Reconnects(t *testing.T) {
	var mu sync.Mutex
	conns := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		conns++
		n := conns
		mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if n == 1 {
			// First connection drops immediately to force a reconnect.
			return
		}
		writeFeatureFrame(t, w, "obi.1m", 0.9)
		<-r.Context().Done()
	}))
	defer srv.Close()

	events := make(chan model.FeatureEvent, 4)
	src := NewSSESource(SSEConfig{
		BaseURL:    srv.URL,
		MinBackoff: time.Millisecond,
		MaxBackoff: 10 * time.Millisecond,
	}, func(ev model.FeatureEvent) { events <- ev })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = src.Run(ctx) }()

	select {
	case ev := <-events:
		if ev.Values["value"] != 0.9 {
			t.Fatalf("unexpected event after reconnect: %+v", ev)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for event after reconnect")
	}

	mu.Lock()
	defer mu.Unlock()
	if conns < 2 {
		t.Fatalf("expected at least 2 connection attempts, got %d", conns)
	}
}

func TestSSESource_Non2xxRetriesThenStops(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	src := NewSSESource(SSEConfig{
		BaseURL:    srv.URL,
		MinBackoff: time.Millisecond,
		MaxBackoff: 5 * time.Millisecond,
	}, func(model.FeatureEvent) { t.Fatal("handler should not be called on a 5xx stream") })

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := src.Run(ctx); err != nil {
		t.Fatalf("Run should return nil on ctx cancel even when never connected: %v", err)
	}
}
