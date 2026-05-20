package journal

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/lgreene03/huginn/internal/model"
)

// JSONLWriter appends model.Fill records and strategy_state envelopes as
// JSONL to a file.
//
// Wire format:
//   - Fill records are written as the bare Fill struct (legacy format; old
//     journal files predate the tagged-record layout and contain only this).
//   - Strategy-state records carry a discriminator: {"type":"strategy_state",
//     "strategy_key":"obi","blob":<inline envelope>,"updated_at":"..."}.
//
// The reader path sniffs the `type` field; records without it are treated as
// Fills (preserves backward compatibility with pre-Phase-1 journals).
type JSONLWriter struct {
	mu   sync.Mutex
	file *os.File
	enc  *json.Encoder
}

// strategyStateRecord is the JSONL envelope carrying a state snapshot.
// The `Blob` payload is the strategy.StateEnvelope JSON produced by
// MarshalState; we keep it as a RawMessage so we don't double-encode.
type strategyStateRecord struct {
	Type        string          `json:"type"`
	StrategyKey string          `json:"strategy_key"`
	Blob        json.RawMessage `json:"blob"`
	UpdatedAt   time.Time       `json:"updated_at"`
}

// NewJSONLWriter creates a new append-only JSONL journal writer.
func NewJSONLWriter(path string) (*JSONLWriter, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}

	return &JSONLWriter{
		file: f,
		enc:  json.NewEncoder(f),
	}, nil
}

// Append serializes and writes a single fill to the journal.
func (w *JSONLWriter) Append(fill model.Fill) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.enc.Encode(fill)
}

// AppendStrategyState writes a tagged state snapshot to the journal.
// Empty blob is a no-op.
func (w *JSONLWriter) AppendStrategyState(key string, blob []byte) error {
	if len(blob) == 0 {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.enc.Encode(strategyStateRecord{
		Type:        "strategy_state",
		StrategyKey: key,
		Blob:        blob,
		UpdatedAt:   time.Now().UTC(),
	})
}

// Close closes the underlying file handle.
func (w *JSONLWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.file.Close()
}
