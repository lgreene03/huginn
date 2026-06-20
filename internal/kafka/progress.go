package kafka

import (
	"sync/atomic"
	"time"
)

// Progress is a lock-free last-advance timestamp shared between a consumer
// loop and a readiness probe. A consumer calls Mark() each time its loop
// makes forward progress (a message read, deserialized, or committed); the
// /readyz handler calls Stale() to decide whether the loop has wedged.
//
// The zero value is NOT ready until Mark() is first called; construct with
// NewProgress so the initial timestamp reflects process start, giving the
// consumer a full staleness window to connect before /readyz can flip to 503.
type Progress struct {
	// lastUnixNano holds the most recent advance time as Unix nanoseconds.
	// Stored as int64 for atomic access without a mutex on the hot path.
	lastUnixNano atomic.Int64
}

// NewProgress returns a Progress seeded with the current time, so a freshly
// started consumer is considered fresh for one full staleness window before
// any reader has advanced.
func NewProgress() *Progress {
	p := &Progress{}
	p.lastUnixNano.Store(time.Now().UnixNano())
	return p
}

// Mark records that the consumer loop advanced. Safe for concurrent use and
// cheap enough to call on every message.
func (p *Progress) Mark() {
	if p == nil {
		return
	}
	p.lastUnixNano.Store(time.Now().UnixNano())
}

// Last returns the most recent advance time.
func (p *Progress) Last() time.Time {
	if p == nil {
		return time.Time{}
	}
	return time.Unix(0, p.lastUnixNano.Load())
}

// Stale reports whether the loop has not advanced within window. A nil
// Progress or non-positive window is never stale (feature disabled).
func (p *Progress) Stale(window time.Duration) bool {
	if p == nil || window <= 0 {
		return false
	}
	return time.Since(p.Last()) > window
}
