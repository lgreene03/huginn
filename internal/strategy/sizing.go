package strategy

import "math"

// SizingMode selects how an order's quantity is determined.
//
// The default (SizingFixed) preserves huginn's historical behaviour: every
// order ships at the strategy's configured OrderSize, unchanged. The other
// modes are OPT-IN equity-aware overrides applied by the executor *after* the
// strategy emits its order, so no strategy needs to be rewritten to benefit.
//
// quant-4 context: KellyFraction (below, historically computed but unused) and
// an inverse-volatility rule are wired here as opt-in capabilities. They are
// off by default — SizingFixed leaves the strategy's OrderSize untouched.
type SizingMode int

const (
	// SizingFixed keeps the strategy's own OrderSize (default, no rescaling).
	SizingFixed SizingMode = iota
	// SizingKelly scales notional to a Kelly fraction of account equity.
	SizingKelly
	// SizingInverseVol scales notional inversely with market volatility,
	// targeting a roughly constant volatility budget per position.
	SizingInverseVol
)

// ParseSizingMode maps a config string to a SizingMode. Unknown / empty values
// map to SizingFixed (the safe default) with ok=false so callers can warn.
func ParseSizingMode(s string) (mode SizingMode, ok bool) {
	switch s {
	case "", "fixed":
		return SizingFixed, true
	case "kelly":
		return SizingKelly, true
	case "inverse_vol", "inverse-vol", "invvol":
		return SizingInverseVol, true
	default:
		return SizingFixed, false
	}
}

// SizingParams bundles the inputs an equity-aware sizing rule needs. The
// executor fills these from the live portfolio snapshot and feature event.
type SizingParams struct {
	// BaseQty is the strategy's original OrderSize for this order. It is the
	// return value for SizingFixed and the lower-bound fallback when a mode
	// can't compute a sane size (e.g. zero equity / price / volatility).
	BaseQty float64
	// Equity is the account's current total value (portfolio Snapshot.TotalValue).
	Equity float64
	// Price is the reference price used to convert a target notional to a
	// quantity (e.g. the order's limit price or the event mid/micro price).
	Price float64
	// Volatility is the market volatility feature (event.Values["volatility"]);
	// 0 means "unknown" and forces inverse-vol to fall back to BaseQty.
	Volatility float64

	// KellyFraction is the precomputed Kelly fraction of equity to allocate
	// (typically strategy.KellyFraction(winRate, avgWin, avgLoss)). Only used
	// by SizingKelly. 0 falls back to BaseQty.
	KellyFraction float64

	// VolTarget is the per-position volatility budget for SizingInverseVol —
	// target_notional = VolTarget/Volatility * Equity. A non-positive value
	// disables the mode (returns BaseQty).
	VolTarget float64

	// MaxNotionalFraction caps any computed notional at this fraction of equity
	// (e.g. 0.25 = never size a single order above 25% of equity). Non-positive
	// disables the cap.
	MaxNotionalFraction float64
}

// SizeOrder applies the selected sizing mode and returns the order quantity to
// use. It NEVER returns a negative quantity, and on any degenerate input
// (non-positive equity / price, or a mode-specific zero) it returns BaseQty so
// behaviour gracefully degrades to the strategy's own OrderSize.
func SizeOrder(mode SizingMode, p SizingParams) float64 {
	if mode == SizingFixed || p.BaseQty <= 0 || p.Equity <= 0 || p.Price <= 0 {
		return p.BaseQty
	}

	var targetNotional float64
	switch mode {
	case SizingKelly:
		if p.KellyFraction <= 0 {
			return p.BaseQty
		}
		targetNotional = p.KellyFraction * p.Equity
	case SizingInverseVol:
		if p.VolTarget <= 0 || p.Volatility <= 0 {
			return p.BaseQty
		}
		targetNotional = (p.VolTarget / p.Volatility) * p.Equity
	default:
		return p.BaseQty
	}

	if p.MaxNotionalFraction > 0 {
		maxNotional := p.MaxNotionalFraction * p.Equity
		if targetNotional > maxNotional {
			targetNotional = maxNotional
		}
	}

	qty := targetNotional / p.Price
	if qty <= 0 || math.IsNaN(qty) || math.IsInf(qty, 0) {
		return p.BaseQty
	}
	return qty
}
