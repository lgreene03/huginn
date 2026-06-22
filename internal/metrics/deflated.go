package metrics

import (
	"math"
	"sort"
)

// This file implements two overfitting-aware statistics from Bailey & López de
// Prado: the Deflated Sharpe Ratio (DSR) and the Probability of Backtest
// Overfitting (PBO). Both exist to answer the same uncomfortable question that
// the rest of this engine keeps raising — when a grid sweep picks the
// best-in-sample config out of N trials, how much of that winning Sharpe is real
// edge and how much is just the maximum of N noisy draws?
//
// References:
//   - Bailey, D. & López de Prado, M. (2014), "The Deflated Sharpe Ratio:
//     Correcting for Selection Bias, Backtest Overfitting and Non-Normality",
//     Journal of Portfolio Management.
//   - Bailey, D., Borwein, J., López de Prado, M. & Zhu, Q. (2017), "The
//     Probability of Backtest Overfitting", Journal of Computational Finance.
//
// Small-sample / degenerate-input policy: every function in this file returns
// NaN (never +Inf/-Inf) when its inputs are too thin or too degenerate to carry
// a defined meaning, and each NaN return is documented at its site. NaN here
// means "undefined / not enough information", which a caller can test with
// math.IsNaN and report as "n/a" rather than silently propagating an infinity
// into a CSV or a downstream comparison.

// euler is e, used for the expected-maximum-Sharpe approximation in the
// Bailey–López de Prado deflation benchmark.
const euler = math.E

// emConstant is the Euler–Mascheroni constant γ ≈ 0.5772156649, used in the
// expected value of the maximum of N independent standard-normal draws.
const emConstant = 0.5772156649015329

// normalCDF is the standard-normal cumulative distribution function Φ(x),
// expressed via the complementary error function. Exact at the tails to the
// precision of math.Erfc.
func normalCDF(x float64) float64 {
	return 0.5 * math.Erfc(-x/math.Sqrt2)
}

// normalPPF is the inverse standard-normal CDF (the quantile function Φ⁻¹(p)),
// implemented with Acklam's rational approximation and one Halley refinement
// step. Accurate to roughly 1e-9 over the open interval (0,1).
//
// For p outside (0,1) it returns ±Inf at the boundaries and NaN beyond them —
// but every caller in this file feeds it arguments strictly inside (0,1) by
// construction (e.g. 1-1/N for N≥2), so those branches are defensive.
func normalPPF(p float64) float64 {
	if math.IsNaN(p) {
		return math.NaN()
	}
	if p <= 0 {
		if p == 0 {
			return math.Inf(-1)
		}
		return math.NaN()
	}
	if p >= 1 {
		if p == 1 {
			return math.Inf(1)
		}
		return math.NaN()
	}

	// Acklam's algorithm coefficients.
	a := [...]float64{-3.969683028665376e+01, 2.209460984245205e+02, -2.759285104469687e+02, 1.383577518672690e+02, -3.066479806614716e+01, 2.506628277459239e+00}
	b := [...]float64{-5.447609879822406e+01, 1.615858368580409e+02, -1.556989798598866e+02, 6.680131188771972e+01, -1.328068155288572e+01}
	c := [...]float64{-7.784894002430293e-03, -3.223964580411365e-01, -2.400758277161838e+00, -2.549732539343734e+00, 4.374664141464968e+00, 2.938163982698783e+00}
	d := [...]float64{7.784695709041462e-03, 3.224671290700398e-01, 2.445134137142996e+00, 3.754408661907416e+00}

	const pLow = 0.02425
	const pHigh = 1 - pLow

	var x float64
	switch {
	case p < pLow:
		q := math.Sqrt(-2 * math.Log(p))
		x = (((((c[0]*q+c[1])*q+c[2])*q+c[3])*q+c[4])*q + c[5]) /
			((((d[0]*q+d[1])*q+d[2])*q+d[3])*q + 1)
	case p <= pHigh:
		q := p - 0.5
		r := q * q
		x = (((((a[0]*r+a[1])*r+a[2])*r+a[3])*r+a[4])*r + a[5]) * q /
			(((((b[0]*r+b[1])*r+b[2])*r+b[3])*r+b[4])*r + 1)
	default:
		q := math.Sqrt(-2 * math.Log(1-p))
		x = -(((((c[0]*q+c[1])*q+c[2])*q+c[3])*q+c[4])*q + c[5]) /
			((((d[0]*q+d[1])*q+d[2])*q+d[3])*q + 1)
	}

	// One Halley step to refine to ~machine precision.
	e := normalCDF(x) - p
	u := e * math.Sqrt(2*math.Pi) * math.Exp(x*x/2)
	x = x - u/(1+x*u/2)
	return x
}

