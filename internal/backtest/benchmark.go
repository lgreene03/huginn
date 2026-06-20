package backtest

import (
	"math"

	"github.com/lgreene03/huginn/internal/model"
)

// Benchmark holds a buy-and-hold reference computed over the same event stream
// and window as a strategy backtest. It answers the only question that makes a
// strategy's return meaningful: did actively trading beat simply buying the
// instruments at the start and holding them?
//
// Construction model (BenchmarkBuyHold): the initial cash is split equally
// across every instrument that has an observable price, each notional slice
// buys at that instrument's first price, and the basket is then marked to
// market on the same daily cadence the strategy equity curve uses. Cash for
// instruments that never priced (so could not be bought) stays uninvested.
type Benchmark struct {
	// Equity is the daily marked-to-market value of the buy-and-hold basket,
	// sampled on the same cadence as the strategy equity curve.
	Equity []float64
	// TotalReturn is the fractional return of the basket over the window
	// (final/initial - 1).
	TotalReturn float64
	// Instruments is the number of instruments the basket actually bought
	// (those with at least one observable price).
	Instruments int
	// Invested is the cash actually deployed into the basket at t0; any
	// remainder (instruments that never priced) is held as idle cash.
	Invested float64
}

// priceOf extracts the same price the executor/strategies use to value an
// instrument from a feature event, in the same priority order as
// executor.estimatePrice plus the obi midPrice fallback: microPrice → value →
// midPrice. Returns (0,false) when none is present/positive.
func priceOf(ev model.FeatureEvent) (float64, bool) {
	if mp, ok := ev.Values["microPrice"]; ok && mp > 0 {
		return mp, true
	}
	if v, ok := ev.Values["value"]; ok && v > 0 {
		return v, true
	}
	if mid, ok := ev.Values["midPrice"]; ok && mid > 0 {
		return mid, true
	}
	return 0, false
}

// BenchmarkBuyHold computes the buy-and-hold benchmark for a feature-event
// stream. It walks the events once, records the first price per instrument as
// the entry, then marks the basket to market on the daily cadence used by the
// backtest engine (year*1000+YearDay buckets). The final point always reflects
// each instrument's last observed price.
//
// The cash is allocated equally across the instruments that are buyable (those
// with at least one observable price). Instruments that never price are not
// bought and their share of cash stays idle, so the benchmark never fabricates
// returns from data it could not have traded on.
func BenchmarkBuyHold(events []model.FeatureEvent, initialCash float64) Benchmark {
	// First pass: discover which instruments ever price and their first price.
	firstPrice := map[string]float64{}
	var order []string
	for _, ev := range events {
		px, ok := priceOf(ev)
		if !ok {
			continue
		}
		if _, seen := firstPrice[ev.Instrument]; !seen {
			firstPrice[ev.Instrument] = px
			order = append(order, ev.Instrument)
		}
	}

	if len(order) == 0 || initialCash <= 0 {
		// Nothing buyable: a flat curve at the starting cash.
		return Benchmark{
			Equity:      []float64{initialCash, initialCash},
			TotalReturn: 0,
			Instruments: 0,
			Invested:    0,
		}
	}

	// Equal-notional allocation; units = (cash slice) / entry price.
	slice := initialCash / float64(len(order))
	units := make(map[string]float64, len(order))
	var invested float64
	for _, inst := range order {
		units[inst] = slice / firstPrice[inst]
		invested += slice
	}
	idleCash := initialCash - invested

	// Second pass: mark to market on the daily cadence. lastPrice carries the
	// most recent observed price per instrument so the basket value at a bucket
	// boundary uses each instrument's latest mark (or its entry price until it
	// first prints).
	lastPrice := make(map[string]float64, len(order))
	for inst, p := range firstPrice {
		lastPrice[inst] = p
	}

	value := func() float64 {
		v := idleCash
		for _, inst := range order {
			v += units[inst] * lastPrice[inst]
		}
		return v
	}

	var equity []float64
	lastDay := -1
	for _, ev := range events {
		if px, ok := priceOf(ev); ok {
			if _, held := units[ev.Instrument]; held {
				lastPrice[ev.Instrument] = px
			}
		}
		day := ev.EventTime.Year()*1000 + ev.EventTime.YearDay()
		if day != lastDay {
			if lastDay != -1 {
				equity = append(equity, value())
			}
			lastDay = day
		}
	}
	equity = append(equity, value())

	if len(equity) < 2 {
		// Guarantee a 2-point curve so downstream Sharpe/return math is defined.
		equity = []float64{initialCash, value()}
	}

	// TotalReturn is measured from the full initial cash (entry cost basis),
	// not from equity[0]: the daily curve's first sample is taken at the first
	// day-boundary crossing, by which point the basket may already have moved,
	// so equity[0] is a mark, not the cost basis. Comparing final value to the
	// capital actually committed is the honest buy-and-hold return.
	last := equity[len(equity)-1]
	totalReturn := 0.0
	if initialCash > 0 {
		totalReturn = last/initialCash - 1
	}

	return Benchmark{
		Equity:      equity,
		TotalReturn: totalReturn,
		Instruments: len(order),
		Invested:    invested,
	}
}

// StrategyTotalReturn is the fractional return of the strategy measured from
// the full initial cash (finalValue/initialCash - 1) — the SAME cost basis as
// BenchmarkBuyHold. It must NOT be measured from equity[0]: the equity curve is
// sampled once per calendar day, so equity[0] is a mid-window mark, not the
// entry cost basis. Measuring from equity[0] makes the figure incomparable to
// the buy-and-hold benchmark and can flip a losing run positive. Returns 0 when
// initialCash is non-positive.
func StrategyTotalReturn(finalValue, initialCash float64) float64 {
	if initialCash <= 0 {
		return 0
	}
	return finalValue/initialCash - 1
}

// ExcessReturn is the strategy total return minus the benchmark total return —
// how much the active strategy added (or destroyed) versus buy-and-hold. Both
// sides are measured from the same initial-cash cost basis.
func ExcessReturn(finalValue, initialCash float64, b Benchmark) float64 {
	return StrategyTotalReturn(finalValue, initialCash) - b.TotalReturn
}

// InformationRatio is the mean per-period active return (strategy minus
// benchmark) divided by the standard deviation of that active-return series —
// the risk-adjusted consistency of the strategy's edge over buy-and-hold.
//
// The two curves are paired index-by-index over their overlapping prefix (both
// are sampled on the same daily cadence, so indices align). Returns 0 when
// there is insufficient overlap or the active-return series has zero
// volatility (avoiding a divide-by-zero blow-up to ±Inf).
func InformationRatio(strategyEquity []float64, b Benchmark) float64 {
	n := len(strategyEquity)
	if len(b.Equity) < n {
		n = len(b.Equity)
	}
	if n < 2 {
		return 0
	}

	var active []float64
	for i := 1; i < n; i++ {
		sp, bp := strategyEquity[i-1], b.Equity[i-1]
		if sp <= 0 || bp <= 0 {
			continue
		}
		sRet := (strategyEquity[i] - sp) / sp
		bRet := (b.Equity[i] - bp) / bp
		active = append(active, sRet-bRet)
	}
	if len(active) == 0 {
		return 0
	}

	var sum float64
	for _, a := range active {
		sum += a
	}
	mean := sum / float64(len(active))

	var variance float64
	for _, a := range active {
		variance += (a - mean) * (a - mean)
	}
	std := math.Sqrt(variance / float64(len(active)))
	if std == 0 {
		return 0
	}
	return mean / std
}
