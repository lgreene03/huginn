package metrics

import (
	"math"
	"sort"

	"github.com/lgreene03/huginn/internal/model"
)

// CryptoPeriodsPerYear is the annualization cadence for this engine: 365.
//
// Equity is sampled once per calendar day from a 24/7 crypto venue — there are
// no weekends or exchange holidays, so a year contains 365 return periods, not
// the 252 trading days used for traditional equities. Using 252 here would
// understate annualized volatility (and inflate Sharpe) by sqrt(365/252).
//
// Convention decision (quant-1, resolved): 365 is the correct factor for a
// continuously-traded crypto market and is the convention huginn standardizes
// on across every Sharpe/Deflated-Sharpe computation. This is a deliberate,
// documented choice — any sibling system (e.g. odin) that wants comparable
// figures must annualize with 365 too; a 252-based number is NOT directly
// comparable and would differ by sqrt(365/252) ≈ 1.20x. We do not silently
// adopt a different cadence to match another system; huginn's annualization is
// defined here and reported as such.
const CryptoPeriodsPerYear = 365.0

// CalculateSharpeRatio computes the annualized Sharpe ratio from a series of
// per-day equity-curve points, using the crypto sampling cadence (365
// periods/year). For a non-default cadence, call CalculateSharpeRatioWithPeriods.
func CalculateSharpeRatio(equity []float64, riskFreeRate float64) float64 {
	return CalculateSharpeRatioWithPeriods(equity, riskFreeRate, CryptoPeriodsPerYear)
}

// CalculateSharpeRatioWithPeriods computes the annualized Sharpe ratio from a
// series of equity-curve points, annualizing by an explicit periodsPerYear that
// must match the sampling cadence of the equity series (e.g. 365 for once-daily
// samples of a 24/7 crypto market, 252 for once-per-trading-day equities).
//
// The annualization factor is applied as: annualized return = mean * periodsPerYear,
// annualized stddev = stddev * sqrt(periodsPerYear).
func CalculateSharpeRatioWithPeriods(equity []float64, riskFreeRate, periodsPerYear float64) float64 {
	if len(equity) < 2 {
		return 0.0
	}
	if periodsPerYear <= 0 {
		return 0.0
	}

	var returns []float64
	for i := 1; i < len(equity); i++ {
		prev := equity[i-1]
		// Guard the denominator: returns mix realized P&L jumps with
		// mark-to-market, so a prior equity of zero or negative would yield
		// Inf/NaN. Skip those steps rather than poison the whole series.
		if prev <= 0 {
			continue
		}
		ret := (equity[i] - prev) / prev
		returns = append(returns, ret)
	}

	if len(returns) == 0 {
		return 0.0
	}

	var sum float64
	for _, r := range returns {
		sum += r
	}
	mean := sum / float64(len(returns))

	var variance float64
	for _, r := range returns {
		variance += math.Pow(r-mean, 2)
	}
	stdDev := math.Sqrt(variance / float64(len(returns)))

	if stdDev == 0 {
		return 0.0
	}

	annualizedReturn := mean * periodsPerYear
	annualizedStdDev := stdDev * math.Sqrt(periodsPerYear)

	return (annualizedReturn - riskFreeRate) / annualizedStdDev
}

// CalculateMaxDrawdown computes the maximum percentage drop from a peak in the equity curve.
func CalculateMaxDrawdown(equity []float64) float64 {
	var maxDrawdown float64
	var peak float64

	for _, value := range equity {
		if value > peak {
			peak = value
		}

		if peak > 0 {
			drawdown := (peak - value) / peak
			if drawdown > maxDrawdown {
				maxDrawdown = drawdown
			}
		}
	}

	return maxDrawdown
}

// HitRate is the fraction of round-trip trades (buy-then-sell, or sell-then-buy
// when shorting is supported) whose realized PnL was positive. Returns 0 when
// no round trips have closed.
//
// The current paper-trading model is long-only at the strategy layer — every
// pair-matched (Buy, Sell) on the same instrument completes one round trip.
// Sells without a prior matching buy are skipped (we don't compute realized
// PnL on short legs here).
func HitRate(fills []model.Fill) float64 {
	wins, total := closedRoundTripStats(fills)
	if total == 0 {
		return 0
	}
	return float64(wins) / float64(total)
}

