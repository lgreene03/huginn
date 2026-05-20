package executor

import "sync"

// dedupCache is a bounded set-with-FIFO-eviction used by OnExecutionFill to
// drop duplicate fills that arrive when sleipnir's boot reconciliation
// re-publishes an event the live WS path already delivered.
//
// The cache is keyed on Fill.ExecutionID (a sleipnir-supplied per-fill identity
// — see sleipnir/docs/CONTRACTS.md). At ~1000 fills/minute steady state, a
// 10_000-entry window covers ~10 minutes of history — more than enough to span
// any sleipnir restart-and-reconcile cycle.
//
// Empty ExecutionID is treated as "not deduplicable" — Seen returns false,
// Mark is a no-op. This preserves paper-mode behavior, where the executor
// produces fills locally without going through sleipnir and therefore has
// no ExecutionID to assign.
type dedupCache struct {
	mu       sync.Mutex
	capacity int
	seen     map[string]struct{}
	order    []string // FIFO ring; oldest at index 0
}

func newDedupCache(capacity int) *dedupCache {
	if capacity <= 0 {
		capacity = 10_000
	}
	return &dedupCache{
		capacity: capacity,
		seen:     make(map[string]struct{}, capacity),
		order:    make([]string, 0, capacity),
	}
}

// Seen reports whether the executionID has already been observed.
// Empty IDs are never "seen".
func (d *dedupCache) Seen(executionID string) bool {
	if executionID == "" {
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	_, ok := d.seen[executionID]
	return ok
}

// Mark records the executionID. If the cache is at capacity, the oldest
// entry is evicted. Empty IDs are ignored.
func (d *dedupCache) Mark(executionID string) {
	if executionID == "" {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.seen[executionID]; ok {
		return
	}
	if len(d.order) >= d.capacity {
		oldest := d.order[0]
		d.order = d.order[1:]
		delete(d.seen, oldest)
	}
	d.order = append(d.order, executionID)
	d.seen[executionID] = struct{}{}
}
