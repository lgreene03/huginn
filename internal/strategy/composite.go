package strategy

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/lgreene03/huginn/internal/metrics"
	"github.com/lgreene03/huginn/internal/model"
)

// CompositeStrategy runs a configured, weighted set of Alphas, blends their
// scores into one combined score, and turns that score into signed-position
// entries — reusing the SAME machinery as OBIThreshold: the net-of-cost
// CostHurdle gate, signed netPosition tracking, the per-instrument
// single-position throttle, and the downstream risk path (it just emits Orders;
// the executor's risk manager vets every fill exactly as it does for any other
// strategy).
//
// # Headline
//
// This is the data/signal-extensibility entry point. Adding a new signal is:
// write an Alpha, register it, add it to the composite with a weight. Nothing
// in OBIThreshold or any other strategy changes.
//
// # Decision flow (mirrors OBIThreshold.OnFeature)
//
//  1. Compute every alpha's AlphaScore for the event.
//  2. Blend into one combined score in [-1, 1] (weighted sum or z-score blend,
//     optionally confidence-weighted). Emit per-alpha contributions + the
//     combined score as Prometheus gauges.
//  3. If |combined| <= EntryThreshold, no trade.
//  4. Otherwise side = sign(combined); size = OrderSize (scaled by conviction
//     when SizeByConviction is set).
//  5. Apply the per-instrument single-position throttle and the signed
//     maxPosition cap, exactly like OBIThreshold.
//  6. Apply the net-of-cost CostHurdle gate (inert by default) before any state
//     mutation, exactly like OBIThreshold.
//  7. Emit the entry order; update netPosition + per-instrument position.
//
// # Concurrency
//
// Self-synchronizing via s.mu, per the strategy concurrency contract. Each
// alpha additionally guards its own internal state (see alpha.go). The combined
// score read of each alpha happens under s.mu, so a single-dispatch caller sees
// a consistent blend.
type CompositeStrategy struct {
	mu sync.Mutex

	name      string
	alphas    []WeightedAlpha
	blendMode BlendMode

	// useConfidence multiplies each alpha contribution by its Confidence.
	useConfidence bool

	// entryThreshold is the |combined score| an entry must exceed to fire.
	entryThreshold float64

	// sizeByConviction scales OrderSize by |combined score| (so a stronger
	// blended signal trades larger, capped at OrderSize). Off by default.
	sizeByConviction bool

	OrderSize   float64
	maxPosition float64
	netPosition float64

	positions map[string]*positionEntry

	// costHurdle is the OPT-IN net-of-cost gate, identical in contract to the
	// one OBIThreshold uses. Nil (default) is inert. See cost_hurdle.go.
	costHurdle *CostHurdle
}

// CompositeConfig configures a CompositeStrategy. Zero values fall back to safe
// defaults (see NewCompositeStrategy).
type CompositeConfig struct {
	// Name is the strategy name for logging + metrics labels. Defaults to
	// "Composite".
	Name string
	// Alphas is the weighted set to blend. An empty set yields a strategy that
	// never trades (combined score is always 0).
	Alphas []WeightedAlpha
	// BlendMode selects weighted-sum (default) or z-score blending.
	BlendMode BlendMode
	// UseConfidence weights each contribution by its alpha Confidence.
	UseConfidence bool
	// EntryThreshold is the |combined| an entry must clear. Must be in [0, 1).
	// Defaults to 0.5.
	EntryThreshold float64
	// SizeByConviction scales OrderSize by |combined|.
	SizeByConviction bool
	// OrderSize is the base entry quantity.
	OrderSize float64
	// MaxPosition is the signed net-position cap (typically OrderSize*10).
	MaxPosition float64
}

const defaultCompositeEntryThreshold = 0.5

// NewCompositeStrategy builds a CompositeStrategy from config. Unset fields get
// defaults so a partial config is safe.
func NewCompositeStrategy(cfg CompositeConfig) *CompositeStrategy {
	name := cfg.Name
	if name == "" {
		name = "Composite"
	}
	threshold := cfg.EntryThreshold
	if threshold <= 0 || threshold >= 1 {
		threshold = defaultCompositeEntryThreshold
	}
	maxPos := cfg.MaxPosition
	if maxPos <= 0 {
		maxPos = cfg.OrderSize * 10
	}
	return &CompositeStrategy{
		name:             name,
		alphas:           cfg.Alphas,
		blendMode:        cfg.BlendMode,
		useConfidence:    cfg.UseConfidence,
		entryThreshold:   threshold,
		sizeByConviction: cfg.SizeByConviction,
		OrderSize:        cfg.OrderSize,
		maxPosition:      maxPos,
		positions:        make(map[string]*positionEntry),
	}
}

