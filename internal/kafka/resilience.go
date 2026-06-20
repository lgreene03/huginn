package kafka

import (
	"context"
	"log/slog"
	"math/rand"
	"time"

	"github.com/lgreene03/huginn/internal/metrics"
)

// backoff produces bounded, jittered sleep durations for a consumer's
// read-error retry loop. Without it a Redpanda outage makes ReadMessage/
// FetchMessage return immediately in a tight loop, pegging a core and
// flooding logs. Each consumer keeps its own backoff value; reset() is
// called after a successful read so transient blips don't accumulate delay.
type backoff struct {
	base    time.Duration
	max     time.Duration
	current time.Duration
}

// newBackoff returns a backoff with sensible defaults (250ms base, 5s ceiling).
func newBackoff() *backoff {
	return &backoff{base: 250 * time.Millisecond, max: 5 * time.Second}
}

// reset clears accumulated delay after a successful read.
func (b *backoff) reset() {
	b.current = 0
}

// sleep blocks for the next backoff interval (with up to ±50% jitter) or
// returns early if ctx is cancelled. The interval doubles each call up to max.
func (b *backoff) sleep(ctx context.Context) {
	if b.current == 0 {
		b.current = b.base
	} else {
		b.current *= 2
		if b.current > b.max {
			b.current = b.max
		}
	}

	// Full +/-50% jitter to avoid a thundering-herd reconnect across the
	// three consumer goroutines when Redpanda comes back.
	jitter := time.Duration(rand.Int63n(int64(b.current))) - b.current/2
	d := b.current + jitter
	if d < 0 {
		d = 0
	}

	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// safeDispatch runs fn, recovering from any panic so one bad message can't
// kill the consumer goroutine (which would leave the container "healthy"
// while trading silently stops). A recovered panic increments
// ConsumerPanicsTotal{consumer} and is logged; the loop then continues.
func safeDispatch(consumer string, fn func()) {
	defer func() {
		if r := recover(); r != nil {
			metrics.ConsumerPanicsTotal.WithLabelValues(consumer).Inc()
			slog.Error("recovered panic in consumer handler dispatch",
				"consumer", consumer,
				"panic", r,
			)
		}
	}()
	fn()
}
