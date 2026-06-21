package backtest

import (
	"math"
	"sort"
	"time"

	"github.com/lgreene03/huginn/internal/model"
)

// Regime labels for per-regime PnL attribution. The feature stream this engine
// consumes does not carry a string regime tag, so ClassifyRegime derives one
// from the microstructure features each event already exposes
// (regimeHurst/regimeAutocorr when present, otherwise vpin/volatility). The four
// buckets match the labels the batch calls for: quiet / trend / mean-revert /
// volatile.
const (
	RegimeQuiet      = "quiet"
	RegimeTrend      = "trend"
	RegimeMeanRevert = "mean-revert"
	RegimeVolatile   = "volatile"
	RegimeUnknown    = "unknown"
)

// regimeOrder fixes a stable, readable ordering for report output.
var regimeOrder = []string{RegimeTrend, RegimeMeanRevert, RegimeVolatile, RegimeQuiet, RegimeUnknown}

// ClassifyRegime maps a feature event to one of the four regime labels.
//
// Priority of signals (first decisive one wins):
//  1. explicit "regime" numeric code if a producer ever stamps one
//     (0=quiet,1=trend,2=mean-revert,3=volatile);
//  2. regimeHurst / regimeAutocorr — the same fields the OBI strategy adapts on:
//     Hurst > 0.6 ⇒ trend, autocorr < -0.2 ⇒ mean-revert;
//  3. vpin / volatility magnitude — high flow-toxicity or vol ⇒ volatile,
//     otherwise quiet.
//
// The thresholds intentionally mirror obi_threshold.go's regime logic so the
// attribution lines up with the signal the strategy itself reacted to.
func ClassifyRegime(ev model.FeatureEvent) string {
	v := ev.Values

	if code, ok := v["regime"]; ok {
		switch int(math.Round(code)) {
		case 0:
			return RegimeQuiet
		case 1:
			return RegimeTrend
		case 2:
			return RegimeMeanRevert
		case 3:
			return RegimeVolatile
		}
	}

	hurst, hasHurst := v["regimeHurst"]
	autocorr, hasAuto := v["regimeAutocorr"]
	if hasHurst && hurst > 0.6 {
		return RegimeTrend
	}
	if hasAuto && autocorr < -0.2 {
		return RegimeMeanRevert
	}

	// Fall back to flow-toxicity / volatility magnitude. vpin is in [0,1];
	// values near 1 mean heavily one-sided, toxic flow → volatile.
	if vpin, ok := v["vpin"]; ok && math.Abs(vpin) >= 0.7 {
		return RegimeVolatile
	}
	if vol, ok := v["volatility"]; ok && vol > 0 {
		// No absolute scale is guaranteed; treat a present, sizeable vol as
		// volatile only when no calmer signal classified the event.
		if vol >= 0.02 {
			return RegimeVolatile
		}
	}
	if hasHurst || hasAuto {
		// Had regime fields but neither trend nor mean-revert fired → quiet.
		return RegimeQuiet
	}
	if _, ok := v["vpin"]; ok {
		return RegimeQuiet
	}
	return RegimeUnknown
}

// RegimePnL holds the gross and net realized PnL attributed to one regime,
// along with how many round trips fell in it.
type RegimePnL struct {
	Regime     string
	GrossPnL   float64
	NetPnL     float64
	RoundTrips int
}