// NewCompositeFromRegistry assembles a CompositeStrategy by looking up each
// named alpha in the registry and pairing it with the given weight. It is the
// config-driven boot path: a YAML/env list of (alpha name, weight) becomes a
// running composite. Returns an error if any named alpha is missing.
func NewCompositeFromRegistry(reg *AlphaRegistry, weights map[string]float64, cfg CompositeConfig) (*CompositeStrategy, error) {
	if reg == nil {
		return nil, fmt.Errorf("strategy: nil alpha registry")
	}
	weighted := make([]WeightedAlpha, 0, len(weights))
	// Deterministic order: iterate registry Names() so the composite's alpha
	// order is stable regardless of map iteration order.
	for _, name := range reg.Names() {
		w, want := weights[name]
		if !want {
			continue
		}
		a, err := reg.Get(name)
		if err != nil {
			return nil, err
		}
		weighted = append(weighted, WeightedAlpha{Alpha: a, Weight: w})
	}
	// Catch names requested but not registered.
	for name := range weights {
		if _, err := reg.Get(name); err != nil {
			return nil, err
		}
	}
	cfg.Alphas = weighted
	return NewCompositeStrategy(cfg), nil
}

// DefaultCompositeConfig returns a ready-to-run composite configuration that
// blends three bundled alphas over the rich feature event: raw OBI, the
// multi-timeframe momentum blend, and EMA mean-reversion. It is the wiring the
// cmd/{huginn,backtest} "composite" switch cases use, so both binaries build an
// identical default composite. threshold is the |combined score| entry band
// (<=0 or >=1 falls back to the package default); orderSize is the base entry
// quantity (maxPosition derives as orderSize*10, matching the other strategies).
//
// The default set is confidence-weighted so a cold-started EMA alpha (which
// reports zero confidence until warmed) contributes nothing on the first event.
func DefaultCompositeConfig(threshold, orderSize float64) CompositeConfig {
	return CompositeConfig{
		Name: "Composite",
		Alphas: []WeightedAlpha{
			{Alpha: FieldAlpha{AlphaName: "obi", Field: "obi", Scale: 1, Conf: 1}, Weight: 0.5},
			{Alpha: MomentumAlpha{AlphaName: "momentum"}, Weight: 0.3},
			{Alpha: &EMAReversionAlpha{AlphaName: "ema_reversion"}, Weight: 0.2},
		},
		BlendMode:      BlendWeightedSum,
		UseConfidence:  true,
		EntryThreshold: threshold,
		OrderSize:      orderSize,
		MaxPosition:    orderSize * 10,
	}
}

// SetCostHurdle attaches (or clears, with nil) the net-of-cost gate. Same
// contract as OBIThreshold.SetCostHurdle: guarded, nil restores the inert
// default.
func (s *CompositeStrategy) SetCostHurdle(h *CostHurdle) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.costHurdle = h
}

// Name implements Strategy.
func (s *CompositeStrategy) Name() string {
	return fmt.Sprintf("%s[%d alphas, thr=%.2f]", s.name, len(s.alphas), s.entryThreshold)
}

// score computes every alpha's score, blends them, and emits per-alpha
// contribution + combined-score metrics. Caller must hold s.mu. Returns the
// combined score in [-1,1]. With no alphas the combined score is 0.
func (s *CompositeStrategy) score(event model.FeatureEvent) float64 {
	if len(s.alphas) == 0 {
		metrics.CompositeScore.WithLabelValues(s.name).Set(0)
		return 0
	}
	scores := make([]AlphaScore, len(s.alphas))
	for i, wa := range s.alphas {
		scores[i] = wa.Alpha.Compute(event)
	}
	combined, contributions := combineScores(s.alphas, scores, s.blendMode, s.useConfidence)

	for i, wa := range s.alphas {
		metrics.AlphaContribution.WithLabelValues(s.name, wa.Alpha.Name()).Set(contributions[i])
	}
	metrics.CompositeScore.WithLabelValues(s.name).Set(combined)
	return combined
}

