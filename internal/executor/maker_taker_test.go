package executor

import (
	"math"
	"testing"
	"time"

	"github.com/lgreene03/huginn/internal/model"
	"github.com/lgreene03/huginn/internal/portfolio"
	"github.com/lgreene03/huginn/internal/strategy"
)

// TestFeeBps_DefaultsToTransactionCost verifies that with the maker/taker fees
// left at zero, both liquidities resolve to the legacy TransactionCostBps —
// i.e. the single-fee behaviour is preserved unless explicitly configured.
func TestFeeBps_DefaultsToTransactionCost(t *testing.T) {
	c := Config{TransactionCostBps: 8}
	if got := c.feeBps(model.Taker); got != 8 {
		t.Errorf("taker fee = %v, want fallback 8", got)
	}
	if got := c.feeBps(model.Maker); got != 8 {
		t.Errorf("maker fee = %v, want fallback 8", got)
	}
}

// TestFeeBps_OverridesAndRebate verifies explicit per-liquidity fees override
// TransactionCostBps and that a negative maker fee (rebate) is honoured rather
// than treated as unset.
func TestFeeBps_OverridesAndRebate(t *testing.T) {
	c := Config{TransactionCostBps: 8, MakerFeeBps: -2, TakerFeeBps: 12}
	if got := c.feeBps(model.Taker); got != 12 {
		t.Errorf("taker fee = %v, want 12", got)
	}
	if got := c.feeBps(model.Maker); got != -2 {
		t.Errorf("maker rebate = %v, want -2 (negative honoured)", got)
	}
}

// TestSimulateFill_TakerUnchanged confirms the default taker path is byte-for-byte
// the legacy behaviour: crosses the spread, pays TransactionCostBps, and tags
// the fill Taker.
func TestSimulateFill_TakerUnchanged(t *testing.T) {
	p := portfolio.New(100_000)
	e := New(strategy.NewOBIThreshold(0.6, 0.01, 0.1), p, nil, nil, Config{
		TransactionCostBps: 10,
		SlippageBps:        5,
	}, false, nil, "")

	order := model.Order{Instrument: "BTC-USDT", Side: model.Buy, Quantity: 2}
	ev := model.FeatureEvent{Values: map[string]float64{"microPrice": 100}}
	fill := e.simulateFill(order, ev)

	// Buy crosses at 100*(1+5bps) = 100.05
	if math.Abs(fill.FillPrice-100.05) > 1e-9 {
		t.Errorf("taker fill price = %v, want 100.05", fill.FillPrice)
	}
	// tx cost = price * qty * 10bps
	wantCost := 100.05 * 2 * (10.0 / 10_000.0)
	if math.Abs(fill.TransactionCost-wantCost) > 1e-9 {
		t.Errorf("taker tx cost = %v, want %v", fill.TransactionCost, wantCost)
	}
	if fill.Liquidity != model.Taker {
		t.Errorf("default fill liquidity = %v, want Taker", fill.Liquidity)
	}
}

// makerEvent builds a feature event carrying a book quote and an explicit mid.
func makerEvent(ts time.Time, bid, ask, mid float64) model.FeatureEvent {
	return model.FeatureEvent{
		Instrument: "BTC-USDT",
		EventTime:  ts,
		Values: map[string]float64{
			"bidPrice":   bid,
			"askPrice":   ask,
			"midPrice":   mid,
			"microPrice": mid,
		},
	}
}

