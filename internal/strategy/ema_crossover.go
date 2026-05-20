package strategy

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"

	"github.com/lgreene03/huginn/internal/model"
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

// emaStateV1 is the persisted EMACrossover state shape, schema version 1.
// All five accumulator fields are essential — losing any of them produces
// a bogus crossover on the next event.
type emaStateV1 struct {
	NetPosition float64 `json:"net_position"`
	FastEMA     float64 `json:"fast_ema"`
	SlowEMA     float64 `json:"slow_ema"`
	PrevFastEMA float64 `json:"prev_fast_ema"`
	PrevSlowEMA float64 `json:"prev_slow_ema"`
	Count       int     `json:"count"`
}

// MarshalState implements Stateful.
func (s *EMACrossover) MarshalState() ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return MarshalEnvelope(1, emaStateV1{
		NetPosition: s.netPosition,
		FastEMA:     s.fastEMA,
		SlowEMA:     s.slowEMA,
		PrevFastEMA: s.prevFastEMA,
		PrevSlowEMA: s.prevSlowEMA,
		Count:       s.count,
	})
}

// RestoreState implements Stateful.
func (s *EMACrossover) RestoreState(data []byte) error {
	version, fields, err := ParseEnvelope(data)
	if err != nil {
		return err
	}
	if version != 1 {
		return fmt.Errorf("%w: EMACrossover got v%d", ErrStateVersionMismatch, version)
	}
	var f emaStateV1
	if err := json.Unmarshal(fields, &f); err != nil {
		return fmt.Errorf("EMACrossover: failed to unmarshal v1 fields: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.netPosition = f.NetPosition
	s.fastEMA = f.FastEMA
	s.slowEMA = f.SlowEMA
	s.prevFastEMA = f.PrevFastEMA
	s.prevSlowEMA = f.PrevSlowEMA
	s.count = f.Count
	return nil
}
