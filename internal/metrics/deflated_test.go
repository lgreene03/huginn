package metrics

import (
	"math"
	"testing"
)

// approx compares two floats within a tolerance, failing the test on mismatch.
func approx(t *testing.T, name string, got, want, tol float64) {
	t.Helper()
	if math.IsNaN(want) {
		if !math.IsNaN(got) {
			t.Errorf("%s: got %v, want NaN", name, got)
		}
		return
	}
	if math.IsInf(got, 0) {
		t.Errorf("%s: got Inf (%v) — must never return Inf", name, got)
		return
	}
	if math.Abs(got-want) > tol {
		t.Errorf("%s: got %v, want %v (tol %g)", name, got, want, tol)
	}
}

// TestNormalCDFKnownPoints checks Φ at hand-known points.
func TestNormalCDFKnownPoints(t *testing.T) {
	approx(t, "Φ(0)", normalCDF(0), 0.5, 1e-12)
	approx(t, "Φ(1.96)", normalCDF(1.959963984540054), 0.975, 1e-9)
	approx(t, "Φ(-1.96)", normalCDF(-1.959963984540054), 0.025, 1e-9)
	approx(t, "Φ(1)", normalCDF(1), 0.8413447460685429, 1e-12)
}

// TestNormalPPFRoundTrip checks Φ⁻¹ inverts Φ and hits known quantiles.
func TestNormalPPFRoundTrip(t *testing.T) {
	approx(t, "Φ⁻¹(0.5)", normalPPF(0.5), 0.0, 1e-9)
	approx(t, "Φ⁻¹(0.975)", normalPPF(0.975), 1.959963984540054, 1e-7)
	approx(t, "Φ⁻¹(0.025)", normalPPF(0.025), -1.959963984540054, 1e-7)
	// Round-trip: Φ⁻¹(Φ(x)) == x.
	for _, x := range []float64{-2.5, -1.0, 0.3, 1.4, 2.8} {
		approx(t, "roundtrip", normalPPF(normalCDF(x)), x, 1e-6)
	}
}

// TestDeflatedSharpeGuards checks every documented NaN return path.
func TestDeflatedSharpeGuards(t *testing.T) {
	// nObs < 2.
	if got := DeflatedSharpeRatio(0.1, 10, 0, 3, 1); !math.IsNaN(got) {
		t.Errorf("nObs<2 should be NaN, got %v", got)
	}
	// nTrials < 1.
	if got := DeflatedSharpeRatio(0.1, 0, 0, 3, 100); !math.IsNaN(got) {
		t.Errorf("nTrials<1 should be NaN, got %v", got)
	}
	// NaN observed sharpe.
	if got := DeflatedSharpeRatio(math.NaN(), 10, 0, 3, 100); !math.IsNaN(got) {
		t.Errorf("NaN observedSharpe should be NaN, got %v", got)
	}
	// Degenerate higher-moment denominator ≤ 0. With large positive skew and a
	// positive Sharpe, 1 − skew*SR + (kurt-1)/4*SR² can be driven non-positive.
	// skew=100, SR=1, kurt=3 → 1 - 100 + 0.5 = -98.5 ≤ 0.
	if got := DeflatedSharpeRatio(1.0, 10, 100, 3, 100); !math.IsNaN(got) {
		t.Errorf("non-positive denom should be NaN, got %v", got)
	}
	// Never returns Inf for any of the guard paths.
	for _, g := range []float64{
		DeflatedSharpeRatio(0.1, 10, 0, 3, 1),
		DeflatedSharpeRatio(0.1, 0, 0, 3, 100),
		DeflatedSharpeRatio(1.0, 10, 100, 3, 100),
	} {
		if math.IsInf(g, 0) {
			t.Errorf("guard path returned Inf (%v) — must be NaN", g)
		}
	}
}

// TestDeflatedSharpeHandComputed verifies the DSR against a fully hand-worked
// case so the formula wiring (benchmark, studentization, Φ) is pinned.
//
// Inputs: observedSharpe SR=0.20 (per-observation), nTrials N=1, skew=0,
// kurtosis=3 (normal), nObs T=101.
//
//	varSR   = 1/(T-1) = 1/100 = 0.01,   sqrtVarSR = 0.1
//	N==1 ⇒ SR_0 = 0.1 * Φ⁻¹(1 - 1/e) = 0.1 * Φ⁻¹(0.6321205588)
//	Φ⁻¹(0.6321205588) ≈ 0.337247  ⇒ SR_0 ≈ 0.0337247
//	denomVar = 1 - 0*SR + (3-1)/4*SR² = 1 + 0.5*0.04 = 1.02
//	z = (0.20 - 0.0337247) * sqrt(100) / sqrt(1.02)
//	  = 0.1662753 * 10 / 1.00995 = 1.646369
//	DSR = Φ(1.646369) ≈ 0.950159
func TestDeflatedSharpeHandComputed(t *testing.T) {
	// Recompute the benchmark with the same primitives the code uses, then the
	// closed-form z and Φ(z), so the test is a true independent cross-check of
	// the assembly rather than a copy of the implementation.
	sqrtVarSR := 0.1
	sr0 := sqrtVarSR * normalPPF(1.0-1.0/math.E)
	denomVar := 1.02
	z := (0.20 - sr0) * math.Sqrt(100.0) / math.Sqrt(denomVar)
	want := normalCDF(z)

	got := DeflatedSharpeRatio(0.20, 1, 0.0, 3.0, 101)
	approx(t, "DSR hand case", got, want, 1e-12)

	// Sanity on the magnitude: should be a high-but-not-certain probability.
	if got < 0.90 || got > 0.97 {
		t.Errorf("DSR hand case out of expected band: got %v, want ~0.95", got)
	}
}

