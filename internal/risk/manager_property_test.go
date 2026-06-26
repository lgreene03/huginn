package risk

import (
	"math"
	"testing"
	"testing/quick"
	"time"

	"github.com/lgreene03/huginn/internal/config"
	"github.com/lgreene03/huginn/internal/model"
	"github.com/lgreene03/huginn/internal/portfolio"
)

// Property-based tests (stdlib testing/quick) over the pre-trade risk gate.
// They complement the example-based cases in manager_test.go by asserting the
// gate's structural invariants across randomized inputs.
//
// Note on the "daily-count limit" property from the spec: the Manager has no
// order-count limit — its daily control is a windowed realized-loss gate
// (Evaluate step 2). The monotonicity property tested here is the meaningful
// real one: once intraday loss crosses the limit the gate rejects, and it stays
// rejecting as the loss deepens (monotone in loss). See TestProp_DailyLoss*.

// fixedClock returns a clock function pinned to a single instant so the daily
// window never rolls mid-test (rolling would re-baseline dayStartRealizedPnL).
func fixedClock(t time.Time) func() time.Time {
	return func() time.Time { return t }
}

// flatSnap builds a snapshot with no open positions at a given equity and
// realized PnL — enough to drive the drawdown and daily-loss gates without the
// position-limit path interfering.
func flatSnap(totalValue, realizedPnL float64) portfolio.Snapshot {
	return portfolio.Snapshot{
		Timestamp:   time.Now(),
		Cash:        totalValue,
		Positions:   make(map[string]portfolio.Position),
		RealizedPnL: realizedPnL,
		TotalValue:  totalValue,
	}
}

// TestProp_OverSizeCapAlwaysRejected asserts that any buy whose resulting gross
// notional exceeds the hard position limit is rejected, regardless of the
// (randomized) quantity and price — with vol scaling disabled (no fills seen,
// no market vol) so the effective limit equals the configured hard limit.
func TestProp_OverSizeCapAlwaysRejected(t *testing.T) {
	const hardLimit = 200_000.0
	clock := time.Date(2026, 6, 20, 12, 0, 0, 0, time.UTC)

	f := func(rawQty, rawPrice float64) bool {
		qty := 0.01 + math.Mod(math.Abs(rawQty), 100.0)
		price := 1.0 + math.Mod(math.Abs(rawPrice), 100_000.0)

		// Only consider inputs that genuinely breach the cap; otherwise the
		// property says nothing (skip by returning true).
		if qty*price <= hardLimit {
			return true
		}

		cfg := config.RiskConfig{
			MaxDrawdownPct:    0.99, // effectively disable drawdown gate
			DailyLossLimit:    1e12, // effectively disable daily-loss gate
			PositionLimitHard: hardLimit,
		}
		// Fresh manager per case so recentPrices vol scaling can't accumulate.
		m := newManagerWithClock(cfg, 1_000_000.0, fixedClock(clock))
		fill := model.Fill{
			Side:       model.Buy,
			Quantity:   qty,
			FillPrice:  price,
			Instrument: "BTC-USDT",
			OrderID:    "prop",
		}
		// Equity well above peak so drawdown never trips; flat position.
		snap := flatSnap(1_000_000.0, 0)
		// Over-cap order MUST be rejected.
		return m.Evaluate(fill, snap) == false
	}
	if err := quick.Check(f, &quick.Config{MaxCount: 3000}); err != nil {
		t.Error(err)
	}
}

// TestProp_UnderSizeCapApproved is the complement: a buy strictly within the
// cap (and within all other generous gates) is always approved. This guards
// against an over-eager reject (the cap must be a clean threshold).
func TestProp_UnderSizeCapApproved(t *testing.T) {
	const hardLimit = 200_000.0
	clock := time.Date(2026, 6, 20, 12, 0, 0, 0, time.UTC)

	f := func(rawNotional float64) bool {
		// Notional strictly inside the cap with margin to avoid float edge.
		notional := math.Mod(math.Abs(rawNotional), hardLimit-1.0)
		price := 1_000.0
		qty := notional / price
		if qty <= 0 {
			return true
		}

		cfg := config.RiskConfig{
			MaxDrawdownPct:    0.99,
			DailyLossLimit:    1e12,
			PositionLimitHard: hardLimit,
		}
		m := newManagerWithClock(cfg, 1_000_000.0, fixedClock(clock))
		fill := model.Fill{
			Side:       model.Buy,
			Quantity:   qty,
			FillPrice:  price,
			Instrument: "BTC-USDT",
			OrderID:    "prop",
		}
		snap := flatSnap(1_000_000.0, 0)
		return m.Evaluate(fill, snap) == true
	}
	if err := quick.Check(f, &quick.Config{MaxCount: 3000}); err != nil {
		t.Error(err)
	}
}

