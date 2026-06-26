package strategy

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"sync"

	"github.com/lgreene03/huginn/internal/model"
)

// OUReversion is a stateful, statistically-grounded mean-reversion strategy.
//
// # Signal hypothesis
//
// The mid-price is modelled as an Ornstein-Uhlenbeck (OU) process — the
// continuous-time analogue of a discrete AR(1). On a rolling window of the
// most recent `Window` mid-prices we fit
//
//	x_{t+1} = a + phi * x_t + eps,   eps ~ N(0, sigma_eps^2)
//
// by ordinary least squares. From the AR(1) coefficients we recover the OU
// parameters:
//
//	mu        = a / (1 - phi)                 // long-run mean
//	theta     = -ln(phi)                      // mean-reversion speed (per step)
//	sigma_eq  = sigma_eps / sqrt(1 - phi^2)   // stationary (equilibrium) stdev
//	halfLife  = ln(2) / theta                 // steps to revert half-way
//
// The z-score of the latest price against the fitted stationary distribution,
// z = (price - mu) / sigma_eq, drives entries. Because the portfolio is
// signed, the strategy works symmetrically:
//
//   - z >  EntryBand  → price is rich → SELL (expect reversion down)
//   - z < -EntryBand  → price is cheap → BUY  (expect reversion up)
//
// # Exits
//
// Exits are derived from the *fitted* dynamics, not a hardcoded clock:
//
//   - Band exit: once |z| falls back inside `ExitBand` (a fraction of the
//     entry band) the position is closed — reversion has played out.
//   - Time exit: a position is force-closed after `holdSteps` feature events,
//     where holdSteps is a small multiple of the fitted half-life
//     (ceil(HoldHalfLives * halfLife)). A faster-reverting series (short
//     half-life) is given a correspondingly shorter leash; a slow one more
//     room. This is the central design choice — the holding horizon adapts to
//     the estimated speed of mean reversion.
//
// # Expected regime
//
// Best in range-bound / stationary micro-structure where phi in (0,1) and the
// half-life is short relative to the window. The OU fit is the strategy's own
// regime detector: a trending series produces phi >= 1 (a unit-or-explosive
// root), theta <= 0, an infinite/undefined half-life — which we treat as
// "no mean reversion" and refuse to trade. That is what stops the strategy
// from whipsawing in trends.
//
// # Known failure modes
//
//   - Regime shift inside the window. A structural break makes the OLS fit
//     average two regimes; mu drifts and z-scores mislead until the break
//     ages out of the rolling window.
//   - Estimation noise at small Window. Below ~30 points the phi estimate is
//     high-variance; half-life and sigma_eq are unstable. MinWindow guards the
//     warmup but does not make a tiny window reliable.
//   - Degenerate variance. A flat or near-constant window yields sigma_eq ≈ 0;
//     z-scores explode. Guarded by an absolute sigma floor.
//
// # Parameter sensitivity
//
//   - Window: the OLS sample. Too short → noisy theta/mu; too long → slow to
//     adapt to regime change. 30–120 is typical for high-frequency mids.
//   - EntryBand: z-score threshold. ~2.0 ≈ a 2-sigma event. Lower → more
//     trades, more cost drag; higher → rare, higher-conviction entries.
//   - HoldHalfLives: leash length in half-lives. ~2–4 lets reversion complete
//     without holding a stale thesis indefinitely.
//
// # State persisted across restarts
//
// The rolling price window, the current signed position, and the bars-held
// counter. Losing the window re-enters warmup (returns nil until refilled);
// losing the position/counter would orphan an open trade past its time exit.
type OUReversion struct {
	mu sync.Mutex

	// Config (immutable after construction).
	Window        int     // rolling OLS sample size
	EntryBand     float64 // |z| entry threshold
	exitBand      float64 // |z| exit threshold (fraction of EntryBand)
	holdHalfLives float64 // time-exit leash, in fitted half-lives
	OrderSize     float64
	maxPosition   float64

	// State.
	prices      []float64 // rolling window of recent mid-prices (len <= Window)
	netPosition float64   // signed exposure
	entrySign   int       // +1 if currently long-from-entry, -1 short, 0 flat
	barsHeld    int       // feature events since entry
	holdSteps   int       // time-exit horizon captured at entry (from half-life)
}