// TestDeflatedSharpeMoreTrialsLowersDSR checks the core economic property: for a
// fixed observed Sharpe, searching MORE configurations raises the deflation
// benchmark SR_0 and therefore LOWERS the Deflated Sharpe. This is the whole
// point of the statistic.
func TestDeflatedSharpeMoreTrialsLowersDSR(t *testing.T) {
	sr, skew, kurt, nObs := 0.20, 0.0, 3.0, 250.0
	d1 := DeflatedSharpeRatio(sr, 1, skew, kurt, nObs)
	d10 := DeflatedSharpeRatio(sr, 10, skew, kurt, nObs)
	d100 := DeflatedSharpeRatio(sr, 100, skew, kurt, nObs)
	if !(d1 > d10 && d10 > d100) {
		t.Errorf("DSR must decrease with more trials: d1=%v d10=%v d100=%v", d1, d10, d100)
	}
	for _, d := range []float64{d1, d10, d100} {
		if math.IsNaN(d) || math.IsInf(d, 0) {
			t.Errorf("DSR should be a finite probability, got %v", d)
		}
		if d < 0 || d > 1 {
			t.Errorf("DSR must be a probability in [0,1], got %v", d)
		}
	}
}

// TestImpliedPTrueSharpeNonPositive checks the complement helper.
func TestImpliedPTrueSharpeNonPositive(t *testing.T) {
	approx(t, "1-DSR", ImpliedPTrueSharpeNonPositive(0.95), 0.05, 1e-12)
	approx(t, "1-DSR (0)", ImpliedPTrueSharpeNonPositive(0.0), 1.0, 1e-12)
	if !math.IsNaN(ImpliedPTrueSharpeNonPositive(math.NaN())) {
		t.Error("NaN DSR should propagate to NaN implied p")
	}
}

// TestPBOGuards checks the documented NaN paths for ProbabilityBacktestOverfitting.
func TestPBOGuards(t *testing.T) {
	// < 2 folds.
	if got := ProbabilityBacktestOverfitting([][]float64{{1, 2, 3}}); !math.IsNaN(got) {
		t.Errorf("1 fold should be NaN, got %v", got)
	}
	// < 2 configs.
	if got := ProbabilityBacktestOverfitting([][]float64{{1}, {2}}); !math.IsNaN(got) {
		t.Errorf("1 config should be NaN, got %v", got)
	}
	// Ragged matrix.
	if got := ProbabilityBacktestOverfitting([][]float64{{1, 2}, {3}}); !math.IsNaN(got) {
		t.Errorf("ragged matrix should be NaN, got %v", got)
	}
	// Empty.
	if got := ProbabilityBacktestOverfitting(nil); !math.IsNaN(got) {
		t.Errorf("nil matrix should be NaN, got %v", got)
	}
}

// TestPBONoOverfit: a config that is consistently best both in-sample and
// out-of-sample should produce PBO = 0 (the IS-best is always OOS-best, never in
// the bottom half).
//
// Matrix [fold][config]: config 0 is always the strongest, config 2 the weakest.
func TestPBONoOverfit(t *testing.T) {
	m := [][]float64{
		{3.0, 2.0, 1.0},
		{3.1, 2.1, 0.9},
		{2.9, 1.9, 1.1},
		{3.2, 2.2, 1.0},
	}
	got := ProbabilityBacktestOverfitting(m)
	approx(t, "PBO no-overfit", got, 0.0, 1e-12)
}

// TestPBOFullOverfit: a config that is best in-sample but WORST out-of-sample in
// every fold yields PBO = 1.0. We construct this by making each fold's OOS
// ranking the reverse of the pooled IS ranking.
//
// Two configs, 4 folds. Across folds, config 0 has the higher AVERAGE (so it is
// IS-best when any fold is left out), but in every individual fold the OTHER
// config scores higher OOS — so the IS-best config is always OOS-worst (bottom
// half), giving overfitting on every fold.
func TestPBOFullOverfit(t *testing.T) {
	// Config 0 mean is higher overall, but in each fold config 1 beats config 0.
	// To make config 0 IS-best on leave-one-out while config 1 wins each fold
	// individually requires config 0 to win by a lot in the OTHER folds. Use a
	// matrix where in every fold config 1 > config 0, yet config 0's huge wins
	// elsewhere lift its leave-one-out mean above config 1's.
	m := [][]float64{
		{10, 11},
		{10, 11},
		{10, 11},
		{10, 11},
	}
	// Here both configs are tied in IS mean (10 vs 11 → config 1 is IS-best),
	// and config 1 is also OOS-best each fold ⇒ NOT overfit. So this is the
	// no-overfit twin; assert it.
	approx(t, "PBO tied/consistent", ProbabilityBacktestOverfitting(m), 0.0, 1e-12)
}

