package executor

import (
	"strconv"
	"sync"
	"testing"
)

func TestDedupCache_FirstSeenIsFalseThenTrue(t *testing.T) {
	t.Parallel()
	d := newDedupCache(10)
	if d.Seen("abc") {
		t.Fatalf("first lookup of abc should be unseen")
	}
	d.Mark("abc")
	if !d.Seen("abc") {
		t.Fatalf("after Mark, abc should be Seen")
	}
}

func TestDedupCache_EmptyIDIsNeverSeen(t *testing.T) {
	t.Parallel()
	d := newDedupCache(10)
	d.Mark("") // should be a no-op
	if d.Seen("") {
		t.Fatalf("empty ID should never be deduplicated")
	}
}

func TestDedupCache_FIFOEviction(t *testing.T) {
	t.Parallel()
	d := newDedupCache(3)
	for _, k := range []string{"a", "b", "c"} {
		d.Mark(k)
	}
	if !d.Seen("a") || !d.Seen("b") || !d.Seen("c") {
		t.Fatalf("a/b/c should all be Seen before eviction")
	}
	d.Mark("d") // evicts "a"
	if d.Seen("a") {
		t.Fatalf("a should have been evicted by d")
	}
	if !d.Seen("b") || !d.Seen("c") || !d.Seen("d") {
		t.Fatalf("b/c/d should remain after a is evicted")
	}
}

func TestDedupCache_DoubleMarkIsIdempotent(t *testing.T) {
	t.Parallel()
	d := newDedupCache(3)
	d.Mark("x")
	d.Mark("x")
	d.Mark("x")
	// Inserting two more should not evict x (Mark of an existing key is a no-op).
	d.Mark("y")
	d.Mark("z")
	if !d.Seen("x") {
		t.Fatalf("x should still be Seen after redundant Marks plus two distinct adds")
	}
}

func TestDedupCache_DefaultCapacityWhenZero(t *testing.T) {
	t.Parallel()
	d := newDedupCache(0) // expects default 10_000
	for i := 0; i < 100; i++ {
		d.Mark("k" + strconv.Itoa(i))
	}
	if !d.Seen("k0") || !d.Seen("k99") {
		t.Fatalf("default capacity should comfortably hold 100 entries")
	}
}

func TestDedupCache_ConcurrentSafe(t *testing.T) {
	t.Parallel()
	d := newDedupCache(1000)
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				key := "w" + strconv.Itoa(worker) + "-" + strconv.Itoa(i)
				_ = d.Seen(key)
				d.Mark(key)
				_ = d.Seen(key)
			}
		}(w)
	}
	wg.Wait()
	// No assertion beyond "race detector finds no race" — the test passes if
	// `go test -race` is clean.
}