// Default exit/hold tuning. Exit when the z-score has reverted to a quarter of
// the entry band; hold at most 3 fitted half-lives before a time-based close.
const (
	defaultOUExitFraction  = 0.25
	defaultOUHoldHalfLives = 3.0
	// minOUWindow is the smallest window for which an AR(1) fit is attempted.
	minOUWindow = 20
	// sigmaFloor guards z-score blow-up on a near-constant window. Absolute
	// price units; mids are large so this is effectively "not exactly flat".
	ouSigmaFloor = 1e-9
	// maxHalfLifeFrac caps an actionable half-life at this fraction of the
	// window length. Above it the fit is a near-unit-root trend, not tradeable
	// reversion, and is treated as non-reverting.
	maxHalfLifeFrac = 1.0
)

// NewOUReversion creates a stateful OU / AR(1) mean-reversion strategy.
//
// window is the rolling OLS sample (clamped up to minOUWindow); entryBand is
// the |z| entry threshold; orderSize and maxPosition throttle exposure. Exit
// band and hold horizon use the package defaults (quarter-band exit, 3
// half-lives), keeping the constructor aligned with the other strategies'
// shape while the adaptive behaviour lives in OnFeature.
func NewOUReversion(window int, entryBand, orderSize, maxPosition float64) *OUReversion {
	if window < minOUWindow {
		window = minOUWindow
	}
	if entryBand <= 0 {
		entryBand = 2.0
	}
	return &OUReversion{
		Window:        window,
		EntryBand:     entryBand,
		exitBand:      entryBand * defaultOUExitFraction,
		holdHalfLives: defaultOUHoldHalfLives,
		OrderSize:     orderSize,
		maxPosition:   maxPosition,
		prices:        make([]float64, 0, window),
	}
}

func (s *OUReversion) Name() string {
	return fmt.Sprintf("OUReversion(w=%d,z=%.2f)", s.Window, s.EntryBand)
}

// ouFit holds the recovered OU parameters from an AR(1) OLS fit.
type ouFit struct {
	phi      float64
	mu       float64
	sigmaEq  float64
	theta    float64
	halfLife float64
	// meanReverting is true when 0 < phi < 1 and the fit yields a finite,
	// positive half-life and a usable equilibrium stdev.
	meanReverting bool
}

// fitOU estimates an AR(1)/OU process from a price window via OLS of
// x_{t+1} on x_t. Returns meanReverting=false for trending/degenerate fits.
//
// A near-unit-root trend (phi just under 1) is technically "reverting" but its
// half-life dwarfs the sample, so the reversion is never observed within the
// window and the z-score is dominated by drift. Such fits are rejected: a fit
// is only actionable when its half-life is no longer than the window itself
// (maxHalfLifeFrac * n). This is the statistical guard that keeps the strategy
// flat in trends rather than fading a runaway move.
func fitOU(prices []float64) ouFit {
	n := len(prices)
	if n < minOUWindow {
		return ouFit{}
	}
	// Regression of y=x_{t+1} on x=x_t over the n-1 overlapping pairs.
	m := n - 1
	var sx, sy, sxx, sxy float64
	for i := 0; i < m; i++ {
		x := prices[i]
		y := prices[i+1]
		sx += x
		sy += y
		sxx += x * x
		sxy += x * y
	}
	fm := float64(m)
	denom := fm*sxx - sx*sx
	if denom == 0 {
		return ouFit{} // degenerate (constant window)
	}
	phi := (fm*sxy - sx*sy) / denom
	a := (sy - phi*sx) / fm

	// Trending / explosive / non-reverting roots: no OU mean reversion.
	if phi <= 0 || phi >= 1 || math.IsNaN(phi) || math.IsInf(phi, 0) {
		return ouFit{}
	}

	mu := a / (1 - phi)

	// Residual variance of the AR(1) innovations.
	var sse float64
	for i := 0; i < m; i++ {
		resid := prices[i+1] - (a + phi*prices[i])
		sse += resid * resid
	}
	// Unbiased-ish: divide by (m-2) for the two fitted params; guard tiny m.
	dof := fm - 2
	if dof < 1 {
		dof = 1
	}
	sigmaEps := math.Sqrt(sse / dof)
	sigmaEq := sigmaEps / math.Sqrt(1-phi*phi)
	if sigmaEq < ouSigmaFloor || math.IsNaN(sigmaEq) || math.IsInf(sigmaEq, 0) {
		return ouFit{}
	}

	theta := -math.Log(phi)
	if theta <= 0 || math.IsNaN(theta) || math.IsInf(theta, 0) {
		return ouFit{}
	}
	halfLife := math.Ln2 / theta
	if halfLife <= 0 || math.IsNaN(halfLife) || math.IsInf(halfLife, 0) {
		return ouFit{}
	}
	// Reject near-unit-root trends: a half-life longer than the observation
	// window is not reversion the window can support.
	if halfLife > maxHalfLifeFrac*float64(n) {
		return ouFit{}
	}

	return ouFit{
		phi:           phi,
		mu:            mu,
		sigmaEq:       sigmaEq,
		theta:         theta,
		halfLife:      halfLife,
		meanReverting: true,
	}
}

