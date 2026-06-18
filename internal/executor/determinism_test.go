package executor

import (
	"testing"
	"time"

	"github.com/lgreene03/huginn/internal/config"
	"github.com/lgreene03/huginn/internal/model"
	"github.com/lgreene03/huginn/internal/portfolio"
	"github.com/lgreene03/huginn/internal/risk"
	"github.com/lgreene03/huginn/internal/strategy"
)

func TestReplayDeterminism_IdenticalFills(t *testing.T) {
	events := generateSyntheticEvents(200)

	run := func() portfolio.Snapshot {
		s := strategy.NewOBIThreshold(0.6, 0.01, 0.1)
		p := portfolio.New(100_000.0)
		rm := risk.NewManager(config.RiskConfig{
			MaxDrawdownPct: 0.20,
			DailyLossLimit: 5000, PositionLimitHard: 100_000,
		}, 100_000.0)
		exec := New(s, p, nil, rm, Config{
			TransactionCostBps: 10,
			SlippageBps:        5,
		}, false, nil, "")
		for _, e := range events {
			exec.OnFeature(e)
		}
		return p.Snapshot()
	}

	snap1 := run()
	snap2 := run()

	if snap1.TotalFills != snap2.TotalFills {
		t.Errorf("fills diverged: run1=%d run2=%d", snap1.TotalFills, snap2.TotalFills)
	}
	if snap1.RealizedPnL != snap2.RealizedPnL {
		t.Errorf("realized PnL diverged: run1=%.6f run2=%.6f", snap1.RealizedPnL, snap2.RealizedPnL)
	}
	if snap1.Cash != snap2.Cash {
		t.Errorf("cash diverged: run1=%.6f run2=%.6f", snap1.Cash, snap2.Cash)
	}
	if snap1.TotalValue != snap2.TotalValue {
		t.Errorf("total value diverged: run1=%.6f run2=%.6f", snap1.TotalValue, snap2.TotalValue)
	}
	if snap1.TotalFills == 0 {
		t.Error("no fills produced — test is vacuous")
	}
}

func TestReplayDeterminism_EMA(t *testing.T) {
	events := generateSyntheticEvents(200)

	run := func() portfolio.Snapshot {
		s := strategy.NewEMACrossover(5, 20, 0.01, 0.1)
		p := portfolio.New(100_000.0)
		rm := risk.NewManager(config.RiskConfig{
			MaxDrawdownPct: 0.20,
			DailyLossLimit: 5000, PositionLimitHard: 100_000,
		}, 100_000.0)
		exec := New(s, p, nil, rm, Config{
			TransactionCostBps: 10,
			SlippageBps:        5,
		}, false, nil, "")
		for _, e := range events {
			exec.OnFeature(e)
		}
		return p.Snapshot()
	}

	snap1 := run()
	snap2 := run()

	if snap1.TotalFills != snap2.TotalFills {
		t.Errorf("fills diverged: run1=%d run2=%d", snap1.TotalFills, snap2.TotalFills)
	}
	if snap1.RealizedPnL != snap2.RealizedPnL {
		t.Errorf("realized PnL diverged: run1=%.6f run2=%.6f", snap1.RealizedPnL, snap2.RealizedPnL)
	}
	if snap1.Cash != snap2.Cash {
		t.Errorf("cash diverged: run1=%.6f run2=%.6f", snap1.Cash, snap2.Cash)
	}
	if snap1.TotalFills == 0 {
		t.Error("no fills produced — test is vacuous")
	}
}

func generateSyntheticEvents(n int) []model.FeatureEvent {
	base := time.Date(2026, 5, 10, 14, 0, 0, 0, time.UTC)
	events := make([]model.FeatureEvent, n)
	for i := range events {
		obi := 0.3
		if i%7 == 0 {
			obi = 0.8
		} else if i%11 == 0 {
			obi = -0.75
		}
		price := 60000.0 + float64(i%30)*100.0 - float64(i%17)*50.0
		events[i] = model.FeatureEvent{
			EventID:     "test-evt-" + time.Duration(i).String(),
			FeatureName: "obi",
			Instrument:  "BTC-USDT",
			EventTime:   base.Add(time.Duration(i) * time.Minute),
			Values: map[string]float64{
				"obi":        obi,
				"microPrice": price,
			},
		}
	}
	return events
}
