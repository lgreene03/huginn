package strategy

import (
	"fmt"
	"log/slog"
	"sync"

	"github.com/lgreene/huginn/internal/model"
)

// EMACrossover is a stateful trend-following momentum strategy.
//
// It tracks Fast and Slow Exponential Moving Averages. When the Fast EMA crosses
// above the Slow EMA, it signals a bullish momentum trend and buys. When the Fast
// EMA crosses below the Slow EMA, it signals a bearish momentum trend and sells.
type EMACrossover struct {
	mu           sync.Mutex
	FastPeriod   int
	SlowPeriod   int
	OrderSize    float64
	maxPosition  float64
	netPosition  float64
	fastEMA      float64
	slowEMA      float64
	prevFastEMA  float64
	prevSlowEMA  float64
	count        int
}

// NewEMACrossover creates a new stateful EMA crossover strategy.
func NewEMACrossover(fastPeriod, slowPeriod int, orderSize, maxPosition float64) *EMACrossover {
	return &EMACrossover{
		FastPeriod:  fastPeriod,
		SlowPeriod:  slowPeriod,
		OrderSize:   orderSize,
		maxPosition: maxPosition,
	}
}

func (s *EMACrossover) Name() string {
	return fmt.Sprintf("EMACrossover(%d,%d)", s.FastPeriod, s.SlowPeriod)
}

func (s *EMACrossover) OnFeature(event model.FeatureEvent) []model.Order {
	price, ok := event.Values["microPrice"]
	if !ok {
		price, ok = event.Values["value"]
	}
	if !ok || price <= 0 {
		return nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	fastAlpha := 2.0 / (float64(s.FastPeriod) + 1.0)
	slowAlpha := 2.0 / (float64(s.SlowPeriod) + 1.0)

	s.prevFastEMA = s.fastEMA
	s.prevSlowEMA = s.slowEMA

	if s.count == 0 {
		s.fastEMA = price
		s.slowEMA = price
	} else {
		s.fastEMA = price*fastAlpha + s.fastEMA*(1.0-fastAlpha)
		s.slowEMA = price*slowAlpha + s.slowEMA*(1.0-slowAlpha)
	}
	s.count++

	// Warmup check: wait until slow EMA stabilizes
	if s.count < s.SlowPeriod {
		return nil
	}

	var orders []model.Order

	// Bullish crossover (Fast crosses above Slow) -> BUY
	if s.prevFastEMA <= s.prevSlowEMA && s.fastEMA > s.slowEMA {
		if s.netPosition < s.maxPosition {
			orders = append(orders, model.Order{
				Instrument: event.Instrument,
				Side:       model.Buy,
				Quantity:   s.OrderSize,
				Reason:     fmt.Sprintf("FastEMA(%d)=%.2f crossed above SlowEMA(%d)=%.2f, bullish trend", s.FastPeriod, s.fastEMA, s.SlowPeriod, s.slowEMA),
				Timestamp:  event.EventTime,
			})
			s.netPosition += s.OrderSize

			slog.Info("Strategy signal",
				"strategy", s.Name(),
				"action", "BUY",
				"fast_ema", fmt.Sprintf("%.2f", s.fastEMA),
				"slow_ema", fmt.Sprintf("%.2f", s.slowEMA),
				"instrument", event.Instrument,
				"net_position", fmt.Sprintf("%.8f", s.netPosition),
			)
		}
	}

	// Bearish crossover (Fast crosses below Slow) -> SELL
	if s.prevFastEMA >= s.prevSlowEMA && s.fastEMA < s.slowEMA {
		if s.netPosition > -s.maxPosition {
			orders = append(orders, model.Order{
				Instrument: event.Instrument,
				Side:       model.Sell,
				Quantity:   s.OrderSize,
				Reason:     fmt.Sprintf("FastEMA(%d)=%.2f crossed below SlowEMA(%d)=%.2f, bearish trend", s.FastPeriod, s.fastEMA, s.SlowPeriod, s.slowEMA),
				Timestamp:  event.EventTime,
			})
			s.netPosition -= s.OrderSize

			slog.Info("Strategy signal",
				"strategy", s.Name(),
				"action", "SELL",
				"fast_ema", fmt.Sprintf("%.2f", s.fastEMA),
				"slow_ema", fmt.Sprintf("%.2f", s.slowEMA),
				"instrument", event.Instrument,
				"net_position", fmt.Sprintf("%.8f", s.netPosition),
			)
		}
	}

	return orders
}
