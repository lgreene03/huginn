package strategy

import (
	"testing"
	"time"

	"github.com/lgreene03/huginn/internal/model"
)

func TestOBIThreshold_BuyOnNegativeOBI(t *testing.T) {
	s := NewOBIThreshold(0.7, 0.01, 0.1)

	event := model.FeatureEvent{
		EventTime:  time.Now(),
		Instrument: "BTC-USDT",
		Values:     map[string]float64{"obi": -0.85},
	}

	orders := s.OnFeature(event)
	if len(orders) != 1 {
		t.Fatalf("expected 1 order, got %d", len(orders))
	}
	if orders[0].Side != model.Buy {
		t.Errorf("expected BUY, got %s", orders[0].Side.String())
	}
	if orders[0].Quantity != 0.01 {
		t.Errorf("expected qty 0.01, got %.8f", orders[0].Quantity)
	}
}

func TestOBIThreshold_SellOnPositiveOBI(t *testing.T) {
	s := NewOBIThreshold(0.7, 0.01, 0.1)

	event := model.FeatureEvent{
		EventTime:  time.Now(),
		Instrument: "BTC-USDT",
		Values:     map[string]float64{"obi": 0.90},
	}

	orders := s.OnFeature(event)
	if len(orders) != 1 {
		t.Fatalf("expected 1 order, got %d", len(orders))
	}
	if orders[0].Side != model.Sell {
		t.Errorf("expected SELL, got %s", orders[0].Side.String())
	}
}

func TestOBIThreshold_NoSignalInDeadZone(t *testing.T) {
	s := NewOBIThreshold(0.7, 0.01, 0.1)

	event := model.FeatureEvent{
		EventTime:  time.Now(),
		Instrument: "BTC-USDT",
		Values:     map[string]float64{"obi": 0.3},
	}

	orders := s.OnFeature(event)
	if len(orders) != 0 {
		t.Errorf("expected no orders in dead zone, got %d", len(orders))
	}
}

func TestOBIThreshold_MaxPositionThrottle(t *testing.T) {
	s := NewOBIThreshold(0.7, 0.05, 0.1)
	t0 := time.Now()

	// Buy twice (0.05 * 2 = 0.10 = max position), spacing by 2 min for cooldown
	for i := 0; i < 2; i++ {
		s.OnFeature(model.FeatureEvent{
			EventTime:  t0.Add(time.Duration(i) * 2 * time.Minute),
			Instrument: "BTC-USDT",
			Values:     map[string]float64{"obi": -0.85},
		})
	}

	// Third buy should be throttled (well after cooldown)
	orders := s.OnFeature(model.FeatureEvent{
		EventTime:  t0.Add(5 * time.Minute),
		Instrument: "BTC-USDT",
		Values:     map[string]float64{"obi": -0.90},
	})
	if len(orders) != 0 {
		t.Errorf("expected throttled (0 orders), got %d", len(orders))
	}
}

func TestVPINBreakout_BuyOnHighVPIN(t *testing.T) {
	s := NewVPINBreakout(0.5, 0.02, time.Minute)

	event := model.FeatureEvent{
		EventTime:  time.Now(),
		Instrument: "BTC-USDT",
		Values:     map[string]float64{"vpin": 0.65},
	}

	orders := s.OnFeature(event)
	if len(orders) != 1 {
		t.Fatalf("expected 1 order, got %d", len(orders))
	}
	if orders[0].Side != model.Buy {
		t.Errorf("expected BUY, got %s", orders[0].Side.String())
	}
}

func TestVPINBreakout_CooldownPreventsRepeat(t *testing.T) {
	s := NewVPINBreakout(0.5, 0.02, time.Minute)
	now := time.Now()

	// First signal triggers
	s.OnFeature(model.FeatureEvent{
		EventTime:  now,
		Instrument: "BTC-USDT",
		Values:     map[string]float64{"vpin": 0.65},
	})

	// Second signal 30s later should be suppressed by cooldown
	orders := s.OnFeature(model.FeatureEvent{
		EventTime:  now.Add(30 * time.Second),
		Instrument: "BTC-USDT",
		Values:     map[string]float64{"vpin": 0.70},
	})
	if len(orders) != 0 {
		t.Errorf("expected cooldown suppression, got %d orders", len(orders))
	}
}

