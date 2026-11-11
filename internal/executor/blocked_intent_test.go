package executor

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/lgreene03/huginn/internal/config"
	"github.com/lgreene03/huginn/internal/metrics"
	"github.com/lgreene03/huginn/internal/model"
	"github.com/lgreene03/huginn/internal/portfolio"
	"github.com/lgreene03/huginn/internal/risk"
)

// These tests guard the executor's half of the G0.8 instrumentation: a risk
// rejection is recorded as huginn_entries_blocked_total{reason="risk_rejected"}
// and only an intent that actually leaves huginn moves
// huginn_last_order_intent_timestamp_seconds. The metrics are process-global,
// so each test resets the gauge, asserts on counter deltas, and none runs in
// parallel.

// oneBuyStrategy emits a single taker buy on every event.
type oneBuyStrategy struct{}

func (oneBuyStrategy) Name() string { return "one-buy" }

func (oneBuyStrategy) OnFeature(e model.FeatureEvent) []model.Order {
	return []model.Order{{
		Instrument: e.Instrument,
		Side:       model.Buy,
		Quantity:   0.001,
		LimitPrice: e.Values["midPrice"],
		Timestamp:  e.EventTime,
	}}
}

// recordingPublisher counts intents and optionally fails every publish.
type recordingPublisher struct {
	calls int
	err   error
}

func (p *recordingPublisher) PublishIntent(context.Context, model.Order, string) error {
	p.calls++
	return p.err
}

func priceEvent() model.FeatureEvent {
	return model.FeatureEvent{
		Instrument: "BTC-USDT",
		EventTime:  time.Now(),
		Values:     map[string]float64{"midPrice": 100, "microPrice": 100},
	}
}

func haltedRiskManager() *risk.Manager {
	rm := risk.NewManager(config.RiskConfig{}, 1000)
	rm.Halt()
	return rm
}

func riskRejected() float64 {
	return testutil.ToFloat64(metrics.EntriesBlockedTotal.WithLabelValues(string(metrics.BlockRiskRejected)))
}

func lastIntent() float64 {
	return testutil.ToFloat64(metrics.LastOrderIntentTimestampSeconds)
}

// assertIntentBetween fails unless the gauge holds a time in [from, to],
// widened by a millisecond either side for float rounding.
func assertIntentBetween(t *testing.T, from, to time.Time) {
	t.Helper()
	got := lastIntent()
	lo := float64(from.Add(-time.Millisecond).UnixNano()) / 1e9
	hi := float64(to.Add(time.Millisecond).UnixNano()) / 1e9
	if got < lo || got > hi {
		t.Fatalf("last order intent = %v, want between %v and %v", got, from.Unix(), to.Unix())
	}
}

func TestIntentGauge_PaperFillSetsIt(t *testing.T) {
	metrics.LastOrderIntentTimestampSeconds.Set(0)
	exec := New(oneBuyStrategy{}, portfolio.New(1000), nil, nil, Config{}, false, nil, "")

	before := time.Now()
	exec.OnFeature(priceEvent())
	assertIntentBetween(t, before, time.Now())
}

func TestIntentGauge_PaperRiskRejectionCountsAndLeavesGauge(t *testing.T) {
	metrics.LastOrderIntentTimestampSeconds.Set(0)
	exec := New(oneBuyStrategy{}, portfolio.New(1000), nil, haltedRiskManager(), Config{}, false, nil, "")

	rejectedBefore := riskRejected()
	exec.OnFeature(priceEvent())

	if got := riskRejected() - rejectedBefore; got != 1 {
		t.Errorf("risk_rejected moved by %v, want 1", got)
	}
	if got := lastIntent(); got != 0 {
		t.Errorf("last order intent = %v, want 0 (a rejected order is not an intent)", got)
	}
}

func TestIntentGauge_LivePublishSetsIt(t *testing.T) {
	metrics.LastOrderIntentTimestampSeconds.Set(0)
	pub := &recordingPublisher{}
	exec := New(oneBuyStrategy{}, portfolio.New(1000), nil, nil, Config{}, true, pub, "")

	before := time.Now()
	exec.OnFeature(priceEvent())

	if pub.calls != 1 {
		t.Fatalf("publisher called %d times, want 1", pub.calls)
	}
	assertIntentBetween(t, before, time.Now())
}

func TestIntentGauge_LivePublishFailureLeavesGauge(t *testing.T) {
	metrics.LastOrderIntentTimestampSeconds.Set(0)
	pub := &recordingPublisher{err: errors.New("broker down")}
	exec := New(oneBuyStrategy{}, portfolio.New(1000), nil, nil, Config{}, true, pub, "")

	exec.OnFeature(priceEvent())

	if pub.calls != 1 {
		t.Fatalf("publisher called %d times, want 1", pub.calls)
	}
	if got := lastIntent(); got != 0 {
		t.Errorf("last order intent = %v, want 0 (the intent never reached the gateway)", got)
	}
}

func TestIntentGauge_LiveRiskRejectionCountsAndSkipsPublish(t *testing.T) {
	metrics.LastOrderIntentTimestampSeconds.Set(0)
	pub := &recordingPublisher{}
	exec := New(oneBuyStrategy{}, portfolio.New(1000), nil, haltedRiskManager(), Config{}, true, pub, "")

	rejectedBefore := riskRejected()
	exec.OnFeature(priceEvent())

	if got := riskRejected() - rejectedBefore; got != 1 {
		t.Errorf("risk_rejected moved by %v, want 1", got)
	}
	if pub.calls != 0 {
		t.Errorf("publisher called %d times, want 0", pub.calls)
	}
	if got := lastIntent(); got != 0 {
		t.Errorf("last order intent = %v, want 0", got)
	}
}
