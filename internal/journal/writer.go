package journal

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"

	"github.com/lgreene/huginn/internal/model"
)

// JSONLWriter appends model.Fill records as JSONL to a file.
type JSONLWriter struct {
	mu   sync.Mutex
	file *os.File
	enc  *json.Encoder
}

// NewJSONLWriter creates a new append-only JSONL journal writer.
func NewJSONLWriter(path string) (*JSONLWriter, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return nil, err
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
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

// Close closes the underlying file handle.
func (w *JSONLWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.file.Close()
}