func TestVWAPDeviation_Signals(t *testing.T) {
	s := NewVWAPDeviation(0.01, 0.05, 0.1) // 1% threshold, 0.05 order size, 0.1 max pos

	// Test BUY: Price = 98.0, VWAP = 100.0, dev = -0.02 (which is < -0.01 threshold)
	orders := s.OnFeature(model.FeatureEvent{
		EventTime:  time.Now(),
		Instrument: "BTC-USDT",
		Values:     map[string]float64{"microPrice": 98.0, "vwap": 100.0},
	})
	if len(orders) != 1 {
		t.Fatalf("expected 1 order, got %d", len(orders))
	}
	if orders[0].Side != model.Buy {
		t.Errorf("expected BUY, got %s", orders[0].Side.String())
	}

	// Test SELL: Price = 102.0, VWAP = 100.0, dev = 0.02 (which is > 0.01 threshold)
	orders = s.OnFeature(model.FeatureEvent{
		EventTime:  time.Now(),
		Instrument: "BTC-USDT",
		Values:     map[string]float64{"microPrice": 102.0, "vwap": 100.0},
	})
	if len(orders) != 1 {
		t.Fatalf("expected 1 order, got %d", len(orders))
	}
	if orders[0].Side != model.Sell {
		t.Errorf("expected SELL, got %s", orders[0].Side.String())
	}
}

func TestEMACrossover_WarmupAndCrossover(t *testing.T) {
	s := NewEMACrossover(2, 4, 0.01, 0.1) // Fast=2, Slow=4

	// Feed 4 events -> warmup count <= SlowPeriod (4), should be nil
	for i := 0; i < 4; i++ {
		orders := s.OnFeature(model.FeatureEvent{
			EventTime:  time.Now(),
			Instrument: "BTC-USDT",
			Values:     map[string]float64{"microPrice": 100.0},
		})
		if len(orders) != 0 {
			t.Fatalf("expected nil orders during warmup, got %d", len(orders))
		}
	}

	// 5th event -> Warmup complete (count = 5 > SlowPeriod). Still no crossover (prices equal).
	orders := s.OnFeature(model.FeatureEvent{
		EventTime:  time.Now(),
		Instrument: "BTC-USDT",
		Values:     map[string]float64{"microPrice": 100.0},
	})
	if len(orders) != 0 {
		t.Fatalf("expected no crossover order, got %d", len(orders))
	}

	// 6th event -> price jumps up (Fast EMA will rise faster than Slow EMA -> Bullish crossover)
	orders = s.OnFeature(model.FeatureEvent{
		EventTime:  time.Now(),
		Instrument: "BTC-USDT",
		Values:     map[string]float64{"microPrice": 110.0},
	})
	if len(orders) != 1 {
		t.Fatalf("expected 1 bullish crossover order, got %d", len(orders))
	}
	if orders[0].Side != model.Buy {
		t.Errorf("expected BUY, got %s", orders[0].Side.String())
	}

	// 7th event -> price drops down (Fast EMA drops faster than Slow EMA -> Bearish crossover)
	orders = s.OnFeature(model.FeatureEvent{
		EventTime:  time.Now(),
		Instrument: "BTC-USDT",
		Values:     map[string]float64{"microPrice": 90.0},
	})
	if len(orders) != 1 {
		t.Fatalf("expected 1 bearish crossover order, got %d", len(orders))
	}
	if orders[0].Side != model.Sell {
		t.Errorf("expected SELL, got %s", orders[0].Side.String())
	}
}

func TestEMACrossover_NoFalseSignalAtWarmupBoundary(t *testing.T) {
	s := NewEMACrossover(2, 4, 0.01, 0.1)

	// Feed exactly SlowPeriod events at constant price.
	// The warmup boundary must NOT produce a signal — previously an off-by-one
	// allowed the first post-warmup tick to fire when prevFastEMA == prevSlowEMA.
	for i := 0; i < 4; i++ {
		orders := s.OnFeature(model.FeatureEvent{
			EventTime:  time.Now(),
			Instrument: "BTC-USDT",
			Values:     map[string]float64{"microPrice": 100.0},
		})
		if len(orders) != 0 {
			t.Fatalf("event %d: expected nil during warmup, got %d orders", i+1, len(orders))
		}
	}

	// SlowPeriod+1 event, still constant price — no crossover should fire.
	orders := s.OnFeature(model.FeatureEvent{
		EventTime:  time.Now(),
		Instrument: "BTC-USDT",
		Values:     map[string]float64{"microPrice": 100.0},
	})
	if len(orders) != 0 {
		t.Fatalf("first post-warmup event at constant price should not signal, got %d orders", len(orders))
	}
}
