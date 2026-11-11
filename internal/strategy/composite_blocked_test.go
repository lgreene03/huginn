package strategy

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/lgreene03/huginn/internal/metrics"
	"github.com/lgreene03/huginn/internal/model"
)

// These tests guard huginn_entries_blocked_total (goal G0.8): every place the
// composite refuses an entry must record exactly one reason, the first gate
// that stopped it, and an entry that goes through must record none. The
// counters are process-global, so each test asserts on deltas and none of
// them runs in parallel.

// blockedCounts reads the current value of every reason's series.
func blockedCounts() map[metrics.BlockReason]float64 {
	out := make(map[metrics.BlockReason]float64, len(metrics.BlockReasons))
	for _, r := range metrics.BlockReasons {
		out[r] = testutil.ToFloat64(metrics.EntriesBlockedTotal.WithLabelValues(string(r)))
	}
	return out
}

// assertBlockedDelta fails unless each reason moved by exactly want[reason]
// (zero for any reason not listed) since before was taken.
func assertBlockedDelta(t *testing.T, before map[metrics.BlockReason]float64, want map[metrics.BlockReason]float64) {
	t.Helper()
	after := blockedCounts()
	for _, r := range metrics.BlockReasons {
		if got := after[r] - before[r]; got != want[r] {
			t.Errorf("huginn_entries_blocked_total{reason=%q} moved by %v, want %v", r, got, want[r])
		}
	}
}

// fixedComposite builds a one-alpha composite with a constant score, an entry
// band of 0.5 and a net-position cap of 1.
func fixedComposite(score, orderSize float64) *CompositeStrategy {
	return NewCompositeStrategy(CompositeConfig{
		Alphas: []WeightedAlpha{
			{Alpha: constAlpha{name: "a", score: AlphaScore{Value: score, Confidence: 1}}, Weight: 1},
		},
		EntryThreshold: 0.5,
		OrderSize:      orderSize,
		MaxPosition:    1,
	})
}

func TestCompositeBlocked_EachGateRecordsItsOwnReason(t *testing.T) {
	btc := evt("BTC", map[string]float64{"midPrice": 100})
	eth := evt("ETH", map[string]float64{"midPrice": 50})

	cases := []struct {
		name   string
		reason metrics.BlockReason
		// setup builds the strategy and runs any events that must pass before
		// the one under test; it returns the strategy and the event to test.
		setup func(t *testing.T) (*CompositeStrategy, model.FeatureEvent)
	}{
		{
			name:   "signal inside the entry band",
			reason: metrics.BlockBelowThreshold,
			setup: func(t *testing.T) (*CompositeStrategy, model.FeatureEvent) {
				return fixedComposite(0.1, 0.01), btc
			},
		},
		{
			name:   "instrument already holds its one position",
			reason: metrics.BlockPositionOpen,
			setup: func(t *testing.T) (*CompositeStrategy, model.FeatureEvent) {
				c := fixedComposite(0.9, 0.01)
				if orders := c.OnFeature(btc); len(orders) != 1 {
					t.Fatalf("first entry: want 1 order, got %d", len(orders))
				}
				return c, btc
			},
		},
		{
			name:   "signed net-position cap reached",
			reason: metrics.BlockMaxPosition,
			setup: func(t *testing.T) (*CompositeStrategy, model.FeatureEvent) {
				c := fixedComposite(0.9, 1)
				if orders := c.OnFeature(btc); len(orders) != 1 {
					t.Fatalf("first entry: want 1 order, got %d", len(orders))
				}
				return c, eth
			},
		},
		{
			name:   "order quantity not positive",
			reason: metrics.BlockZeroSize,
			setup: func(t *testing.T) (*CompositeStrategy, model.FeatureEvent) {
				return fixedComposite(0.9, 0), btc
			},
		},
		{
			name:   "net-of-cost hurdle not cleared",
			reason: metrics.BlockCostHurdle,
			setup: func(t *testing.T) (*CompositeStrategy, model.FeatureEvent) {
				c := fixedComposite(0.9, 0.01)
				c.SetCostHurdle(&CostHurdle{K: 1, TransactionCostBps: 5, SlippageBps: 2, Edge: DefaultEdgeModel{}})
				return c, btc
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, e := tc.setup(t)
			before := blockedCounts()
			if orders := c.OnFeature(e); len(orders) != 0 {
				t.Fatalf("want the entry refused, got %d orders", len(orders))
			}
			assertBlockedDelta(t, before, map[metrics.BlockReason]float64{tc.reason: 1})
		})
	}
}