// ReturnMoments holds the per-observation statistics the Deflated Sharpe Ratio
// consumes, derived from an equity curve. PerObsSharpe is the NON-annualized
// Sharpe (mean/stddev of per-period returns), which is exactly the unit
// DeflatedSharpeRatio expects — unlike CalculateSharpeRatio, which annualizes.
// NObs is the number of usable return observations (one fewer than the equity
// points, minus any skipped non-positive-prior steps).
type ReturnMoments struct {
	PerObsSharpe float64
	Skew         float64 // sample skewness γ₃ (0 for normal)
	Kurtosis     float64 // sample (non-excess) kurtosis γ₄ (3 for normal)
	NObs         float64
}

// EquityReturnMoments computes the per-observation Sharpe, skewness, and
// (non-excess) kurtosis of the simple returns implied by an equity curve, plus
// the usable observation count — everything DeflatedSharpeRatio needs.
//
// It mirrors CalculateSharpeRatio's return construction exactly (same
// prev<=0 skip) so the PerObsSharpe here is consistent with the engine's
// annualized Sharpe up to the √periodsPerYear factor. When fewer than 2 returns
// survive or the return series has zero variance, the moment fields that are
// undefined are returned as NaN (PerObsSharpe/Skew/Kurtosis), with NObs still
// reporting the true count, so a caller can detect "not enough data" via
// math.IsNaN(PerObsSharpe).
func EquityReturnMoments(equity []float64) ReturnMoments {
	var returns []float64
	for i := 1; i < len(equity); i++ {
		prev := equity[i-1]
		if prev <= 0 {
			continue
		}
		returns = append(returns, (equity[i]-prev)/prev)
	}
	n := float64(len(returns))
	if len(returns) < 2 {
		return ReturnMoments{PerObsSharpe: math.NaN(), Skew: math.NaN(), Kurtosis: math.NaN(), NObs: n}
	}

	var sum float64
	for _, r := range returns {
		sum += r
	}
	mean := sum / n

	var m2, m3, m4 float64
	for _, r := range returns {
		d := r - mean
		m2 += d * d
		m3 += d * d * d
		m4 += d * d * d * d
	}
	m2 /= n
	m3 /= n
	m4 /= n

	std := math.Sqrt(m2)
	if std == 0 {
		return ReturnMoments{PerObsSharpe: math.NaN(), Skew: math.NaN(), Kurtosis: math.NaN(), NObs: n}
	}

	return ReturnMoments{
		PerObsSharpe: mean / std,
		Skew:         m3 / (std * std * std),
		Kurtosis:     m4 / (m2 * m2),
		NObs:         n,
	}
}

