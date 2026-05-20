package strategy

import (
	"encoding/json"
	"errors"
)

// Stateful is an optional interface that strategies may implement to persist
// their in-memory state across process restarts.
//
// The executor checks for this interface via a type assertion. Strategies that
// do not implement it (no `netPosition`, no decayed history) are simply not
// persisted — at no cost. Idiomatic Go: same shape as io.Closer / http.Flusher.
//
// Contract:
//   - MarshalState returns a deterministic, version-tagged snapshot of the
//     strategy's mutable state. The returned bytes must round-trip through
//     RestoreState on the same strategy version.
//   - RestoreState applies a previously-marshalled snapshot. It returns
//     ErrStateVersionMismatch if the embedded schema version is unknown to
//     this binary (older versions should be migrated up via a per-strategy
//     migration registry; newer versions refuse to load — see design doc).
//   - Both methods must be safe to call concurrently with OnFeature on the
//     same strategy instance. Strategies hold their own sync.Mutex.
//
// See docs/STRATEGY_STATE_DESIGN.md for the full design + open questions.
type Stateful interface {
	MarshalState() ([]byte, error)
	RestoreState(data []byte) error
}

// StateEnvelope is the on-wire shape produced by Stateful.MarshalState. The
// outer envelope carries a version number; the inner `Fields` is the
// per-strategy payload, opaque to the executor/journal.
type StateEnvelope struct {
	Version int             `json:"version"`
	Fields  json.RawMessage `json:"fields"`
}

// MarshalEnvelope is a tiny helper for strategies to produce a versioned blob.
func MarshalEnvelope(version int, fields any) ([]byte, error) {
	f, err := json.Marshal(fields)
	if err != nil {
		return nil, err
	}
	return json.Marshal(StateEnvelope{Version: version, Fields: f})
}

// ParseEnvelope is the read-side counterpart. Returns the embedded version
// and the raw fields payload. Caller decides whether to migrate or refuse.
func ParseEnvelope(data []byte) (int, json.RawMessage, error) {
	if len(data) == 0 {
		return 0, nil, ErrStateEmpty
	}
	var env StateEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		return 0, nil, err
	}
	return env.Version, env.Fields, nil
}

// ErrStateEmpty signals that RestoreState was called with no prior state.
// Callers should treat this as "fresh start, no error".
var ErrStateEmpty = errors.New("strategy: no prior state")

// ErrStateVersionMismatch signals that the persisted envelope version is
// unsupported by this binary. The boot path should refuse to start unless the
// operator passes --ignore-strategy-state.
var ErrStateVersionMismatch = errors.New("strategy: unsupported state version")