// RegimeAttribution buckets round-trip PnL (gross and net) by the regime active
// at each round trip's ENTRY (the buy leg), using the feature stream to look up
// the regime in force at the entry timestamp.
//
// Matching is FIFO buy→sell per instrument, identical to the metrics
// round-trip matcher. Gross PnL is the price-move result on fill prices; the
// round trip's fees and slippage (both legs) are subtracted to get net. The
// regime is taken from the most recent feature event at or before the buy
// leg's timestamp for that instrument, so the attribution reflects the
// microstructure the position was opened into.
func RegimeAttribution(fills []model.Fill, events []model.FeatureEvent) []RegimePnL {
	regimeAt := newRegimeLookup(events)

	type openBuy struct {
		qty, price float64
		fee, slip  float64 // per-unit cost components carried from the buy leg
		regime     string
	}
	open := map[string][]openBuy{}

	sorted := append([]model.Fill(nil), fills...)
	sort.SliceStable(sorted, func(i, j int) bool {
		return sorted[i].Timestamp.Before(sorted[j].Timestamp)
	})

	acc := map[string]*RegimePnL{}
	get := func(r string) *RegimePnL {
		if p, ok := acc[r]; ok {
			return p
		}
		p := &RegimePnL{Regime: r}
		acc[r] = p
		return p
	}

	for _, f := range sorted {
		switch f.Side {
		case model.Buy:
			perUnitFee, perUnitSlip := perUnitCosts(f)
			open[f.Instrument] = append(open[f.Instrument], openBuy{
				qty:    f.Quantity,
				price:  f.FillPrice,
				fee:    perUnitFee,
				slip:   perUnitSlip,
				regime: regimeAt(f.Instrument, f.Timestamp),
			})
		case model.Sell:
			sellFee, sellSlip := perUnitCosts(f)
			q := f.Quantity
			for q > 0 && len(open[f.Instrument]) > 0 {
				b := &open[f.Instrument][0]
				take := math.Min(b.qty, q)

				gross := (f.FillPrice - b.price) * take
				// Cost charged to this round trip = buy-leg costs on `take`
				// plus sell-leg costs on `take` (per-unit * take).
				cost := (b.fee+b.slip)*take + (sellFee+sellSlip)*take

				p := get(b.regime)
				p.GrossPnL += gross
				p.NetPnL += gross - cost
				p.RoundTrips++

				b.qty -= take
				q -= take
				if b.qty == 0 {
					open[f.Instrument] = open[f.Instrument][1:]
				}
			}
		}
	}

	out := make([]RegimePnL, 0, len(acc))
	for _, r := range regimeOrder {
		if p, ok := acc[r]; ok {
			out = append(out, *p)
		}
	}
	return out
}

// perUnitCosts returns the fee and slippage cost of a fill expressed per unit
// of quantity, so a partially-matched leg can be charged proportionally.
// Returns zeros for a zero-quantity fill.
func perUnitCosts(f model.Fill) (perUnitFee, perUnitSlip float64) {
	if f.Quantity == 0 {
		return 0, 0
	}
	notional := math.Abs(f.Quantity) * f.FillPrice
	slip := notional * (f.SlippageBps / 10_000.0)
	return f.TransactionCost / f.Quantity, slip / f.Quantity
}

// regimeLookup answers "what regime was instrument X in at time T?" against a
// pre-sorted per-instrument timeline of (time, regime) points.
type regimeLookup func(instrument string, t time.Time) string

func newRegimeLookup(events []model.FeatureEvent) regimeLookup {
	type pt struct {
		t      time.Time
		regime string
	}
	timeline := map[string][]pt{}
	for _, ev := range events {
		timeline[ev.Instrument] = append(timeline[ev.Instrument], pt{ev.EventTime, ClassifyRegime(ev)})
	}
	for inst := range timeline {
		pts := timeline[inst]
		sort.SliceStable(pts, func(i, j int) bool { return pts[i].t.Before(pts[j].t) })
		timeline[inst] = pts
	}

	return func(instrument string, t time.Time) string {
		pts := timeline[instrument]
		if len(pts) == 0 {
			return RegimeUnknown
		}
		// Most recent point at or before t (binary search).
		idx := sort.Search(len(pts), func(i int) bool { return pts[i].t.After(t) })
		if idx == 0 {
			// All points are after t; use the earliest as the best available.
			return pts[0].regime
		}
		return pts[idx-1].regime
	}
}