// TestMaker_FillsOnlyOnThroughTrade verifies a maker buy rests at the bid and
// fills only when a later event's mid trades down through that level, paying
// the maker fee and crossing no spread.
func TestMaker_FillsOnlyOnThroughTrade(t *testing.T) {
	p := portfolio.New(100_000)
	// Permissive strategy is irrelevant — we drive orders directly through the
	// paper branch by parking a maker and stepping events.
	e := New(strategy.NewOBIThreshold(0.6, 0.01, 0.1), p, nil, nil, Config{
		TransactionCostBps: 10,
		MakerFeeBps:        1,
		TakerFeeBps:        10,
		SlippageBps:        5,
	}, false, nil, "")

	t0 := time.Date(2026, 5, 10, 14, 0, 0, 0, time.UTC)
	// Rest a maker buy at the bid (100). restPriceForMaker uses bidPrice.
	ev0 := makerEvent(t0, 100, 101, 100.5)
	order := model.Order{Instrument: "BTC-USDT", Side: model.Buy, Quantity: 3, Liquidity: model.Maker}
	rest, ok := e.restPriceForMaker(order, ev0)
	if !ok || rest != 100 {
		t.Fatalf("rest price = %v ok=%v, want 100 true", rest, ok)
	}
	e.restingMakers = append(e.restingMakers, restingMaker{order: order, restPrice: 100})

	// Event with mid still ABOVE the resting bid: no through-trade, stays parked.
	e.tryFillRestingMakers(makerEvent(t0.Add(time.Minute), 100.2, 101.2, 100.7))
	if len(e.restingMakers) != 1 {
		t.Fatalf("maker should still be resting (mid above bid), have %d", len(e.restingMakers))
	}
	if p.Snapshot().TotalFills != 0 {
		t.Fatalf("no fill expected before through-trade, got %d", p.Snapshot().TotalFills)
	}

	// Event with mid AT/BELOW the resting bid: through-trade -> fill.
	e.tryFillRestingMakers(makerEvent(t0.Add(2*time.Minute), 99.5, 100.5, 99.8))
	if len(e.restingMakers) != 0 {
		t.Fatalf("maker should have filled, still resting %d", len(e.restingMakers))
	}
	snap := p.Snapshot()
	if snap.TotalFills != 1 {
		t.Fatalf("expected 1 maker fill, got %d", snap.TotalFills)
	}
	pos := snap.Positions["BTC-USDT"]
	if math.Abs(pos.Quantity-3) > 1e-9 {
		t.Errorf("maker filled qty = %v, want 3", pos.Quantity)
	}
	// Filled at the resting price (100), not crossing the spread. The portfolio
	// folds the maker fee into average cost: 100 + (100*3*1bps)/3 = 100.01.
	wantAvg := 100 + (100*3*(1.0/10_000.0))/3
	if math.Abs(pos.AverageCost-wantAvg) > 1e-6 {
		t.Errorf("maker avg cost = %v, want %v (rested at bid + maker fee)", pos.AverageCost, wantAvg)
	}
}

// TestMaker_PaysMakerFee checks the through-trade fill charges the maker fee,
// not the taker fee, and records zero slippage.
func TestMaker_PaysMakerFee(t *testing.T) {
	p := portfolio.New(100_000)
	e := New(strategy.NewOBIThreshold(0.6, 0.01, 0.1), p, nil, nil, Config{
		TransactionCostBps: 10,
		MakerFeeBps:        2,
		TakerFeeBps:        10,
		SlippageBps:        5,
	}, false, nil, "")

	rm := restingMaker{
		order:     model.Order{Instrument: "BTC-USDT", Side: model.Buy, Quantity: 4, Liquidity: model.Maker},
		restPrice: 50,
	}
	fill := e.makerFill(rm, model.FeatureEvent{EventTime: time.Now()})

	if fill.Liquidity != model.Maker {
		t.Errorf("fill liquidity = %v, want Maker", fill.Liquidity)
	}
	if fill.SlippageBps != 0 {
		t.Errorf("maker slippage = %v, want 0", fill.SlippageBps)
	}
	if fill.FillPrice != 50 {
		t.Errorf("maker fill price = %v, want 50 (rest price)", fill.FillPrice)
	}
	// maker fee = price * qty * 2bps = 50 * 4 * 0.0002 = 0.04
	wantCost := 50.0 * 4 * (2.0 / 10_000.0)
	if math.Abs(fill.TransactionCost-wantCost) > 1e-9 {
		t.Errorf("maker tx cost = %v, want %v (maker fee, not taker)", fill.TransactionCost, wantCost)
	}
}

