// Package strategy defines the pluggable Strategy interface and provides
// concrete implementations for quantitative signal-driven paper trading.
package strategy

import "github.com/lgreene/huginn/internal/model"

// Strategy is the core abstraction for signal-driven order generation.
//
// Every strategy receives a FeatureEvent from Muninn and returns zero or more
// Orders to be paper-executed. Strategies must be stateless across calls or
// manage their own internal state with explicit synchronization.
type Strategy interface {
	// Name returns a human-readable identifier for logging and telemetry.
	Name() string

	// OnFeature processes a single computed feature event and returns
	// zero or more orders to be paper-executed.
	OnFeature(event model.FeatureEvent) []model.Order
}