func (s *OUReversion) OnFeature(event model.FeatureEvent) []model.Order {
	price, ok := event.Values["midPrice"]
	if !ok {
		price, ok = event.Values["microPrice"]
	}
	if !ok {
		price, ok = event.Values["value"]
	}
	if !ok || price <= 0 {
		return nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// Push into the rolling window (drop oldest beyond capacity).
	s.prices = append(s.prices, price)
	if len(s.prices) > s.Window {
		s.prices = s.prices[len(s.prices)-s.Window:]
	}

	// Warmup: not enough history to fit.
	if len(s.prices) < s.Window {
		return nil
	}

	fit := fitOU(s.prices)

	// If we hold a position, advance its clock and check exits first.
	if s.entrySign != 0 {
		s.barsHeld++
		var z float64
		zValid := false
		if fit.meanReverting {
			z = (price - fit.mu) / fit.sigmaEq
			zValid = true
		}

		bandExit := zValid && math.Abs(z) <= s.exitBand
		timeExit := s.holdSteps > 0 && s.barsHeld >= s.holdSteps

		if bandExit || timeExit {
			return s.closePosition(event, z, zValid, bandExit, timeExit)
		}
		// Still holding, no exit this bar.
		return nil
	}

	// Flat: only the OU fit can open a position.
	if !fit.meanReverting {
		return nil
	}

	z := (price - fit.mu) / fit.sigmaEq

	// Capture the time-exit horizon from the fitted half-life at entry.
	holdSteps := int(math.Ceil(s.holdHalfLives * fit.halfLife))
	if holdSteps < 1 {
		holdSteps = 1
	}

	var orders []model.Order

	// Rich → SELL (expect reversion down).
	if z > s.EntryBand && s.netPosition > -s.maxPosition {
		orders = append(orders, model.Order{
			Instrument: event.Instrument,
			Side:       model.Sell,
			Quantity:   s.OrderSize,
			Reason: fmt.Sprintf(
				"OU z=%.2f > +%.2f (mu=%.2f sigma_eq=%.4f half-life=%.1f), mean-reversion sell",
				z, s.EntryBand, fit.mu, fit.sigmaEq, fit.halfLife),
			Timestamp: event.EventTime,
		})
		s.netPosition -= s.OrderSize
		s.entrySign = -1
		s.barsHeld = 0
		s.holdSteps = holdSteps
		slog.Debug("Strategy signal",
			"strategy", s.Name(), "action", "SELL",
			"z", fmt.Sprintf("%.2f", z),
			"half_life", fmt.Sprintf("%.2f", fit.halfLife),
			"hold_steps", holdSteps,
			"instrument", event.Instrument,
			"net_position", fmt.Sprintf("%.8f", s.netPosition),
		)
		return orders
	}

	// Cheap → BUY (expect reversion up).
	if z < -s.EntryBand && s.netPosition < s.maxPosition {
		orders = append(orders, model.Order{
			Instrument: event.Instrument,
			Side:       model.Buy,
			Quantity:   s.OrderSize,
			Reason: fmt.Sprintf(
				"OU z=%.2f < -%.2f (mu=%.2f sigma_eq=%.4f half-life=%.1f), mean-reversion buy",
				z, s.EntryBand, fit.mu, fit.sigmaEq, fit.halfLife),
			Timestamp: event.EventTime,
		})
		s.netPosition += s.OrderSize
		s.entrySign = 1
		s.barsHeld = 0
		s.holdSteps = holdSteps
		slog.Debug("Strategy signal",
			"strategy", s.Name(), "action", "BUY",
			"z", fmt.Sprintf("%.2f", z),
			"half_life", fmt.Sprintf("%.2f", fit.halfLife),
			"hold_steps", holdSteps,
			"instrument", event.Instrument,
			"net_position", fmt.Sprintf("%.8f", s.netPosition),
		)
		return orders
	}

	return nil
}

// closePosition emits the offsetting order that flattens the current position
// and resets the entry bookkeeping. Caller holds s.mu.
func (s *OUReversion) closePosition(event model.FeatureEvent, z float64, zValid, bandExit, timeExit bool) []model.Order {
	// Offsetting side: a long-from-entry (+1) is closed by a SELL.
	var side model.Side
	if s.entrySign > 0 {
		side = model.Sell
	} else {
		side = model.Buy
	}

	reason := "OU exit: "
	switch {
	case bandExit && zValid:
		reason += fmt.Sprintf("z=%.2f reverted within +/-%.2f band", z, s.exitBand)
	case timeExit:
		reason += fmt.Sprintf("held %d>=%d steps (half-life horizon)", s.barsHeld, s.holdSteps)
	default:
		reason += "exit"
	}

	order := model.Order{
		Instrument: event.Instrument,
		Side:       side,
		Quantity:   s.OrderSize,
		Reason:     reason,
		Timestamp:  event.EventTime,
	}
	if side == model.Sell {
		s.netPosition -= s.OrderSize
	} else {
		s.netPosition += s.OrderSize
	}

	slog.Debug("Strategy signal",
		"strategy", s.Name(), "action", side.String(),
		"reason", reason,
		"instrument", event.Instrument,
		"net_position", fmt.Sprintf("%.8f", s.netPosition),
	)

	s.entrySign = 0
	s.barsHeld = 0
	s.holdSteps = 0
	return []model.Order{order}
}

// ouStateV1 is the persisted OUReversion state shape, schema version 1.
type ouStateV1 struct {
	Prices      []float64 `json:"prices"`
	NetPosition float64   `json:"net_position"`
	EntrySign   int       `json:"entry_sign"`
	BarsHeld    int       `json:"bars_held"`
	HoldSteps   int       `json:"hold_steps"`
}

// MarshalState implements Stateful.
func (s *OUReversion) MarshalState() ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	prices := make([]float64, len(s.prices))
	copy(prices, s.prices)
	return MarshalEnvelope(1, ouStateV1{
		Prices:      prices,
		NetPosition: s.netPosition,
		EntrySign:   s.entrySign,
		BarsHeld:    s.barsHeld,
		HoldSteps:   s.holdSteps,
	})
}

// RestoreState implements Stateful.
func (s *OUReversion) RestoreState(data []byte) error {
	version, fields, err := ParseEnvelope(data)
	if err != nil {
		return err
	}
	if version != 1 {
		return fmt.Errorf("%w: OUReversion got v%d", ErrStateVersionMismatch, version)
	}
	var f ouStateV1
	if err := json.Unmarshal(fields, &f); err != nil {
		return fmt.Errorf("OUReversion: failed to unmarshal v1 fields: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prices = f.Prices
	if cap(s.prices) < s.Window {
		grown := make([]float64, len(f.Prices), s.Window)
		copy(grown, f.Prices)
		s.prices = grown
	}
	s.netPosition = f.NetPosition
	s.entrySign = f.EntrySign
	s.barsHeld = f.BarsHeld
	s.holdSteps = f.HoldSteps
	return nil
}
