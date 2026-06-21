package metrics

import (
	"math"
	"sort"

	"github.com/lgreene03/huginn/internal/model"
)

// CostBreakdown summarizes gross-versus-net realized performance over a set of
// fills. It is the heart of cost-aware reporting: a strategy with a real gross
// edge can still be net-negative once fees and slippage are charged, and that
// gap is the true objective this engine optimizes against.
//
// Definitions (all in account currency):
//
//   - GrossPnL  — realized round-trip PnL computed on FILL prices, before any
//     fee is deducted. Fill prices already embed slippage (the executor shifts
//     buys up and sells down by SlippageBps), so GrossPnL is the price-move
//     result an idealized fee-free book would have booked at those marks.
//   - Fees      — sum of Fill.TransactionCost across every fill (both legs).
//   - Slippage  — sum of each fill's slippage cost, notional*SlippageBps/1e4.
//     This is the implicit cost the slipped fill price already paid; surfacing
//     it lets the report attribute the gross→net gap between fees and slippage.
//   - NetPnL    — GrossPnL − Fees − Slippage. The honest bottom line.
//
// Only matched round trips (FIFO buy→sell per instrument) contribute to
// GrossPnL; fees and slippage are charged on every fill, including the legs of
// a still-open position, because those costs are paid the moment the fill
// happens regardless of whether the round trip has closed.
type CostBreakdown struct {
	GrossPnL    float64
	Fees        float64
	Slippage    float64
	NetPnL      float64
	RoundTrips  int
	Fills       int
}

// ComputeCostBreakdown walks the fill stream once for fee/slippage totals and
// FIFO-matches round trips for GrossPnL, then derives NetPnL.
//
// Slippage cost per fill is notional*SlippageBps/1e4 where notional =
// Quantity*FillPrice. This uses the SlippageBps actually stamped on each fill
// by the executor (which already includes any square-root market-impact term),
// so the figure is faithful to the sqrt-impact slippage model rather than a
// re-derivation.
func ComputeCostBreakdown(fills []model.Fill) CostBreakdown {
	var cb CostBreakdown
	cb.Fills = len(fills)

	for _, f := range fills {
		cb.Fees += f.TransactionCost
		notional := math.Abs(f.Quantity) * f.FillPrice
		cb.Slippage += notional * (f.SlippageBps / 10_000.0)
	}

	gross, rt := grossRoundTripPnL(fills)
	cb.GrossPnL = gross
	cb.RoundTrips = rt
	cb.NetPnL = cb.GrossPnL - cb.Fees - cb.Slippage
	return cb
}

// grossRoundTripPnL FIFO-matches buys with subsequent sells per instrument and
// returns the summed price-move PnL on fill prices plus the number of matched
// round trips. Mirrors the matching in closedRoundTripStats so HitRate and the
// gross figure agree on what a round trip is.
func grossRoundTripPnL(fills []model.Fill) (gross float64, roundTrips int) {
	type openBuy struct {
		qty, price float64
	}
	open := map[string][]openBuy{}

	sorted := append([]model.Fill(nil), fills...)
	sort.SliceStable(sorted, func(i, j int) bool {
		return sorted[i].Timestamp.Before(sorted[j].Timestamp)
	})

	for _, f := range sorted {
		switch f.Side {
		case model.Buy:
			open[f.Instrument] = append(open[f.Instrument], openBuy{f.Quantity, f.FillPrice})
		case model.Sell:
			q := f.Quantity
			for q > 0 && len(open[f.Instrument]) > 0 {
				b := &open[f.Instrument][0]
				take := math.Min(b.qty, q)
				gross += (f.FillPrice - b.price) * take
				roundTrips++
				b.qty -= take
				q -= take
				if b.qty == 0 {
					open[f.Instrument] = open[f.Instrument][1:]
				}
			}
		}
	}
	return gross, roundTrips
}

// NetReturnSeries converts an equity curve into a per-period net-return series
// and a parallel gross-return series, used to compute net Sharpe.
//
// The equity curve the engine samples is ALREADY net of fees and slippage: the
// portfolio's cost basis is fee-inclusive and fills are booked at slipped
// prices, so equity[i] reflects money actually in the book. That makes the
// curve's own period returns the NET return series directly.
//
// To recover a GROSS return series we add back the per-period cost drag. Costs
// are not sampled per equity period here, so callers that want a gross Sharpe
// pass the total cost and we amortize it uniformly across periods — a
// first-order reconstruction adequate for a side-by-side Sharpe comparison.
// When totalCost is 0 the two series are identical.
func NetReturnSeries(equity []float64) []float64 {
	if len(equity) < 2 {
		return nil
	}
	out := make([]float64, 0, len(equity)-1)
	for i := 1; i < len(equity); i++ {
		prev := equity[i-1]
		if prev <= 0 {
			continue
		}
		out = append(out, (equity[i]-prev)/prev)
	}
	return out
}

// NetSharpe is the annualized Sharpe of the net-return series derived from the
// (already net-of-cost) equity curve. It is a thin, explicit alias over the
// standard Sharpe so call sites that mean "net Sharpe" read as such.
func NetSharpe(equity []float64, riskFreeRate float64) float64 {
	return CalculateSharpeRatio(equity, riskFreeRate)
}
