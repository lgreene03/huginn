// Package strategy defines the pluggable Strategy interface and provides
// concrete implementations for quantitative signal-driven paper trading.
//
// # Concurrency contract
//
// The huginn dispatcher (kafka.Consumer → executor.OnFeature) calls OnFeature
// from a single goroutine. Most strategies could therefore omit locking, but
// huginn additionally:
//
//   - persists strategy state from a 5-second ticker goroutine
//     (executor.RunStatePersister → Stateful.MarshalState), and
//   - exposes the dashboard's `/api/strategy/config` PUT (planned Phase 6)
//     which mutates strategy parameters from an HTTP handler goroutine.
//
// Both paths read strategy fields concurrently with the dispatcher. The
// contract is therefore: **all four bundled strategies are self-synchronizing
// via a per-instance sync.Mutex**. New strategies that carry mutable state
// must follow the same pattern — see ema_crossover.go for the canonical shape.
//
// # Strategy life cycle
//
//  1. Constructed from config at boot in cmd/huginn/main.go (NewOBIThreshold,
//     NewVPINBreakout, NewVWAPDeviation, NewEMACrossover).
//  2. If the strategy implements Stateful and a prior state blob exists in
//     the journal, RestoreState is invoked before any OnFeature dispatch.
//  3. OnFeature is called for every consumed feature event.
//  4. After each fill (and on a 5-second ticker) the executor calls
//     MarshalState and persists it under the configured strategy key.
//
// # See also
//
//   - docs/STRATEGY_STATE_DESIGN.md — how state survives restart
//   - docs/ROADMAP.md Phase 2 — calibration story, failure modes
package strategy

import "github.com/lgreene03/huginn/internal/model"

// Strategy is the core abstraction for signal-driven order generation.
//
// Implementations must:
//
//   - Be safe for concurrent OnFeature + Stateful method calls (typically by
//     embedding a sync.Mutex).
//   - Never reach for wall-clock time. Strategy decisions are functions of
//     event time (FeatureEvent.EventTime) — this keeps backtests deterministic
//     and replay-faithful.
//   - Return nil (or an empty slice) when no signal fires. Never block.
type Strategy interface {
	// Name returns a human-readable identifier for logging and telemetry.
	// Typically includes the dominant parameter (e.g. "OBIThreshold(0.70)")
	// so log lines remain self-descriptive when the threshold is retuned.
	Name() string

	// OnFeature processes a single computed feature event and returns
	// zero or more orders to be paper-executed (or, in live mode,
	// forwarded to Sleipnir as intents).
	//
	// OnFeature may be called from concurrent goroutines; implementations
	// must be safe for concurrent use.
	OnFeature(event model.FeatureEvent) []model.Order
}
