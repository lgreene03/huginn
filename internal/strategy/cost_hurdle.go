package strategy

import (
	"math"

	"github.com/lgreene03/huginn/internal/model"
)

// CostHurdle is the net-of-cost signal gate (quant-alpha-1). It suppresses a
// candidate entry unless the strategy's expected per-round-trip edge (in basis
// points) clears K times the round-trip trading cost (also in bps).
//
// # Why this exists
//
// The OBI strategy has a real GROSS edge (~70% win, profit factor ~5.85) but is
// NET NEGATIVE after fees because it over-trades relative to that edge: it takes
// marginal signals whose expected reversion does not cover the round-trip cost
// of crossing the spread twice plus fees and slippage. This gate is the fix —
// it only lets an entry through when the edge model says the move is worth the
// cost.
//
// # Inertness contract (NON-BREAKING DEFAULT)
//
// The zero value of CostHurdle (K == 0) is INERT: Suppress always returns false,
// so existing behaviour and all current tests are unchanged. The gate only ever
// suppresses when K > 0 AND an EdgeModel is attached. A nil *CostHurdle is also
// inert. This is what keeps the feature non-breaking by default.
//
// # Where the cost comes from
//
// The strategy package must not import internal/executor (that would be an
// import cycle), so the round-trip cost is recomputed here from the same
// primitives the executor's Config exposes: TransactionCostBps, a base
// SlippageBps, and the optional square-root market-impact term
// (SlippageImpactK * sqrt(qty/SlippageImpactScale)). These mirror
// executor.Config.effectiveSlippageBps exactly so the gate's cost estimate
// agrees with the cost the simulated fill will actually incur.
type CostHurdle struct {
	// K is the safety multiple on the round-trip cost the expected edge must
	// clear. K == 0 (the default) makes the hurdle inert (never suppresses).
	// K == 1 requires the edge to merely break even on cost; K > 1 demands a
	// margin. Negative K is treated as 0 (inert).
	K float64

	// TransactionCostBps is the per-fill fee in bps (mirrors
	// executor.Config.TransactionCostBps). Charged on both legs of the round
	// trip.
	TransactionCostBps float64

	// SlippageBps is the base (size-independent) slippage per fill in bps
	// (mirrors executor.Config.SlippageBps). Charged on both legs.
	SlippageBps float64

	// SlippageImpactK and SlippageImpactScale parameterise the optional
	// square-root market-impact term, identical to executor.Config. Zero
	// SlippageImpactK disables the term so slippage collapses to the flat
	// SlippageBps.
	SlippageImpactK     float64
	SlippageImpactScale float64

	// Edge maps a candidate entry to its expected per-round-trip edge in bps.
	// When nil the hurdle is inert regardless of K (we cannot estimate edge, so
	// we never suppress — fail open, preserving existing behaviour).
	Edge EdgeModel
}

// EdgeModel estimates the expected per-round-trip edge, in basis points, of a
// candidate entry. It is the half of the gate the cost side is compared
// against. Implementations are pure functions of the signal and event — they
// must not mutate strategy state.
//
// A conservative implementation may return 0 to make the gate suppress every
// trade once K > 0 (no estimated edge ⇒ nothing clears a positive hurdle); the
// bundled DefaultEdgeModel does exactly that so strategies without a bespoke
// model degrade safely rather than trading through an unknown edge. Strategies
// with a real edge model (see OBIEdgeModel) return a meaningful estimate.
type EdgeModel interface {
	// ExpectedEdgeBps returns the expected round-trip edge in basis points for
	// the given entry. signalStrength is a strategy-defined scalar (for OBI:
	// |obi - effectiveThreshold|); event is the feature event the entry is
	// derived from. side is the proposed entry side.
	ExpectedEdgeBps(signalStrength float64, side model.Side, event model.FeatureEvent) float64
}

// DefaultEdgeBpsPerUnit is the fallback linear coefficient mapping OBI signal
// strength to expected reversion in basis points, used when OBI_EDGE_BPS_PER_UNIT
// is not configured. It is intentionally conservative.
//
// Rationale for the default: with a round-trip cost on the order of ~14 bps
// (2 * (TransactionCostBps≈5 + SlippageBps≈2)) and OBI signal strengths typically
// in the 0.05–0.35 range above threshold, a coefficient of 60 bps/unit means a
// signal strength of ~0.23 produces ~14 bps of expected edge — i.e. only clearly
// above-threshold OBI prints clear a K=1 hurdle, which is the intended
// over-trading suppression. This is a defensible default, not a fitted constant;
// the offline obi→bps regression that would refine it lives in muninn-py, and the
// swept COST_HURDLE_K absorbs residual miscalibration. See docs note in the
// quant-alpha-1 report.
const DefaultEdgeBpsPerUnit = 60.0

