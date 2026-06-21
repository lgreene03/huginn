package strategy

import (
	"math"
	"testing"

	"github.com/lgreene03/huginn/internal/model"
)

func ev(values map[string]float64) model.FeatureEvent {
	return model.FeatureEvent{Instrument: "BTC-USDT", Values: values}
}

func TestCostHurdle_InertWhenKZero(t *testing.T) {
	h := &CostHurdle{
		K:                  0, // inert
		TransactionCostBps: 5,
		SlippageBps:        2,
		Edge:               OBIEdgeModel{BpsPerUnit: 60},
	}
	// Even a zero-edge signal must NOT be suppressed when K == 0.
	if h.Suppress(0, 0.01, model.Buy, ev(nil)) {
		t.Fatal("K==0 must be inert (never suppress)")
	}
}

func TestCostHurdle_NilReceiverInert(t *testing.T) {
	var h *CostHurdle // nil
	if h.Suppress(0, 0.01, model.Buy, ev(nil)) {
		t.Fatal("nil *CostHurdle must be inert")
	}
}

func TestCostHurdle_InertWhenNoEdgeModel(t *testing.T) {
	h := &CostHurdle{K: 5, TransactionCostBps: 5, SlippageBps: 2, Edge: nil}
	if h.Suppress(0.5, 0.01, model.Buy, ev(nil)) {
		t.Fatal("missing EdgeModel must fail open (never suppress)")
	}
}

func TestCostHurdle_SuppressesMarginalEdge(t *testing.T) {
	// Round-trip cost = 2*(5+2) = 14 bps. K=1 ⇒ hurdle 14 bps.
	h := &CostHurdle{
		K:                  1,
		TransactionCostBps: 5,
		SlippageBps:        2,
		Edge:               OBIEdgeModel{BpsPerUnit: 60},
	}
	// signalStrength 0.1 ⇒ edge 6 bps < 14 ⇒ suppress.
	if !h.Suppress(0.1, 0.01, model.Buy, ev(nil)) {
		t.Fatal("marginal-edge entry (6bps) below 14bps hurdle must be suppressed")
	}
}

func TestCostHurdle_LetsThroughClearingEdge(t *testing.T) {
	// Round-trip cost = 14 bps, K=1.
	h := &CostHurdle{
		K:                  1,
		TransactionCostBps: 5,
		SlippageBps:        2,
		Edge:               OBIEdgeModel{BpsPerUnit: 60},
	}
	// signalStrength 0.3 ⇒ edge 18 bps ≥ 14 ⇒ allow.
	if h.Suppress(0.3, 0.01, model.Buy, ev(nil)) {
		t.Fatal("entry whose edge (18bps) clears the 14bps hurdle must fire")
	}
}

func TestCostHurdle_HighKSuppressesEvenStrongSignal(t *testing.T) {
	// K=3 ⇒ hurdle 42 bps; edge 18 bps ⇒ suppress.
	h := &CostHurdle{
		K:                  3,
		TransactionCostBps: 5,
		SlippageBps:        2,
		Edge:               OBIEdgeModel{BpsPerUnit: 60},
	}
	if !h.Suppress(0.3, 0.01, model.Buy, ev(nil)) {
		t.Fatal("high K must raise the hurdle above an 18bps edge")
	}
}

func TestCostHurdle_SpreadCrossingAddedWhenBidAskPresent(t *testing.T) {
	h := &CostHurdle{K: 1, TransactionCostBps: 5, SlippageBps: 2, Edge: OBIEdgeModel{BpsPerUnit: 60}}
	// Without bid/ask: cost 14 bps.
	bare := h.roundTripCostBps(0.01, ev(nil))
	if math.Abs(bare-14) > 1e-9 {
		t.Fatalf("bare round-trip cost = %.4f, want 14", bare)
	}
	// bid=100, ask=100.1 ⇒ mid 100.05, spread ≈ 9.995 bps added.
	withSpread := h.roundTripCostBps(0.01, ev(map[string]float64{"bidPrice": 100, "askPrice": 100.1}))
	if withSpread <= bare {
		t.Fatalf("spread crossing should increase cost: bare=%.4f with=%.4f", bare, withSpread)
	}
	wantSpreadBps := (100.1 - 100.0) / 100.05 * 10_000.0
	if math.Abs((withSpread-bare)-wantSpreadBps) > 1e-6 {
		t.Fatalf("spread term = %.4f, want %.4f", withSpread-bare, wantSpreadBps)
	}
}

func TestCostHurdle_ImpactTermGrowsWithSize(t *testing.T) {
	h := &CostHurdle{
		K:                   1,
		TransactionCostBps:  5,
		SlippageBps:         2,
		SlippageImpactK:     10,
		SlippageImpactScale: 1,
		Edge:                OBIEdgeModel{BpsPerUnit: 60},
	}
	small := h.roundTripCostBps(0.01, ev(nil))
	big := h.roundTripCostBps(4.0, ev(nil))
	if big <= small {
		t.Fatalf("impact term must grow with qty: small=%.4f big=%.4f", small, big)
	}
}

func TestOBIEdgeModel_LinearAndDefault(t *testing.T) {
	// Explicit coefficient.
	m := OBIEdgeModel{BpsPerUnit: 50}
	if got := m.ExpectedEdgeBps(0.2, model.Buy, ev(nil)); math.Abs(got-10) > 1e-9 {
		t.Fatalf("edge = %.4f, want 10", got)
	}
	// Non-positive coefficient falls back to default.
	d := OBIEdgeModel{BpsPerUnit: 0}
	if got := d.ExpectedEdgeBps(1.0, model.Buy, ev(nil)); math.Abs(got-DefaultEdgeBpsPerUnit) > 1e-9 {
		t.Fatalf("default edge = %.4f, want %.4f", got, DefaultEdgeBpsPerUnit)
	}
	// Negative signal strength clamps to 0.
	if got := m.ExpectedEdgeBps(-1, model.Buy, ev(nil)); got != 0 {
		t.Fatalf("negative signal strength must yield 0 edge, got %.4f", got)
	}
}

func TestDefaultEdgeModel_SuppressesEverythingWhenActive(t *testing.T) {
	h := &CostHurdle{K: 1, TransactionCostBps: 5, SlippageBps: 2, Edge: DefaultEdgeModel{}}
	// Zero modelled edge ⇒ any positive hurdle suppresses.
	if !h.Suppress(999, 0.01, model.Buy, ev(nil)) {
		t.Fatal("DefaultEdgeModel (0 edge) must be suppressed by a positive hurdle")
	}
}