// DeflatedSharpeRatio computes the Deflated Sharpe Ratio (DSR) of Bailey &
// López de Prado (2014).
//
// The DSR is the probability that the observed (per-observation, NON-annualized)
// Sharpe ratio exceeds a deflated benchmark SR_0 that accounts for (a) the
// number of independent strategy configurations tried (nTrials), (b) the
// non-normality of the return stream (skew and kurtosis), and (c) the sample
// length (nObs). It is, equivalently, the estimated probability that the
// strategy's TRUE Sharpe is greater than zero once selection bias has been
// stripped out.
//
//	DSR = Φ( ( (SR̂ − SR_0) · √(nObs − 1) ) /
//	         √( 1 − γ₃·SR̂ + (γ₄ − 1)/4 · SR̂² ) )
//
// where SR_0 is the expected maximum of nTrials independent Sharpe estimates
// under the null of zero true skill:
//
//	SR_0 = √Var(SR) · [ (1 − γ_E)·Φ⁻¹(1 − 1/N) + γ_E·Φ⁻¹(1 − 1/(N·e)) ]
//
// Because the cross-trial Var(SR) is not separately observed here, it is taken
// as 1/(nObs−1) — the asymptotic variance of an individual non-annualized
// Sharpe estimate under normality — which is the standard substitution when only
// a single backtest's stats are available. This makes SR_0 a function of
// nTrials and nObs alone.
//
// IMPORTANT — units: observedSharpe must be the PER-OBSERVATION Sharpe (mean
// return / stddev of returns over the sampling cadence), NOT the annualized
// figure produced by CalculateSharpeRatio. To convert an annualized Sharpe back,
// divide by √periodsPerYear (e.g. √365 for this engine's daily crypto cadence).
//
// Arguments:
//   - observedSharpe — the selected config's per-observation Sharpe (SR̂).
//   - nTrials        — number of configurations searched (N). The multiple-
//     testing count; pass the grid size.
//   - skew           — sample skewness γ₃ of the return series (0 for normal).
//   - kurtosis       — sample (non-excess) kurtosis γ₄ of the returns (3 for
//     normal). Pass the raw fourth-moment kurtosis, not excess.
//   - nObs           — number of return observations in the series (T).
//
// Returns NaN (documented "undefined") when:
//   - nObs < 2 (the √(nObs−1) scaling and the variance proxy are undefined);
//   - nTrials < 1 (no trials → no selection benchmark);
//   - the non-normality denominator 1 − γ₃·SR̂ + (γ₄−1)/4·SR̂² is ≤ 0 (the
//     variance estimate of the Sharpe is non-positive, so the test statistic is
//     undefined rather than infinite).
//
// With nTrials == 1 the deflation benchmark SR_0 collapses to the expected
// maximum of a single draw, i.e. Φ⁻¹(1 − 1/e)·√Var(SR) > 0; the DSR is then the
// ordinary Probabilistic Sharpe Ratio against that mild benchmark. There is no
// special-casing — the formula degrades smoothly.
func DeflatedSharpeRatio(observedSharpe float64, nTrials int, skew, kurtosis, nObs float64) float64 {
	if nObs < 2 || nTrials < 1 || math.IsNaN(observedSharpe) {
		return math.NaN()
	}

	// Variance proxy for a single non-annualized Sharpe estimate under
	// normality: Var(SR̂) ≈ 1/(T−1). Used both as the cross-trial dispersion in
	// SR_0 and (implicitly) as the scale of the test statistic.
	varSR := 1.0 / (nObs - 1.0)
	sqrtVarSR := math.Sqrt(varSR)

	sr0 := expectedMaxSharpe(nTrials, sqrtVarSR)

	// Non-normality-adjusted standard error of the Sharpe estimate
	// (Mertens / Lo): 1 − γ₃·SR + (γ₄−1)/4·SR². Guard non-positive: a degenerate
	// higher-moment combination can drive this ≤ 0, at which point the studentized
	// statistic is undefined (return NaN, never Inf).
	denomVar := 1.0 - skew*observedSharpe + (kurtosis-1.0)/4.0*observedSharpe*observedSharpe
	if denomVar <= 0 {
		return math.NaN()
	}

	z := (observedSharpe - sr0) * math.Sqrt(nObs-1.0) / math.Sqrt(denomVar)
	return normalCDF(z)
}

// expectedMaxSharpe returns the Bailey–López de Prado estimate of the expected
// maximum Sharpe across nTrials independent strategies drawn under the null of
// zero true skill, scaled by the cross-trial Sharpe dispersion sqrtVarSR:
//
//	SR_0 = sqrtVarSR · [ (1−γ_E)·Φ⁻¹(1 − 1/N) + γ_E·Φ⁻¹(1 − 1/(N·e)) ]
//
// For N == 1 the two quantile arguments are 1−1/1 = 0 (→ −Inf) — which would be
// nonsensical — so N == 1 is handled as the expected value of a single draw:
// Φ⁻¹(1 − 1/e)·sqrtVarSR. Assumes nTrials ≥ 1 (the caller guarantees this).
func expectedMaxSharpe(nTrials int, sqrtVarSR float64) float64 {
	n := float64(nTrials)
	if nTrials == 1 {
		// Expected max of one draw: the (1−1/e) quantile gives a small positive
		// benchmark rather than the undefined Φ⁻¹(0).
		return sqrtVarSR * normalPPF(1.0-1.0/euler)
	}
	q1 := normalPPF(1.0 - 1.0/n)
	q2 := normalPPF(1.0 - 1.0/(n*euler))
	return sqrtVarSR * ((1.0-emConstant)*q1 + emConstant*q2)
}

// ImpliedPTrueSharpeNonPositive converts a Deflated Sharpe Ratio into the
// implied probability that the strategy's TRUE Sharpe is ≤ 0. Since the DSR is
// (by construction) the probability that the true Sharpe exceeds the deflated
// zero-skill benchmark, the complementary probability 1 − DSR is the natural
// "could be a false discovery" figure to report alongside it.
//
// Returns NaN when dsr is NaN (undefined input propagates).
func ImpliedPTrueSharpeNonPositive(dsr float64) float64 {
	if math.IsNaN(dsr) {
		return math.NaN()
	}
	return 1.0 - dsr
}