func TestCompositeBlocked_AcceptedEntryRecordsNothing(t *testing.T) {
	c := fixedComposite(-0.9, 0.01)
	before := blockedCounts()
	if orders := c.OnFeature(evt("BTC", map[string]float64{"midPrice": 100})); len(orders) != 1 {
		t.Fatalf("want 1 order, got %d", len(orders))
	}
	assertBlockedDelta(t, before, nil)
}

// liveCompositeState is the composite's persisted strategy_state row as read
// (SELECT only) from norse-stack's postgres-huginn on 2026-11-11. It holds the
// five long positions opened between 14:39 and 14:49 UTC on 2026-06-22, the
// last fills huginn ever made.
const liveCompositeState = `{"fields": {"positions": {"BTC-USDT": {"qty": 0.001, "side": 0, "entry_time": "2026-06-22T14:44:59Z", "entry_price": 64930.01}, "ETH-USDT": {"qty": 0.001, "side": 0, "entry_time": "2026-06-22T14:39:59Z", "entry_price": 1753.93}, "SOL-USDT": {"qty": 0.001, "side": 0, "entry_time": "2026-06-22T14:49:59Z", "entry_price": 73.5}, "XRP-USDT": {"qty": 0.001, "side": 0, "entry_time": "2026-06-22T14:49:59Z", "entry_price": 1.14}, "DOGE-USDT": {"qty": 0.001, "side": 0, "entry_time": "2026-06-22T14:49:59Z", "entry_price": 0.08}}, "net_position": 0.005}, "version": 1}`

// TestCompositeBlocked_LiveStateRefusesEveryEntry reproduces the cause of
// huginn's silence since 2026-06-22. With the live state restored into the
// composite norse-stack runs (threshold 0.05, order size 0.001), a strong
// signal in either direction on any of the five instruments is refused by the
// one-position-per-instrument throttle. Nothing in the composite ever removes
// a position, so a sell signal cannot even flatten the long it holds.
func TestCompositeBlocked_LiveStateRefusesEveryEntry(t *testing.T) {
	c := NewCompositeStrategy(DefaultCompositeConfig(0.05, 0.001))
	if err := c.RestoreState([]byte(liveCompositeState)); err != nil {
		t.Fatalf("RestoreState: %v", err)
	}

	instruments := []string{"BTC-USDT", "ETH-USDT", "SOL-USDT", "XRP-USDT", "DOGE-USDT"}
	before := blockedCounts()
	for _, inst := range instruments {
		for _, obi := range []float64{0.9, -0.9} {
			e := evt(inst, map[string]float64{"obi": obi, "midPrice": 100})
			if orders := c.OnFeature(e); len(orders) != 0 {
				t.Errorf("%s obi=%v: want refused, got %d orders", inst, obi, len(orders))
			}
		}
	}
	assertBlockedDelta(t, before, map[metrics.BlockReason]float64{
		metrics.BlockPositionOpen: float64(2 * len(instruments)),
	})

	// An instrument the live state does not hold still trades, which pins the
	// refusal on the open positions rather than on the signal or the caps.
	if orders := c.OnFeature(evt("ADA-USDT", map[string]float64{"obi": 0.9, "midPrice": 1})); len(orders) != 1 {
		t.Fatalf("instrument with no open position: want 1 order, got %d", len(orders))
	}
}
