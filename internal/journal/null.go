package journal

import "github.com/lgreene03/huginn/internal/model"

// NullWriter is a no-op journal that discards all writes.
// Used by walk-forward validation where per-fold journal output is unnecessary.
type NullWriter struct{}

func NewNullWriter() *NullWriter { return &NullWriter{} }

func (w *NullWriter) Append(_ model.Fill) error                    { return nil }
func (w *NullWriter) AppendStrategyState(_ string, _ []byte) error { return nil }
func (w *NullWriter) Close() error                                 { return nil }
