package research

import (
	"math"
	"testing"
	"time"

	"github.com/lgreene03/huginn/internal/config"
	"github.com/lgreene03/huginn/internal/model"
)

// sixMonthRiskConfig is the policy the six-month walk-forward of 2026-09-10 ran
// under. configs/default.yaml has no risk block, so config.Load applied the
// defaults in internal/config/config.go: a 10,000 daily loss limit, a 20%
// trailing drawdown and a 500,000 hard position limit, on 100,000 initial cash,
// with the default.yaml costs of 5 bps fees and 2 bps slippage.
func sixMonthRiskConfig() *config.Config {
	c := testConfig()
	c.Risk.MaxDrawdownPct = 0.20
	c.Risk.DailyLossLimit = 10_000
	c.Risk.PositionLimitHard = 500_000
	c.Executor.TransactionCostBps = 5
	c.Executor.SlippageBps = 2
	return c
}

// steadyLossEvents builds `days` consecutive UTC days of one-minute BTC bars on
// which OBI loses about the same amount each day. Each day opens with
// `roundTrips` pairs of bars: strong taker buying (obi +0.9, so OBI sells) at a
// low price, then strong taker selling (obi -0.9, so OBI buys back) at a high
// price. Every pair is a losing round trip that ends flat. Like the six-month
// dataset, the bars carry microPrice and no midPrice, so the strategy's own
// exits never fire and only the risk manager can stop it trading.
func steadyLossEvents(days, roundTrips int) []model.FeatureEvent {
	start := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	bar := func(at time.Time, obi, price float64) model.FeatureEvent {
		return model.FeatureEvent{
			EventTime:  at,
			Instrument: "BTC-USD",
			Values: map[string]float64{
				"microPrice": price, "obi": obi, "vpin": obi,
				"vwap": price, "volume": 10,
			},
		}
	}
	ev := make([]model.FeatureEvent, 0, days*roundTrips*2)
	for d := 0; d < days; d++ {
		day := start.AddDate(0, 0, d)
		for i := 0; i < roundTrips; i++ {
			at := day.Add(time.Duration(2*i) * time.Minute)
			ev = append(ev,
				bar(at, 0.9, 49_900),
				bar(at.Add(time.Minute), -0.9, 50_100),
			)
		}
	}
	return ev
}

// TestRunFoldDailyLossLimitRollsOnEventTime reproduces fold 4 of the six-month
// walk-forward, whose train and test PnL both stopped within about 1.7 of
// -10,000 on 100,000 of capital.
//
// The replay below loses about 2,700 on each of five days, 13,500 in all. No
// single day comes near the 10,000 daily loss limit, so the limit must not
// change the result: the replay has to match the same replay with a limit too
// large to bind, fill for fill. When the risk manager reads the wall clock, the
// whole replay happens inside one wall-clock day, the daily window never rolls,
// and the daily limit acts as a lifetime cap: every fill after the first 10,000
// of cumulative loss is rejected and the PnL freezes just past -10,000, which is
// fold 4's signature.
func TestRunFoldDailyLossLimitRollsOnEventTime(t *testing.T) {
	const days, roundTrips = 5, 10
	params := Params{Threshold: 0.5, OrderSize: 1}
	events := steadyLossEvents(days, roundTrips)
	limit := sixMonthRiskConfig().Risk.DailyLossLimit

	unbound := sixMonthRiskConfig()
	unbound.Risk.DailyLossLimit = 1e12
	want := runFold(unbound, events, params)

	// Guard the fixture itself: one day must lose well under the limit and the
	// whole replay well over it, or the test cannot tell a daily window from a
	// lifetime cap.
	oneDay := runFold(unbound, events[:2*roundTrips], params)
	if oneDay.pnl >= 0 || oneDay.pnl < -limit/2 {
		t.Fatalf("fixture: one day's PnL = %.4f, want a loss smaller than %.0f", oneDay.pnl, limit/2)
	}
	if want.pnl > -limit*1.2 {
		t.Fatalf("fixture: whole replay PnL = %.4f, want a loss beyond %.0f", want.pnl, limit*1.2)
	}
	if want.fills != days*roundTrips*2 {
		t.Fatalf("fixture: unbound replay made %d fills, want %d", want.fills, days*roundTrips*2)
	}

	got := runFold(sixMonthRiskConfig(), events, params)
	if got.fills != want.fills || math.Abs(got.pnl-want.pnl) > 1e-6 {
		t.Fatalf("daily loss limit %.0f changed a replay in which no day loses more than %.4f:\n"+
			"  with the limit:    PnL %.4f over %d fills\n"+
			"  limit unbinding:   PnL %.4f over %d fills\n"+
			"the daily window did not roll on event time, so the limit acted as a lifetime cap",
			limit, -oneDay.pnl, got.pnl, got.fills, want.pnl, want.fills)
	}
}
