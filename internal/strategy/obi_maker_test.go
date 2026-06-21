package strategy

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/lgreene03/huginn/internal/model"
)

// makerTestEvent builds an OBI feature event that clears every entry filter, so
// the strategy emits an entry order for the given obi sign/magnitude. midPrice
// is overridable so a follow-up event can drive an exit (stop-loss/take-profit).
func makerTestEvent(obi, midPrice float64, t time.Time) model.FeatureEvent {
	return model.FeatureEvent{
		EventID:     "maker-evt",
		EventTime:   t,
		FeatureName: "obi",
		Instrument:  "BTC-USDT",
		Values: map[string]float64{
			"obi":           obi,
			"midPrice":      midPrice,
			"momentum":      0.0001,
			"momentum1m":    0.0001,
			"momentum15m":   0.0001,
			"volatility":    0.008,
			"fearGreed":     50,
			"volumeRatio":   1.2,
			"mlScore":       0.5,
			"mlReady":       0,
			"newsSentiment": 0.0,
			"fundingRate":   0.0,
			"oiChange":      0.0,
		},
	}
}

func quietLogsT(t *testing.T) {
	t.Helper()
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
}

// TestOBIEntryLiquidity_DefaultTaker asserts the default (maker lever off) keeps
// both SELL and BUY entry orders at Taker liquidity — unchanged behaviour.
func TestOBIEntryLiquidity_DefaultTaker(t *testing.T) {
	quietLogsT(t)
	base := time.Unix(1_700_000_000, 0)

	// SELL entry: obi > threshold.
	sell := NewOBIThreshold(0.7, 0.001, 0.1)
	orders := sell.OnFeature(makerTestEvent(0.95, 60_000, base))
	if len(orders) != 1 || orders[0].Side != model.Sell {
		t.Fatalf("expected 1 SELL entry, got %+v", orders)
	}
	if orders[0].Liquidity != model.Taker {
		t.Errorf("default SELL entry liquidity = %v, want Taker", orders[0].Liquidity)
	}

	// BUY entry: obi < -threshold.
	buy := NewOBIThreshold(0.7, 0.001, 0.1)
	orders = buy.OnFeature(makerTestEvent(-0.95, 60_000, base))
	if len(orders) != 1 || orders[0].Side != model.Buy {
		t.Fatalf("expected 1 BUY entry, got %+v", orders)
	}
	if orders[0].Liquidity != model.Taker {
		t.Errorf("default BUY entry liquidity = %v, want Taker", orders[0].Liquidity)
	}
}

// TestOBIEntryLiquidity_MakerEnabled asserts that with the lever on, SELL and
// BUY entries are Maker while the subsequent exit (a close) stays Taker.
func TestOBIEntryLiquidity_MakerEnabled(t *testing.T) {
	quietLogsT(t)
	base := time.Unix(1_700_000_000, 0)

	p := DefaultOBIParams()
	p.MakerEntries = true

	// ── SELL entry is Maker ──
	sell := NewOBIThresholdWithParams(0.7, 0.001, 0.1, p)
	orders := sell.OnFeature(makerTestEvent(0.95, 60_000, base))
	if len(orders) != 1 || orders[0].Side != model.Sell {
		t.Fatalf("expected 1 SELL entry, got %+v", orders)
	}
	if orders[0].Liquidity != model.Maker {
		t.Errorf("maker-enabled SELL entry liquidity = %v, want Maker", orders[0].Liquidity)
	}

	// Exit the SELL position via stop-loss (price rises against a short by >0.5%).
	// The resulting close (a BUY) must stay Taker.
	exitTime := base.Add(time.Minute) // past cooldown irrelevant; exit runs first
	exit := sell.OnFeature(makerTestEvent(0.95, 60_600, exitTime))
	if len(exit) != 1 || exit[0].Side != model.Buy {
		t.Fatalf("expected 1 BUY close, got %+v", exit)
	}
	if exit[0].Liquidity != model.Taker {
		t.Errorf("exit/close liquidity = %v, want Taker", exit[0].Liquidity)
	}

	// ── BUY entry is Maker ──
	buy := NewOBIThresholdWithParams(0.7, 0.001, 0.1, p)
	orders = buy.OnFeature(makerTestEvent(-0.95, 60_000, base))
	if len(orders) != 1 || orders[0].Side != model.Buy {
		t.Fatalf("expected 1 BUY entry, got %+v", orders)
	}
	if orders[0].Liquidity != model.Maker {
		t.Errorf("maker-enabled BUY entry liquidity = %v, want Maker", orders[0].Liquidity)
	}

	// Exit the long via stop-loss (price falls against a long by >0.5%); the
	// resulting SELL close must stay Taker.
	exit = buy.OnFeature(makerTestEvent(-0.95, 59_400, exitTime))
	if len(exit) != 1 || exit[0].Side != model.Sell {
		t.Fatalf("expected 1 SELL close, got %+v", exit)
	}
	if exit[0].Liquidity != model.Taker {
		t.Errorf("exit/close liquidity = %v, want Taker", exit[0].Liquidity)
	}
}