// ProbabilityBacktestOverfitting computes the Probability of Backtest
// Overfitting (PBO) via the Combinatorially-Symmetric Cross-Validation (CSCV)
// procedure of Bailey, Borwein, López de Prado & Zhu (2017).
//
// Input — isOosFoldMatrix — is a per-fold performance matrix laid out as
// [fold][config]: each row is one out-of-sample fold (or, in the simplest
// walk-forward usage, one fold's vector of per-config scores), and each column
// is one strategy configuration. The value is that config's performance score in
// that fold (Sharpe, PnL, etc. — any "higher is better" metric).
//
// Method (the rank-logit estimator that this simplified form implements):
//
//	For each fold f, treat fold f as the OUT-OF-SAMPLE slice and the average of
//	all OTHER folds as the IN-SAMPLE slice. Pick the config n* that is best
//	in-sample, then find its rank among all configs out-of-sample. Map that OOS
//	rank to a relative rank ω ∈ (0,1) and a logit λ = ln(ω/(1−ω)). PBO is the
//	fraction of folds whose IS-best config landed in the BOTTOM HALF out of
//	sample, i.e. P(λ ≤ 0) — the probability the in-sample optimum is no better
//	than median out of sample, the operational definition of overfitting.
//
// This is a leave-one-fold-out / fold-level PBO proxy: it approximates the full
// combinatorially-symmetric CSCV (which evaluates every balanced split of folds
// into IS/OOS halves) but is NOT that estimator. It uses a single IS/OOS
// partition per fold rather than the symmetric set of all balanced splits, so it
// needs only the fold matrix the walk-forward harness already produces. It is a
// directionally consistent overfitting indicator, not a drop-in for full CSCV;
// adding folds does not make it converge to the combinatorial estimator.
//
// Returns NaN (documented "undefined") when:
//   - the matrix has fewer than 2 folds (no IS/OOS split is possible);
//   - the matrix has fewer than 2 configurations (ranking is degenerate — with
//     one config there is no selection to overfit);
//   - rows are ragged (differing config counts) — the matrix is malformed.
func ProbabilityBacktestOverfitting(isOosFoldMatrix [][]float64) float64 {
	nFolds := len(isOosFoldMatrix)
	if nFolds < 2 {
		return math.NaN()
	}
	nConfigs := len(isOosFoldMatrix[0])
	if nConfigs < 2 {
		return math.NaN()
	}
	for _, row := range isOosFoldMatrix {
		if len(row) != nConfigs {
			return math.NaN() // ragged matrix
		}
	}

	overfitCount := 0
	usableFolds := 0
	for f := 0; f < nFolds; f++ {
		// In-sample score for each config = mean over all folds except f.
		isScore := make([]float64, nConfigs)
		for c := 0; c < nConfigs; c++ {
			var sum float64
			for g := 0; g < nFolds; g++ {
				if g == f {
					continue
				}
				sum += isOosFoldMatrix[g][c]
			}
			isScore[c] = sum / float64(nFolds-1)
		}

		// Config that is best in-sample.
		bestIS := 0
		for c := 1; c < nConfigs; c++ {
			if isScore[c] > isScore[bestIS] {
				bestIS = c
			}
		}

		// Out-of-sample vector is fold f itself. Rank the IS-best config among
		// all configs OOS. Relative rank ω: 1/(N+1) (worst) .. N/(N+1) (best).
		oos := isOosFoldMatrix[f]
		rank := oosRank(oos, bestIS) // 1 = worst, nConfigs = best
		omega := float64(rank) / float64(nConfigs+1)
		lambda := math.Log(omega / (1.0 - omega))

		usableFolds++
		// Overfit = IS-best is at or below the OOS median (λ ≤ 0).
		if lambda <= 0 {
			overfitCount++
		}
	}

	if usableFolds == 0 {
		return math.NaN()
	}
	return float64(overfitCount) / float64(usableFolds)
}

// oosRank returns the 1-based rank of configuration target within the OOS score
// vector oos, where rank 1 is the WORST score and len(oos) is the BEST. Ties are
// broken so the target's own position is counted: rank = 1 + (#configs strictly
// worse than target) + (#tied configs that sort before target by index). The
// stable tie-break keeps the mapping deterministic.
func oosRank(oos []float64, target int) int {
	type sc struct {
		idx   int
		score float64
	}
	arr := make([]sc, len(oos))
	for i, v := range oos {
		arr[i] = sc{idx: i, score: v}
	}
	// Ascending by score, then by original index for stable ties.
	sort.SliceStable(arr, func(i, j int) bool {
		if arr[i].score != arr[j].score {
			return arr[i].score < arr[j].score
		}
		return arr[i].idx < arr[j].idx
	})
	for pos, s := range arr {
		if s.idx == target {
			return pos + 1 // 1-based: worst=1, best=len
		}
	}
	return 0 // unreachable: target is always present
}