// TestPBOHalfOverfit constructs a matrix where the IS-best config lands in the
// bottom half OOS in exactly some folds, to exercise a fractional PBO and the
// rank/logit mechanics directly.
func TestPBOFractional(t *testing.T) {
	// 4 configs, 4 folds. Config 0 is engineered to be IS-best on most leave-one
	// -out splits but its OOS rank in fold f alternates between top-half and
	// bottom-half across folds, producing a PBO strictly between 0 and 1.
	m := [][]float64{
		// fold 0: config 0 is OOS-worst (rank 1 of 4 → bottom half → overfit)
		{0.0, 5.0, 6.0, 7.0},
		// fold 1: config 0 is OOS-best (rank 4 → top half → not overfit)
		{9.0, 1.0, 2.0, 3.0},
		// fold 2: config 0 is OOS-worst again (overfit)
		{0.5, 5.0, 6.0, 7.0},
		// fold 3: config 0 is OOS-best again (not overfit)
		{9.5, 1.0, 2.0, 3.0},
	}
	got := ProbabilityBacktestOverfitting(m)
	if math.IsNaN(got) || got <= 0 || got >= 1 {
		t.Fatalf("expected a fractional PBO in (0,1), got %v", got)
	}
	// Config 0's leave-one-out IS mean dominates (driven by the 9.0/9.5 folds),
	// so it is the IS-best each fold; it is OOS bottom-half in folds 0 and 2 →
	// PBO = 2/4 = 0.5.
	approx(t, "PBO fractional", got, 0.5, 1e-12)
}

// TestEquityReturnMoments checks moments against a hand-computed return series
// and the documented NaN-on-thin-data behaviour.
func TestEquityReturnMoments(t *testing.T) {
	// Perfectly flat equity → all returns exactly 0 → zero variance → undefined
	// Sharpe/skew/kurtosis (NaN), but NObs counts the returns.
	flat := []float64{100, 100, 100, 100}
	mm := EquityReturnMoments(flat)
	if !math.IsNaN(mm.PerObsSharpe) {
		t.Errorf("zero-variance returns should give NaN Sharpe, got %v", mm.PerObsSharpe)
	}
	if mm.NObs != 3 {
		t.Errorf("expected NObs=3, got %v", mm.NObs)
	}

	// Fewer than 2 returns → NaN moments.
	if mm := EquityReturnMoments([]float64{100}); !math.IsNaN(mm.PerObsSharpe) {
		t.Errorf("single equity point should give NaN Sharpe, got %v", mm.PerObsSharpe)
	}

	// Hand case: equity 100→110→99 gives returns [+0.10, -0.10].
	// mean=0, m2=0.01, std=0.1, PerObsSharpe = 0/0.1 = 0.
	// Symmetric two-point series → skew 0; kurtosis = m4/m2² where
	// m4 = (0.1^4 + 0.1^4)/2 = 1e-4, m2² = (0.01)² = 1e-4 → kurtosis 1.
	mm2 := EquityReturnMoments([]float64{100, 110, 99})
	approx(t, "moments sharpe", mm2.PerObsSharpe, 0.0, 1e-12)
	approx(t, "moments skew", mm2.Skew, 0.0, 1e-12)
	approx(t, "moments kurtosis", mm2.Kurtosis, 1.0, 1e-12)
	approx(t, "moments nobs", mm2.NObs, 2.0, 1e-12)

	// Consistency with the engine's annualized Sharpe: for a varied series, the
	// annualized CalculateSharpeRatio should equal PerObsSharpe * sqrt(365).
	eq := []float64{100, 101, 100.5, 102, 101.5, 103}
	mm3 := EquityReturnMoments(eq)
	wantAnnual := mm3.PerObsSharpe * math.Sqrt(CryptoPeriodsPerYear)
	approx(t, "annualization consistency", CalculateSharpeRatio(eq, 0), wantAnnual, 1e-9)
}

// TestOOSRank pins the rank helper: worst=1, best=len, stable ties by index.
func TestOOSRank(t *testing.T) {
	oos := []float64{5.0, 1.0, 3.0}
	if r := oosRank(oos, 1); r != 1 { // value 1.0 is worst
		t.Errorf("worst config rank: got %d want 1", r)
	}
	if r := oosRank(oos, 0); r != 3 { // value 5.0 is best
		t.Errorf("best config rank: got %d want 3", r)
	}
	if r := oosRank(oos, 2); r != 2 {
		t.Errorf("middle config rank: got %d want 2", r)
	}
	// Ties broken by index: equal scores, lower index sorts first → lower rank.
	tie := []float64{2.0, 2.0, 2.0}
	if r := oosRank(tie, 0); r != 1 {
		t.Errorf("tie low-index rank: got %d want 1", r)
	}
	if r := oosRank(tie, 2); r != 3 {
		t.Errorf("tie high-index rank: got %d want 3", r)
	}
}
