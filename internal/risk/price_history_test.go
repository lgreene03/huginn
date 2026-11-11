package risk

import (
	"testing"
	"time"

	"github.com/lgreene03/huginn/internal/model"
	"github.com/lgreene03/huginn/internal/portfolio"
)

// TestPositionLimit_PriceHistoryIsPerInstrument guards the fill-price fallback
// of the vol-scaled position limit against mixing instruments. BTC near 50,000
// and ETH near 3,000 are each perfectly steady below, so neither has any
// volatility of its own. One history shared across instruments reads the gap
// between the two price levels as volatility (std/mean near 0.9, so the limit
// shrinks about ninety-fold) and throttles an ordinary BTC order.
func TestPositionLimit_PriceHistoryIsPerInstrument(t *testing.T) {
	t.Parallel()
	clock := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	m := newManagerWithClock(baseCfg(), 100_000, func() time.Time { return clock })
	snap := portfolio.Snapshot{
		Cash: 100_000, TotalValue: 100_000, Positions: map[string]portfolio.Position{},
	}

	// Ten small fills on each instrument, interleaved, all at steady prices.
	for i := 0; i < 10; i++ {
		btc := model.Fill{Side: model.Buy, Quantity: 0.001, FillPrice: 50_000, Instrument: "BTC-USD"}
		eth := model.Fill{Side: model.Buy, Quantity: 0.01, FillPrice: 3_000, Instrument: "ETH-USD"}
		if !m.Evaluate(btc, snap) || !m.Evaluate(eth, snap) {
			t.Fatalf("warm-up fill %d rejected; the fixture expects small fills to pass", i)
		}
	}

	// 2 BTC is 100,000 of notional against a 200,000 hard limit. BTC's own
	// history has no dispersion, so the limit is unscaled and the fill passes.
	big := model.Fill{Side: model.Buy, Quantity: 2, FillPrice: 50_000, Instrument: "BTC-USD"}
	if !m.Evaluate(big, snap) {
		t.Fatal("BTC fill throttled by volatility computed across BTC and ETH prices; " +
			"recent-price history must be kept per instrument")
	}
}
