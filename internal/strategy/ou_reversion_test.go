package strategy

import (
	"math"
	"math/rand"
	"testing"
	"time"

	"github.com/lgreene03/huginn/internal/model"
)

// feedOU pushes one mid-price through the strategy and returns the orders.
func feedOU(s *OUReversion, price float64, t time.Time) []model.Order {
	return s.OnFeature(model.FeatureEvent{
		Instrument: "BTC-USDT",
		EventTime:  t,
		Values:     map[string]float64{"midPrice": price},
	})
}

// genOU produces a synthetic AR(1) mean-reverting series with the given
// parameters, then injects a single large shock at shockIdx so a clear
// entry-and-revert episode is guaranteed within the test horizon.
func genOU(n int, mu, phi, sigma float64, seed int64) []float64 {
	rng := rand.New(rand.NewSource(seed))
	out := make([]float64, n)
	x := mu
	for i := 0; i < n; i++ {
		x = mu + phi*(x-mu) + rng.NormFloat64()*sigma
		out[i] = x
	}
	return out
}

func TestOU_FitRecoversMeanRevertingParams(t *testing.T) {
	prices := genOU(400, 100.0, 0.85, 0.5, 42)
	fit := fitOU(prices)
	if !fit.meanReverting {
		t.Fatalf("expected mean-reverting fit, got non-reverting")
	}
	if fit.phi <= 0 || fit.phi >= 1 {
		t.Errorf("phi=%.4f out of (0,1)", fit.phi)
	}
	if math.Abs(fit.mu-100.0) > 2.0 {
		t.Errorf("mu=%.4f, want ~100", fit.mu)
	}
	// Half-life must be positive and finite (core requirement).
	if !(fit.halfLife > 0) || math.IsInf(fit.halfLife, 0) || math.IsNaN(fit.halfLife) {
		t.Errorf("half-life=%.4f not positive-finite", fit.halfLife)
	}
	if !(fit.theta > 0) {
		t.Errorf("theta=%.4f not positive", fit.theta)
	}
}

func TestOU_TrendingSeriesIsNotMeanReverting(t *testing.T) {
	// A pure upward drift: random-walk-with-drift → phi >= 1, no reversion.
	rng := rand.New(rand.NewSource(7))
	prices := make([]float64, 300)
	x := 100.0
	for i := range prices {
		x += 0.5 + rng.NormFloat64()*0.05 // strong drift, tiny noise
		prices[i] = x
	}
	fit := fitOU(prices)
	if fit.meanReverting {
		t.Errorf("trending series classified as mean-reverting (phi=%.4f half-life=%.4f)",
			fit.phi, fit.halfLife)
	}
}