// OBIEdgeModel maps OBI signal strength (|obi - effectiveThreshold|) to an
// expected mean-reversion edge in bps via a linear coefficient
// (BpsPerUnit, config key OBI_EDGE_BPS_PER_UNIT). Larger imbalance past the
// threshold ⇒ larger expected reversion ⇒ larger edge.
type OBIEdgeModel struct {
	// BpsPerUnit is the linear coefficient: expected_edge_bps =
	// BpsPerUnit * signalStrength. Non-positive falls back to
	// DefaultEdgeBpsPerUnit.
	BpsPerUnit float64
}

// ExpectedEdgeBps implements EdgeModel for OBI. signalStrength is expected to be
// the (non-negative) distance of |obi| past the effective threshold.
func (m OBIEdgeModel) ExpectedEdgeBps(signalStrength float64, _ model.Side, _ model.FeatureEvent) float64 {
	c := m.BpsPerUnit
	if c <= 0 {
		c = DefaultEdgeBpsPerUnit
	}
	if signalStrength < 0 {
		signalStrength = 0
	}
	return c * signalStrength
}

// DefaultEdgeModel is the conservative fallback EdgeModel for strategies without
// a bespoke one. It returns 0 expected edge, so once K > 0 the gate suppresses
// every entry (an unknown edge never clears a positive hurdle). This is the
// deliberately safe default — a strategy opts into trading-through-the-gate only
// by supplying a real EdgeModel. With K == 0 the hurdle is inert regardless, so
// this model never changes behaviour unless the operator both enables the gate
// and declines to model the edge.
type DefaultEdgeModel struct{}

// ExpectedEdgeBps always returns 0 (conservative: no modelled edge).
func (DefaultEdgeModel) ExpectedEdgeBps(_ float64, _ model.Side, _ model.FeatureEvent) float64 {
	return 0
}

// roundTripCostBps returns 2 * (TransactionCostBps + effectiveSlippageBps(qty)),
// plus the expected spread crossing in bps when both bidPrice and askPrice are
// present on the event (else that term is omitted). This mirrors the executor's
// per-fill cost model, doubled for the entry+exit round trip.
func (h *CostHurdle) roundTripCostBps(qty float64, event model.FeatureEvent) float64 {
	slipBps := h.SlippageBps
	if h.SlippageImpactK > 0 {
		scale := h.SlippageImpactScale
		if scale <= 0 {
			scale = 1.0
		}
		slipBps += h.SlippageImpactK * math.Sqrt(math.Abs(qty)/scale)
	}
	perLeg := h.TransactionCostBps + slipBps
	rt := 2 * perLeg

	// Optional spread-crossing term: when the feature event carries a bid/ask
	// touch, an aggressive entry crosses half the spread (the executor already
	// fills buys at the ask and sells at the bid). Add the full spread in bps as
	// a conservative round-trip crossing estimate. Omitted when absent.
	bid, hasBid := event.Values["bidPrice"]
	ask, hasAsk := event.Values["askPrice"]
	if hasBid && hasAsk && bid > 0 && ask > 0 && ask >= bid {
		mid := (ask + bid) / 2
		if mid > 0 {
			spreadBps := (ask - bid) / mid * 10_000.0
			rt += spreadBps
		}
	}
	return rt
}

// Suppress reports whether a candidate entry should be blocked because its
// expected edge does not clear K * round_trip_cost.
//
// Returns false (do not suppress) whenever the hurdle is inert: nil receiver,
// K <= 0, or no EdgeModel attached. This is the non-breaking default path.
//
// When active, it returns true iff:
//
//	expectedEdgeBps < K * roundTripCostBps(qty, event)
//
// signalStrength is the strategy-specific signal magnitude handed to the
// EdgeModel (for OBI, |obi - effectiveThreshold|).
func (h *CostHurdle) Suppress(signalStrength, qty float64, side model.Side, event model.FeatureEvent) bool {
	if h == nil || h.K <= 0 || h.Edge == nil {
		return false
	}
	edgeBps := h.Edge.ExpectedEdgeBps(signalStrength, side, event)
	hurdleBps := h.K * h.roundTripCostBps(qty, event)
	return edgeBps < hurdleBps
}
