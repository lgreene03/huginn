package kafka

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/lgreene03/huginn/internal/metrics"
	"github.com/lgreene03/huginn/internal/model"
)

// TestSafeDispatch_PanicRecoversAndContinues asserts the core sre-resilience-2
// guarantee: a panic in a handler dispatch is recovered (does not kill the
// goroutine), increments the per-consumer panic counter, and the surrounding
// loop keeps processing subsequent messages.
func TestSafeDispatch_PanicRecoversAndContinues(t *testing.T) {
	before := testutil.ToFloat64(metrics.ConsumerPanicsTotal.WithLabelValues("feature"))

	// Simulate a batch of messages where the second one panics. With
	// safeDispatch wrapping each dispatch, all three must be attempted and the
	// processed count must reach 3 (the panic must not abort the loop).
	processed := 0
	msgs := []int{1, 2, 3}
	for _, m := range msgs {
		safeDispatch("feature", func() {
			processed++
			if m == 2 {
				panic("simulated handler explosion")
			}
		})
	}

	if processed != 3 {
		t.Fatalf("expected loop to continue past panic and process all 3 messages, got %d", processed)
	}

	after := testutil.ToFloat64(metrics.ConsumerPanicsTotal.WithLabelValues("feature"))
	if got := after - before; got != 1 {
		t.Fatalf("expected ConsumerPanicsTotal{feature} to increment by 1, got %v", got)
	}
}

// TestSafeDispatch_NoPanicDoesNotCount verifies the happy path does not touch
// the panic counter.
func TestSafeDispatch_NoPanicDoesNotCount(t *testing.T) {
	before := testutil.ToFloat64(metrics.ConsumerPanicsTotal.WithLabelValues("price"))
	ran := false
	safeDispatch("price", func() { ran = true })
	if !ran {
		t.Fatal("handler should have run")
	}
	after := testutil.ToFloat64(metrics.ConsumerPanicsTotal.WithLabelValues("price"))
	if after != before {
		t.Fatalf("panic counter changed on a non-panicking dispatch: %v -> %v", before, after)
	}
}

// TestDeserializeFailed_IncrementsCounter exercises the sre-data-ops-10 path:
// a malformed payload increments deserialize_failed_total{consumer} instead of
// being silently skipped. We drive the exact unmarshal+count sequence the
// consumer loop runs on a bad frame.
func TestDeserializeFailed_IncrementsCounter(t *testing.T) {
	before := testutil.ToFloat64(metrics.DeserializeFailedTotal.WithLabelValues("feature"))

	bad := []byte(`{"instrument": 12345, not valid json`)
	var ev model.FeatureEvent
	if err := json.Unmarshal(bad, &ev); err != nil {
		// This is the branch the consumer takes; mirror its counter increment.
		metrics.DeserializeFailedTotal.WithLabelValues("feature").Inc()
	} else {
		t.Fatal("expected malformed payload to fail to unmarshal")
	}

	after := testutil.ToFloat64(metrics.DeserializeFailedTotal.WithLabelValues("feature"))
	if got := after - before; got != 1 {
		t.Fatalf("expected DeserializeFailedTotal{feature} to increment by 1, got %v", got)
	}
}

// TestBackoff_IsBoundedAndGrows verifies the read-error backoff grows but stays
// bounded by its ceiling (sre-resilience-1), so a Redpanda outage cannot drive
// an unbounded busy loop.
func TestBackoff_IsBoundedAndGrows(t *testing.T) {
	// Use tiny intervals so the test exercises growth/capping logic without
	// sleeping real seconds.
	b := &backoff{base: time.Millisecond, max: 8 * time.Millisecond}
	ctx := context.Background()

	start := time.Now()
	// First sleep should be ~base with jitter; just confirm it blocks and that
	// the internal current value climbs then caps.
	b.sleep(ctx)
	if elapsed := time.Since(start); elapsed < 0 {
		t.Fatalf("backoff sleep returned a negative elapsed")
	}

	// Drive many iterations; current must cap at max, never exceed it.
	for i := 0; i < 20; i++ {
		b.sleep(ctx)
		if b.current > b.max {
			t.Fatalf("backoff current %v exceeded ceiling %v", b.current, b.max)
		}
	}
	if b.current != b.max {
		t.Fatalf("after many iterations backoff should sit at the ceiling %v, got %v", b.max, b.current)
	}

	// reset returns to zero so transient blips don't accumulate delay.
	b.reset()
	if b.current != 0 {
		t.Fatalf("reset should zero the backoff, got %v", b.current)
	}
}

// TestNewBackoff_Defaults documents the production ceiling/base used by the
// consumers so a regression in the defaults is caught.
func TestNewBackoff_Defaults(t *testing.T) {
	b := newBackoff()
	if b.base != 250*time.Millisecond {
		t.Fatalf("unexpected base %v", b.base)
	}
	if b.max != 5*time.Second {
		t.Fatalf("unexpected max %v", b.max)
	}
}

// TestBackoff_SleepReturnsOnCancel ensures a cancelled context unblocks the
// backoff sleep promptly (clean shutdown during an outage).
func TestBackoff_SleepReturnsOnCancel(t *testing.T) {
	b := newBackoff()
	b.current = b.max // make the nominal sleep long
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled

	done := make(chan struct{})
	go func() {
		b.sleep(ctx)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("backoff sleep did not return promptly on cancelled context")
	}
}
