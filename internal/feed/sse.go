// Package feed provides feature-event sources for huginn's strategy dispatch
// path beyond the default Kafka consumer (internal/kafka).
//
// SSESource tails muninn's live feature stream — GET /api/v1/features/stream,
// a text/event-stream of FeatureComputedEvent frames (muninn ADR-0009) — and
// maps each frame onto huginn's internal model.FeatureEvent, handing it to the
// same handler the Kafka consumer uses (executor.OnFeature). Because dispatch
// flows through that handler, the existing RISK_STALENESS_TIMEOUT watchdog
// (risk.Manager.OnFeatureSeen) covers the stream path with no extra wiring.
//
// The stream is a live tail with no backfill (see ADR-0009): a client sees
// only events produced after it connects. This source is therefore an
// alternative to the Kafka path, selected by config — not a replacement for
// the warehouse Query API.
package feed

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/lgreene03/huginn/internal/metrics"
	"github.com/lgreene03/huginn/internal/model"
)

const (
	streamPath       = "/api/v1/features/stream"
	featureEventName = "feature"

	defaultMinBackoff = 500 * time.Millisecond
	defaultMaxBackoff = 30 * time.Second
)

// SSEConfig configures an SSESource.
type SSEConfig struct {
	// BaseURL is muninn's host, e.g. "http://localhost:8080". The stream path
	// is appended; a trailing slash is tolerated.
	BaseURL string
	// Feature, when non-empty, restricts the stream to a single feature name
	// via the ?feature= query parameter (e.g. "vwap.1m"). Empty streams all.
	Feature string
	// MinBackoff / MaxBackoff bound the exponential reconnect backoff. Zero
	// values fall back to 500ms / 30s.
	MinBackoff time.Duration
	MaxBackoff time.Duration
}

// SSESource consumes muninn's SSE feature stream and dispatches decoded
// FeatureEvents to a handler.
type SSESource struct {
	cfg     SSEConfig
	client  *http.Client
	handler func(model.FeatureEvent)
}

// NewSSESource builds a source that streams from cfg.BaseURL and calls handler
// for every decoded feature event. handler is typically executor.OnFeature.
func NewSSESource(cfg SSEConfig, handler func(model.FeatureEvent)) *SSESource {
	if cfg.MinBackoff <= 0 {
		cfg.MinBackoff = defaultMinBackoff
	}
	if cfg.MaxBackoff <= 0 {
		cfg.MaxBackoff = defaultMaxBackoff
	}
	return &SSESource{
		cfg: cfg,
		// No client-level timeout: an SSE connection is idle between events and
		// muninn keeps it warm with keepalive comment frames. A request timeout
		// would sever a healthy-but-quiet stream; cancellation is the ctx's job.
		client:  &http.Client{},
		handler: handler,
	}
}

// Run connects and streams until ctx is cancelled, reconnecting with
// exponential backoff on any disconnect or error. It returns nil on a clean
// ctx cancellation.
func (s *SSESource) Run(ctx context.Context) error {
	url := strings.TrimRight(s.cfg.BaseURL, "/") + streamPath
	if s.cfg.Feature != "" {
		url += "?feature=" + s.cfg.Feature
	}
	slog.Info("SSE feature stream source starting", "url", url)

	backoff := s.cfg.MinBackoff
	connectedOnce := false
	for {
		connected, err := s.streamOnce(ctx, url, &connectedOnce)
		metrics.FeatureStreamConnected.Set(0)
		if ctx.Err() != nil {
			slog.Info("SSE feature stream source stopped")
			return nil
		}

		// A session that actually connected and then dropped resets the
		// backoff: the next attempt should be prompt, not penalized by the
		// failures that preceded the good connection. A never-connected
		// attempt grows the backoff toward MaxBackoff.
		if connected {
			backoff = s.cfg.MinBackoff
		}
		slog.Warn("SSE feature stream disconnected; reconnecting after backoff",
			"error", err, "backoff", backoff.String())

		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			slog.Info("SSE feature stream source stopped")
			return nil
		}
		if !connected {
			backoff *= 2
			if backoff > s.cfg.MaxBackoff {
				backoff = s.cfg.MaxBackoff
			}
		}
	}
}