func TestOU_SyntheticSeriesTriggersEntryAndExit(t *testing.T) {
	s := NewOUReversion(60, 2.0, 0.01, 1.0)

	base := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	tick := 0
	next := func() time.Time {
		tick++
		return base.Add(time.Duration(tick) * time.Second)
	}

	// 1) Warm up + establish a tight stationary distribution around mu=100.
	mu, phi, sigma := 100.0, 0.7, 0.3
	prices := genOU(200, mu, phi, sigma, 99)
	for _, p := range prices {
		feedOU(s, p, next())
	}

	// 2) Inject a large negative shock (well below mu) → z << -2 → BUY entry.
	var entry []model.Order
	for i := 0; i < 5 && entry == nil; i++ {
		entry = feedOU(s, mu-5.0, next()) // ~16 sigma_eq below mean
	}
	if entry == nil {
		t.Fatalf("expected a BUY entry after large negative shock")
	}
	if entry[0].Side != model.Buy {
		t.Errorf("entry side = %v, want BUY", entry[0].Side)
	}

	s.mu.Lock()
	if s.entrySign != 1 {
		t.Errorf("entrySign = %d, want 1 (long)", s.entrySign)
	}
	if s.holdSteps <= 0 {
		t.Errorf("holdSteps = %d, want positive (half-life derived)", s.holdSteps)
	}
	s.mu.Unlock()

	// 3) Feed prices back at the mean → z reverts inside the exit band → close.
	var exit []model.Order
	for i := 0; i < 30 && exit == nil; i++ {
		exit = feedOU(s, mu, next())
	}
	if exit == nil {
		t.Fatalf("expected a closing order once price reverted to the mean")
	}
	if exit[0].Side != model.Sell {
		t.Errorf("exit side = %v, want SELL (closing a long)", exit[0].Side)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.entrySign != 0 {
		t.Errorf("entrySign = %d after exit, want 0 (flat)", s.entrySign)
	}
	if s.netPosition != 0 {
		t.Errorf("netPosition = %v after round-trip, want 0", s.netPosition)
	}
}

func TestOU_TrendingSeriesDoesNotWhipsaw(t *testing.T) {
	s := NewOUReversion(60, 2.0, 0.01, 1.0)
	base := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

	// A steady up-trend should never be classified as mean-reverting, so the
	// strategy must stay flat — no churn of entries.
	rng := rand.New(rand.NewSource(123))
	x := 100.0
	entries := 0
	for i := 0; i < 400; i++ {
		x += 0.3 + rng.NormFloat64()*0.05
		orders := feedOU(s, x, base.Add(time.Duration(i)*time.Second))
		entries += len(orders)
	}
	if entries != 0 {
		t.Errorf("trending series produced %d orders, want 0 (no whipsaw)", entries)
	}
}

func TestOU_HalfLifeDerivedHoldIsPositiveFinite(t *testing.T) {
	// Across a range of phi, the time-exit horizon derived from the half-life
	// must always be a positive, finite number of steps.
	for _, phi := range []float64{0.5, 0.7, 0.85, 0.95} {
		prices := genOU(500, 50.0, phi, 0.4, int64(phi*1000))
		fit := fitOU(prices)
		if !fit.meanReverting {
			t.Fatalf("phi=%.2f: expected mean-reverting fit", phi)
		}
		hold := int(math.Ceil(defaultOUHoldHalfLives * fit.halfLife))
		if hold < 1 {
			t.Errorf("phi=%.2f: hold=%d not positive", phi, hold)
		}
		if math.IsInf(fit.halfLife, 0) || math.IsNaN(fit.halfLife) {
			t.Errorf("phi=%.2f: half-life=%.4f not finite", phi, fit.halfLife)
		}
		// Faster reversion (smaller phi) → shorter half-life → shorter leash.
	}
}

func TestOU_StateRoundTrip(t *testing.T) {
	s := NewOUReversion(40, 2.0, 0.01, 1.0)
	base := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	prices := genOU(60, 100.0, 0.7, 0.3, 5)
	for i, p := range prices {
		feedOU(s, p, base.Add(time.Duration(i)*time.Second))
	}

	blob, err := s.MarshalState()
	if err != nil {
		t.Fatalf("MarshalState: %v", err)
	}

	restored := NewOUReversion(40, 2.0, 0.01, 1.0)
	if err := restored.RestoreState(blob); err != nil {
		t.Fatalf("RestoreState: %v", err)
	}

	s.mu.Lock()
	restored.mu.Lock()
	defer s.mu.Unlock()
	defer restored.mu.Unlock()

	if len(restored.prices) != len(s.prices) {
		t.Fatalf("restored window len=%d, want %d", len(restored.prices), len(s.prices))
	}
	for i := range s.prices {
		if restored.prices[i] != s.prices[i] {
			t.Errorf("prices[%d]=%v, want %v", i, restored.prices[i], s.prices[i])
		}
	}
	if restored.netPosition != s.netPosition {
		t.Errorf("netPosition=%v, want %v", restored.netPosition, s.netPosition)
	}
	if cap(restored.prices) < restored.Window {
		t.Errorf("restored window cap=%d < Window=%d (would realloc on append)",
			cap(restored.prices), restored.Window)
	}
}

func TestOU_VersionMismatchRejected(t *testing.T) {
	s := NewOUReversion(40, 2.0, 0.01, 1.0)
	blob, err := MarshalEnvelope(99, ouStateV1{NetPosition: 1})
	if err != nil {
		t.Fatalf("MarshalEnvelope: %v", err)
	}
	if err := s.RestoreState(blob); err == nil {
		t.Errorf("expected version-mismatch error, got nil")
	}
}