// Turnover is the gross notional traded divided by the maximum portfolio
// notional ever held at-cost. Higher turnover = more churn per unit of
// average capital deployed.
//
// Returns 0 when no fills happened or when the maximum at-cost notional
// never exceeded zero.
func Turnover(fills []model.Fill) float64 {
	if len(fills) == 0 {
		return 0
	}
	var grossNotional, maxAtCost float64
	pos := map[string]float64{}
	for _, f := range fills {
		grossNotional += f.Quantity * f.FillPrice
		switch f.Side {
		case model.Buy:
			pos[f.Instrument] += f.Quantity
		case model.Sell:
			pos[f.Instrument] -= f.Quantity
		}
		var atCost float64
		for _, q := range pos {
			atCost += math.Abs(q) * f.FillPrice
		}
		if atCost > maxAtCost {
			maxAtCost = atCost
		}
	}
	if maxAtCost == 0 {
		return 0
	}
	return grossNotional / maxAtCost
}

// AvgHoldTimeSeconds is the mean wall-clock duration of completed round
// trips (buy → matching sell on the same instrument). FIFO matching; the
// oldest open buy is closed by the next sell of the same instrument.
// Returns 0 when no round trips have closed.
func AvgHoldTimeSeconds(fills []model.Fill) float64 {
	holds := roundTripHolds(fills)
	if len(holds) == 0 {
		return 0
	}
	var sum float64
	for _, h := range holds {
		sum += h
	}
	return sum / float64(len(holds))
}

// closedRoundTripStats pair-matches buys with subsequent sells (FIFO per
// instrument) and returns (#wins, #closed_round_trips).
func closedRoundTripStats(fills []model.Fill) (wins, total int) {
	type openBuy struct {
		qty, price float64
	}
	open := map[string][]openBuy{}
	// Sort defensively — fills should already be in event-time order but
	// callers may pass slices from non-chronological sources (e.g. CSV).
	sortedFills := append([]model.Fill(nil), fills...)
	sort.SliceStable(sortedFills, func(i, j int) bool {
		return sortedFills[i].Timestamp.Before(sortedFills[j].Timestamp)
	})

	for _, f := range sortedFills {
		switch f.Side {
		case model.Buy:
			open[f.Instrument] = append(open[f.Instrument], openBuy{f.Quantity, f.FillPrice})
		case model.Sell:
			q := f.Quantity
			for q > 0 && len(open[f.Instrument]) > 0 {
				b := &open[f.Instrument][0]
				take := math.Min(b.qty, q)
				pnl := (f.FillPrice - b.price) * take
				if pnl > 0 {
					wins++
				}
				total++
				b.qty -= take
				q -= take
				if b.qty == 0 {
					open[f.Instrument] = open[f.Instrument][1:]
				}
			}
		}
	}
	return wins, total
}

// roundTripHolds returns the hold time in seconds for every closed round trip.
func roundTripHolds(fills []model.Fill) []float64 {
	type openBuy struct {
		qty   float64
		price float64
		t     int64 // unix seconds
	}
	open := map[string][]openBuy{}
	var holds []float64

	sortedFills := append([]model.Fill(nil), fills...)
	sort.SliceStable(sortedFills, func(i, j int) bool {
		return sortedFills[i].Timestamp.Before(sortedFills[j].Timestamp)
	})

	for _, f := range sortedFills {
		switch f.Side {
		case model.Buy:
			open[f.Instrument] = append(open[f.Instrument], openBuy{
				qty: f.Quantity, price: f.FillPrice, t: f.Timestamp.Unix(),
			})
		case model.Sell:
			q := f.Quantity
			closeT := f.Timestamp.Unix()
			for q > 0 && len(open[f.Instrument]) > 0 {
				b := &open[f.Instrument][0]
				take := math.Min(b.qty, q)
				holds = append(holds, float64(closeT-b.t))
				b.qty -= take
				q -= take
				if b.qty == 0 {
					open[f.Instrument] = open[f.Instrument][1:]
				}
			}
		}
	}
	return holds
}