// streamOnce performs a single connect-and-read cycle. It returns connected=true
// if the HTTP request reached a 2xx and the read loop began, regardless of how
// the stream subsequently ended.
func (s *SSESource) streamOnce(ctx context.Context, url string, connectedOnce *bool) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Accept", "text/event-stream")

	resp, err := s.client.Do(req)
	if err != nil {
		return false, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return false, fmt.Errorf("unexpected status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	metrics.FeatureStreamConnected.Set(1)
	if *connectedOnce {
		metrics.FeatureStreamReconnectsTotal.Inc()
		slog.Info("SSE feature stream reconnected", "url", url)
	} else {
		*connectedOnce = true
		slog.Info("SSE feature stream connected", "url", url)
	}

	dec := &sseDecoder{}
	scanner := bufio.NewScanner(resp.Body)
	// Feature frames are small, but a generous cap avoids truncating an
	// oversized map feature into a parse error.
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		frame, ok := dec.push(scanner.Text())
		if !ok || frame.event != featureEventName {
			continue
		}
		event, err := decodeFeatureEvent(frame.data)
		if err != nil {
			slog.Warn("dropping malformed SSE feature frame", "error", err)
			continue
		}
		metrics.FeaturesConsumedTotal.WithLabelValues(event.FeatureName).Inc()
		s.handler(event)
	}
	return true, scanner.Err()
}

// sseFrame is one completed Server-Sent Events frame.
type sseFrame struct {
	event string
	data  string
}

// sseDecoder is an incremental Server-Sent Events line decoder. Fed one line at
// a time (no trailing newline, as bufio.Scanner yields), push returns a
// completed frame when a blank line terminates it. Comment lines (starting with
// ":", e.g. keepalives) and fields other than event/data are ignored per the
// SSE spec. Mirrors muninn-py's _SseDecoder so the two clients agree frame for
// frame.
type sseDecoder struct {
	event string
	data  []string
}

func (d *sseDecoder) push(line string) (sseFrame, bool) {
	if line == "" {
		if len(d.data) == 0 {
			d.event = ""
			return sseFrame{}, false
		}
		frame := sseFrame{event: d.event, data: strings.Join(d.data, "\n")}
		if frame.event == "" {
			frame.event = "message"
		}
		d.event = ""
		d.data = nil
		return frame, true
	}
	if strings.HasPrefix(line, ":") {
		return sseFrame{}, false
	}
	field, value, _ := strings.Cut(line, ":")
	value = strings.TrimPrefix(value, " ")
	switch field {
	case "event":
		d.event = value
	case "data":
		d.data = append(d.data, value)
	}
	return sseFrame{}, false
}

// wireFeatureEvent mirrors muninn's FeatureComputedEvent JSON
// (io.muninn.shared.event.FeatureComputedEvent). Only the fields huginn's
// strategies consume are mapped onto model.FeatureEvent.
type wireFeatureEvent struct {
	EventID        string             `json:"eventId"`
	EventTime      time.Time          `json:"eventTime"`
	FeatureName    string             `json:"featureName"`
	FeatureVersion string             `json:"featureVersion"`
	Value          *float64           `json:"value"`
	Values         map[string]float64 `json:"values"`
	WindowStart    time.Time          `json:"windowStart"`
	WindowEnd      time.Time          `json:"windowEnd"`
}

// decodeFeatureEvent parses one SSE data payload into a huginn FeatureEvent.
//
// muninn emits scalar features in `value` and map features in `values`.
// huginn's strategies look up either a named key (obi/vpin/vwap) or fall back
// to "value" (ema_crossover, vwap_deviation). To bridge the scalar form to
// both, a scalar `value` is written under the literal "value" key and under
// the feature's leading name segment ("obi.1m" → "obi"), so a scalar obi
// feature reaches the OBI strategy. An explicit key already in `values` is
// never overwritten.
func decodeFeatureEvent(data string) (model.FeatureEvent, error) {
	var w wireFeatureEvent
	if err := json.Unmarshal([]byte(data), &w); err != nil {
		return model.FeatureEvent{}, fmt.Errorf("unmarshal feature event: %w", err)
	}

	values := make(map[string]float64, len(w.Values)+2)
	for k, v := range w.Values {
		values[k] = v
	}
	if w.Value != nil {
		values["value"] = *w.Value
		if key := leadingSegment(w.FeatureName); key != "" {
			if _, exists := values[key]; !exists {
				values[key] = *w.Value
			}
		}
	}

	return model.FeatureEvent{
		EventID:        w.EventID,
		EventTime:      w.EventTime,
		FeatureName:    w.FeatureName,
		FeatureVersion: w.FeatureVersion,
		WindowStart:    w.WindowStart,
		WindowEnd:      w.WindowEnd,
		Values:         values,
	}, nil
}

// leadingSegment returns the part of a dotted feature name before the first
// dot ("vwap.1m" → "vwap"), or the whole name if it has none.
func leadingSegment(featureName string) string {
	if i := strings.IndexByte(featureName, '.'); i >= 0 {
		return featureName[:i]
	}
	return featureName
}
