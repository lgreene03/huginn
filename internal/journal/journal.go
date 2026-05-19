package journal

import "github.com/lgreene/huginn/internal/model"

// Writer is the pluggable interface for logging simulated order execution fills.
type Writer interface {
	// Append writes/logs a single fill record.
	Append(fill model.Fill) error
	// Close closes any underlying network connections or file handles.
	Close() error
}