// OnFeature implements Strategy.
func (s *CompositeStrategy) OnFeature(event model.FeatureEvent) []model.Order {
	s.mu.Lock()
	defer s.mu.Unlock()

	instrument := event.Instrument
	midPrice := event.Values["midPrice"]

	combined := s.score(event)

	// ── Entry threshold ─────────────────────────────────────────────────
	if combined <= s.entryThreshold && combined >= -s.entryThreshold {
		return nil
	}

	// ── Per-instrument throttle: only 1 open position per instrument ─────
	if _, hasPos := s.positions[instrument]; hasPos {
		return nil
	}

	// side = direction of the blended conviction.
	side := model.Buy
	if combined < 0 {
		side = model.Sell
	}

	// Signed-position cap, identical semantics to OBIThreshold.
	if side == model.Sell && s.netPosition <= -s.maxPosition {
		return nil
	}
	if side == model.Buy && s.netPosition >= s.maxPosition {
		return nil
	}

	// Size: base OrderSize, optionally scaled up by conviction (|combined|).
	qty := s.OrderSize
	if s.sizeByConviction {
		conviction := combined
		if conviction < 0 {
			conviction = -conviction
		}
		qty = s.OrderSize * conviction
	}
	if qty <= 0 {
		return nil
	}

	// ── Net-of-cost signal gate (reused, inert by default) ──────────────
	// signalStrength is the distance of |combined| past the entry threshold,
	// matching how OBIThreshold passes |obi - effectiveThreshold|. Placed
	// BEFORE any state mutation so a suppressed entry leaves state untouched.
	signalStrength := combined - s.entryThreshold
	if side == model.Sell {
		signalStrength = -combined - s.entryThreshold
	}
	if s.costHurdle.Suppress(signalStrength, qty, side, event) {
		metrics.OrdersCostSuppressedTotal.WithLabelValues(s.Name(), side.String()).Inc()
		slog.Debug("Composite entry suppressed by net-of-cost hurdle",
			"instrument", instrument,
			"combined", fmt.Sprintf("%.4f", combined),
			"signal_strength", fmt.Sprintf("%.4f", signalStrength),
		)
		return nil
	}

	order := model.Order{
		Instrument: instrument,
		Side:       side,
		Quantity:   qty,
		LimitPrice: midPrice,
		Reason: fmt.Sprintf(
			"composite score=%.4f %s threshold=%.2f (%d alphas) — %s",
			combined, gtlt(combined, s.entryThreshold), s.entryThreshold, len(s.alphas), side.String(),
		),
		Timestamp: event.EventTime,
	}

	if side == model.Sell {
		s.netPosition -= qty
	} else {
		s.netPosition += qty
	}

	if midPrice > 0 {
		s.positions[instrument] = &positionEntry{
			EntryPrice: midPrice,
			EntryTime:  event.EventTime,
			Qty:        qty,
			Side:       side,
		}
	}

	slog.Info("Composite signal",
		"strategy", s.Name(),
		"action", side.String(),
		"instrument", instrument,
		"combined", fmt.Sprintf("%.4f", combined),
		"qty", fmt.Sprintf("%.8f", qty),
		"net_position", fmt.Sprintf("%.8f", s.netPosition),
	)

	return []model.Order{order}
}

// gtlt renders the comparison direction for the order reason string.
func gtlt(v, thr float64) string {
	if v > thr {
		return ">"
	}
	return "<-"
}

// ── State persistence (Stateful) ────────────────────────────────────────────
//
// Mirrors OBIThreshold's v2 shape so the generic executor restore path (keyed
// on the config strategy name) recovers netPosition + open positions on
// restart with no extra wiring.

type compositeStateV1 struct {
	NetPosition float64                    `json:"net_position"`
	Positions   map[string]positionStateV2 `json:"positions"`
}

// MarshalState implements Stateful.
func (s *CompositeStrategy) MarshalState() ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	positions := make(map[string]positionStateV2, len(s.positions))
	for inst, pos := range s.positions {
		positions[inst] = positionStateV2{
			EntryPrice: pos.EntryPrice,
			EntryTime:  pos.EntryTime.Format(time.RFC3339Nano),
			Qty:        pos.Qty,
			Side:       int(pos.Side),
		}
	}
	return MarshalEnvelope(1, compositeStateV1{
		NetPosition: s.netPosition,
		Positions:   positions,
	})
}

// RestoreState implements Stateful.
func (s *CompositeStrategy) RestoreState(data []byte) error {
	version, fields, err := ParseEnvelope(data)
	if err != nil {
		return err
	}
	if version != 1 {
		return fmt.Errorf("%w: CompositeStrategy got v%d", ErrStateVersionMismatch, version)
	}
	var f compositeStateV1
	if err := json.Unmarshal(fields, &f); err != nil {
		return fmt.Errorf("CompositeStrategy: failed to unmarshal v1 fields: %w", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.netPosition = f.NetPosition
	s.positions = make(map[string]*positionEntry, len(f.Positions))
	for inst, ps := range f.Positions {
		t, _ := time.Parse(time.RFC3339Nano, ps.EntryTime)
		s.positions[inst] = &positionEntry{
			EntryPrice: ps.EntryPrice,
			EntryTime:  t,
			Qty:        ps.Qty,
			Side:       model.Side(ps.Side),
		}
	}
	return nil
}