// TestMaker_SellRestsAtAsk verifies a maker sell rests at the ask and fills when
// the mid trades up through that level.
func TestMaker_SellRestsAtAsk(t *testing.T) {
	p := portfolio.New(100_000)
	e := New(strategy.NewOBIThreshold(0.6, 0.01, 0.1), p, nil, nil, Config{
		TransactionCostBps: 10,
		MakerFeeBps:        1,
		TakerFeeBps:        10,
	}, false, nil, "")

	t0 := time.Date(2026, 5, 10, 14, 0, 0, 0, time.UTC)
	order := model.Order{Instrument: "BTC-USDT", Side: model.Sell, Quantity: 2, Liquidity: model.Maker}
	rest, ok := e.restPriceForMaker(order, makerEvent(t0, 100, 101, 100.5))
	if !ok || rest != 101 {
		t.Fatalf("sell rest price = %v ok=%v, want 101 true", rest, ok)
	}
	e.restingMakers = append(e.restingMakers, restingMaker{order: order, restPrice: 101})

	// Mid below ask: stays parked.
	e.tryFillRestingMakers(makerEvent(t0.Add(time.Minute), 100.4, 100.9, 100.6))
	if len(e.restingMakers) != 1 {
		t.Fatalf("sell maker should still rest, have %d", len(e.restingMakers))
	}
	// Mid at/above ask: through-trade -> fill (short position).
	e.tryFillRestingMakers(makerEvent(t0.Add(2*time.Minute), 101.5, 102.5, 101.5))
	snap := p.Snapshot()
	if snap.TotalFills != 1 {
		t.Fatalf("expected 1 maker sell fill, got %d", snap.TotalFills)
	}
	pos := snap.Positions["BTC-USDT"]
	if math.Abs(pos.Quantity-(-2)) > 1e-9 {
		t.Errorf("maker sell qty = %v, want -2 (short)", pos.Quantity)
	}
}

// TestMaker_OnFeatureIntegration drives a maker order through OnFeature: it is
// parked on the emitting event, then fills on a later through-trade event.
func TestMaker_OnFeatureIntegration(t *testing.T) {
	p := portfolio.New(100_000)
	s := &fixedMakerStrategy{}
	e := New(s, p, nil, nil, Config{
		TransactionCostBps: 10,
		MakerFeeBps:        1,
		TakerFeeBps:        10,
	}, false, nil, "")

	t0 := time.Date(2026, 5, 10, 14, 0, 0, 0, time.UTC)
	// First event: strategy emits a maker buy -> parked, no fill yet.
	s.emit = true
	e.OnFeature(makerEvent(t0, 100, 101, 100.5))
	if p.Snapshot().TotalFills != 0 {
		t.Fatalf("maker should be parked, not filled on emit event")
	}
	if len(e.restingMakers) != 1 {
		t.Fatalf("expected 1 resting maker, got %d", len(e.restingMakers))
	}

	// Second event: no new order, but mid trades through the resting bid -> fill.
	s.emit = false
	e.OnFeature(makerEvent(t0.Add(time.Minute), 99, 100, 99.5))
	snap := p.Snapshot()
	if snap.TotalFills != 1 {
		t.Fatalf("expected maker fill on through-trade, got %d fills", snap.TotalFills)
	}
}

// fixedMakerStrategy emits a single maker buy when emit is set. Implements the
// strategy.Strategy interface minimally for the integration test.
type fixedMakerStrategy struct{ emit bool }

func (f *fixedMakerStrategy) Name() string { return "fixed-maker" }
func (f *fixedMakerStrategy) OnFeature(ev model.FeatureEvent) []model.Order {
	if !f.emit {
		return nil
	}
	return []model.Order{{
		Instrument: ev.Instrument,
		Side:       model.Buy,
		Quantity:   1,
		Liquidity:  model.Maker,
	}}
}