// TestProp_HaltBlocksAllOrders asserts that once halted (manual), EVERY order
// is rejected no matter its size, side, or the snapshot — the halt check is the
// first gate in Evaluate and must dominate.
func TestProp_HaltBlocksAllOrders(t *testing.T) {
	clock := time.Date(2026, 6, 20, 12, 0, 0, 0, time.UTC)

	f := func(isBuy bool, rawQty, rawPrice, rawEquity, rawRealized float64) bool {
		cfg := config.RiskConfig{
			MaxDrawdownPct:    0.20,
			DailyLossLimit:    10_000.0,
			PositionLimitHard: 500_000.0,
		}
		m := newManagerWithClock(cfg, 1_000_000.0, fixedClock(clock))
		m.Halt() // manual circuit breaker

		side := model.Sell
		if isBuy {
			side = model.Buy
		}
		fill := model.Fill{
			Side:       side,
			Quantity:   math.Mod(math.Abs(rawQty), 1_000.0),
			FillPrice:  math.Mod(math.Abs(rawPrice), 100_000.0),
			Instrument: "BTC-USDT",
			OrderID:    "prop",
		}
		// Even a generous, healthy snapshot must not get an order through.
		equity := 500_000.0 + math.Mod(math.Abs(rawEquity), 1_000_000.0)
		realized := math.Mod(rawRealized, 100_000.0)
		snap := flatSnap(equity, realized)
		return m.Evaluate(fill, snap) == false
	}
	if err := quick.Check(f, &quick.Config{MaxCount: 3000}); err != nil {
		t.Error(err)
	}
}

// TestProp_DailyLossGateMonotone asserts monotonicity of the daily-loss gate in
// realized loss: for a fixed limit and day window, if an intraday loss L1 is
// rejected then any deeper loss L2 >= L1 is also rejected. The gate never
// "un-rejects" as losses grow within the same day.
func TestProp_DailyLossGateMonotone(t *testing.T) {
	const limit = 5_000.0
	clock := time.Date(2026, 6, 20, 12, 0, 0, 0, time.UTC)

	// evaluate returns whether a fresh manager approves a tiny order given an
	// intraday loss of `loss` (>=0). dayStartRealizedPnL starts at 0 (fresh
	// manager, fixed clock, no window roll), so realized = -loss is the
	// intraday PnL directly.
	evaluate := func(loss float64) bool {
		cfg := config.RiskConfig{
			MaxDrawdownPct:    0.99, // disable drawdown gate
			DailyLossLimit:    limit,
			PositionLimitHard: 1e12, // disable position gate
		}
		m := newManagerWithClock(cfg, 1_000_000.0, fixedClock(clock))
		fill := model.Fill{
			Side:       model.Buy,
			Quantity:   0.001,
			FillPrice:  1_000.0,
			Instrument: "BTC-USDT",
			OrderID:    "prop",
		}
		snap := flatSnap(1_000_000.0, -loss)
		return m.Evaluate(fill, snap)
	}

	f := func(rawL1, rawDelta float64) bool {
		l1 := math.Mod(math.Abs(rawL1), 20_000.0)
		l2 := l1 + math.Mod(math.Abs(rawDelta), 20_000.0) // l2 >= l1

		approved1 := evaluate(l1)
		approved2 := evaluate(l2)

		// Monotone: if the shallower loss was already rejected, the deeper one
		// must also be rejected (rejected == !approved).
		if !approved1 && approved2 {
			return false
		}
		return true
	}
	if err := quick.Check(f, &quick.Config{MaxCount: 3000}); err != nil {
		t.Error(err)
	}
}

// TestProp_DailyLossThreshold pins the exact threshold semantics the
// monotonicity property relies on: a loss strictly past the limit rejects, and
// a loss strictly within it (with all other gates disabled) approves.
func TestProp_DailyLossThreshold(t *testing.T) {
	const limit = 5_000.0
	clock := time.Date(2026, 6, 20, 12, 0, 0, 0, time.UTC)

	mk := func() *Manager {
		cfg := config.RiskConfig{
			MaxDrawdownPct:    0.99,
			DailyLossLimit:    limit,
			PositionLimitHard: 1e12,
		}
		return newManagerWithClock(cfg, 1_000_000.0, fixedClock(clock))
	}
	fill := model.Fill{Side: model.Buy, Quantity: 0.001, FillPrice: 1_000.0, Instrument: "BTC-USDT"}

	// Loss just under the limit: approved.
	if !mk().Evaluate(fill, flatSnap(1_000_000.0, -(limit-1.0))) {
		t.Errorf("loss under daily limit should be approved")
	}
	// Loss past the limit: rejected.
	if mk().Evaluate(fill, flatSnap(1_000_000.0, -(limit+1.0))) {
		t.Errorf("loss past daily limit should be rejected")
	}
}
