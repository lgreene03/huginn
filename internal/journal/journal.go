package journal

import "github.com/lgreene03/huginn/internal/model"

// Writer is the pluggable interface for logging simulated order execution fills
// and persisting strategy state across restarts.
type Writer interface {
	// Append writes a single fill record.
	Append(fill model.Fill) error

	// AppendStrategyState writes a snapshot of the active strategy's mutable
	// state, keyed by a stable identifier (typically the config strategy name,
	// e.g. "obi"). Implementations overwrite any previous record for the same
	// key — latest-wins; the design doc rejects an audit-history table.
	//
	// Passing an empty blob is a no-op (used during graceful shutdown when
	// MarshalState returns the zero-state).
	AppendStrategyState(key string, blob []byte) error

	// Close closes any underlying network connections or file handles.
	Close() error
}

// StateLoader is implemented by both the JSONL and Postgres journal backends
// to support restoring strategy state on boot. Returns (nil, nil) when no
// state exists for the given key — the caller should start fresh.
type StateLoader interface {
	LoadStrategyState(key string) ([]byte, error)
}
