package risk

import (
	"testing"
	"time"

	"github.com/lgreene03/huginn/internal/model"
	"github.com/lgreene03/huginn/internal/portfolio"
)

// TestPositionLimit_UsesMarketVolatilityWhenPresent verifies quant-14: when a
// market volatility feature has been observed via OnFeatureSeen, the vol-scaled
// position limit derives its scale from that feature rather than from the
// strategy's own fill prices (which, on the very first fill, carry no
// dispersion and would leave the limit unscaled).
func TestPositionLimit_UsesMarketVolatilityWhenPresent(t *testing.T) {
	t.Parallel()
	cfg := baseCfg()
	// Hard limit chosen so an unscaled fill passes but a vol-scaled one fails.
	// Fill: qty 1500 * price 100 = 150_000 gross notional.
	// With market vol 0.02: volScale = 1/(1+100*0.02) = 1/3 ≈ 0.333.
	// effective limit = 200_000 * 0.333 ≈ 66_667 < 150_000 => rejected.
	m := NewManager(cfg, 100_000)

	// Observe a market volatility feature.
	m.OnFeatureSeen(time.Now(), 0.02)

	fill := model.Fill{Side: model.Buy, Quantity: 1500, FillPrice: 100, Instrument: "BTC-USD", OrderID: "o1"}
	snap := portfolio.Snapshot{
		Cash: 100_000, TotalValue: 100_000, Positions: map[string]portfolio.Position{},
	}
	if m.Evaluate(fill, snap) {
		t.Fatal("expected fill rejected by market-vol-scaled position limit, but it was approved")
	}
}

// TestPositionLimit_FallsBackToFillPricesWithoutMarketVol verifies the legacy
// path still applies when no market volatility feature is available: a single
// fill (no dispersion, fewer than 10 prices) leaves volScale at 1.0 so the
// same notional that the market-vol case rejected is approved here.
func TestPositionLimit_FallsBackToFillPricesWithoutMarketVol(t *testing.T) {
	t.Parallel()
	cfg := baseCfg()
	m := NewManager(cfg, 100_000)

	// No OnFeatureSeen with volatility => marketVolatility stays 0, and with a
	// single fill price there is no dispersion-based scaling yet.
	fill := model.Fill{Side: model.Buy, Quantity: 1500, FillPrice: 100, Instrument: "BTC-USD", OrderID: "o1"}
	snap := portfolio.Snapshot{
		Cash: 100_000, TotalValue: 100_000, Positions: map[string]portfolio.Position{},
	}
	// 150_000 gross notional < 200_000 hard limit, unscaled => approved.
	if !m.Evaluate(fill, snap) {
		t.Fatal("expected fill approved under unscaled fallback limit, but it was rejected")
	}
}

// TestOnFeatureSeen_ZeroVolDoesNotClobber ensures a later event missing the
// volatility feature does not wipe a previously observed value.
func TestOnFeatureSeen_ZeroVolDoesNotClobber(t *testing.T) {
	t.Parallel()
	m := NewManager(baseCfg(), 100_000)
	m.OnFeatureSeen(time.Now(), 0.02)
	m.OnFeatureSeen(time.Now(), 0) // missing feature on this event

	m.mu.RLock()
	got := m.marketVolatility
	m.mu.RUnlock()
	if got != 0.02 {
		t.Errorf("marketVolatility = %v, want retained 0.02", got)
	}
}
