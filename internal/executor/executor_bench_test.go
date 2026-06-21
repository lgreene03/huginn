package executor

import (
	"testing"
	"time"

	"github.com/lgreene03/huginn/internal/model"
	"github.com/lgreene03/huginn/internal/portfolio"
	"github.com/lgreene03/huginn/internal/strategy"
)

// benchEvent is a feature event carrying an order-book quote so simulateFill
// exercises its book-aware pricing branch (fill at the ask touch + slippage).
func benchEvent() model.FeatureEvent {
	return model.FeatureEvent{
		EventTime:  time.Unix(1_700_000_000, 0),
		Instrument: "BTC-USDT",
		Values: map[string]float64{
			"midPrice":   60_000,
			"askPrice":   60_001,
			"bidPrice":   59_999,
			"microPrice": 60_000,
		},
	}
}

// BenchmarkSimulateFill measures the cost of synthesising a single paper fill:
// effective slippage, book-aware fill price, transaction cost, and the Fill
// struct construction. The size-dependent slippage impact term is enabled so
// the sqrt market-impact path is exercised (the heavier of the two branches).
func BenchmarkSimulateFill(b *testing.B) {
	p := portfolio.New(100_000)
	e := New(
		strategy.NewOBIThreshold(0.7, 0.01, 0.1),
		p, nil, nil,
		Config{
			TransactionCostBps:  10,
			SlippageBps:         5,
			SlippageImpactK:     2,
			SlippageImpactScale: 1,
		},
		false, nil, "",
	)
	order := model.Order{Instrument: "BTC-USDT", Side: model.Buy, Quantity: 0.5}
	event := benchEvent()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = e.simulateFill(order, event)
	}
}
