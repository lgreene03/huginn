package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/lgreene03/huginn/internal/executor"
	"github.com/lgreene03/huginn/internal/metrics"
	"github.com/lgreene03/huginn/internal/model"
	"github.com/lgreene03/huginn/internal/portfolio"
	"github.com/lgreene03/huginn/internal/risk"
	"github.com/lgreene03/huginn/internal/strategy"
	"github.com/lgreene03/huginn/internal/version"
)

// defaultWalkforwardResultsPath is the walk-forward artifact the /api/validation
// endpoint reads when WALKFORWARD_RESULTS_PATH is unset.
const defaultWalkforwardResultsPath = "data/walkforward_results.json"

// equityPoint is one sample in the server-side equity history ring.
type equityPoint struct {
	Timestamp  time.Time `json:"timestamp"`
	TotalValue float64   `json:"total_value"`
	Cash       float64   `json:"cash"`
}

// equityRing is a bounded in-memory ring buffer of recent equity samples,
// capacity fixed at construction time (default 720 ≈ 6 h at 30 s sampling).
type equityRing struct {
	mu     sync.RWMutex
	points []equityPoint
	cap    int
}

func newEquityRing(capacity int) *equityRing {
	return &equityRing{cap: capacity, points: make([]equityPoint, 0, capacity)}
}

func (r *equityRing) push(p equityPoint) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.points) >= r.cap {
		r.points = append(r.points[1:], p)
	} else {
		r.points = append(r.points, p)
	}
}

func (r *equityRing) snapshot() []equityPoint {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]equityPoint, len(r.points))
	copy(out, r.points)
	return out
}

type Server struct {
	addr       string
	portfolio  *portfolio.Portfolio
	riskMgr    *risk.Manager
	executor   *executor.Executor
	isReady    bool
	readyMutex sync.RWMutex
	srv        *http.Server
	// apiToken gates mutating endpoints. Read from HUGINN_API_TOKEN env var.
	// When empty, mutating control endpoints FAIL CLOSED (503) rather than
	// passing through — an unconfigured token must never leave the breaker
	// and mock-fill controls open. Read-only endpoints are unaffected.
	apiToken string
	// corsOrigin is the single allowed dashboard origin echoed in
	// Access-Control-Allow-Origin. Read from HUGINN_DASHBOARD_ORIGIN;
	// defaults to http://localhost:8084. Never "*" on token-gated routes.
	corsOrigin string
	equity     *equityRing
	// readinessProbe, when set, is an extra gate on /readyz: it returns a
	// non-nil error when the consumer loop has not advanced within the
	// staleness window, making /readyz return 503 even though liveness
	// (/healthz) stays green. Nil (the default) means /readyz only reflects
	// SetReady — fully backward-compatible.
	readinessProbe func() error
}

// ReadinessProbe registers a deep-readiness check consulted by /readyz in
// addition to the SetReady flag. Pass nil to disable (default). Typically
// wired to a kafka.Progress staleness check so a wedged consumer loop trips
// readiness without affecting liveness.
func (s *Server) ReadinessProbe(probe func() error) {
	s.readyMutex.Lock()
	defer s.readyMutex.Unlock()
	s.readinessProbe = probe
}

func New(addr string, portf *portfolio.Portfolio, riskMgr *risk.Manager, exec *executor.Executor) *Server {
	origin := os.Getenv("HUGINN_DASHBOARD_ORIGIN")
	if origin == "" {
		origin = "http://localhost:8084"
	}
	return &Server{
		addr:       addr,
		portfolio:  portf,
		riskMgr:    riskMgr,
		executor:   exec,
		apiToken:   os.Getenv("HUGINN_API_TOKEN"),
		corsOrigin: origin,
		equity:     newEquityRing(720),
	}
}

func (s *Server) SetReady(ready bool) {
	s.readyMutex.Lock()
	defer s.readyMutex.Unlock()
	s.isReady = ready
}

// RunEquitySampler pushes one equity point per interval into the ring buffer.
// Call this in a goroutine; it stops when ctx is cancelled.
func (s *Server) RunEquitySampler(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case t := <-ticker.C:
			snap := s.portfolio.Snapshot()
			s.equity.push(equityPoint{
				Timestamp:  t,
				TotalValue: snap.TotalValue,
				Cash:       snap.Cash,
			})
		}
	}
}

// corsMiddleware adds CORS headers scoped to the configured dashboard origin.
// Access-Control-Allow-Origin is set to s.corsOrigin (default
// http://localhost:8084), never "*", so token-gated endpoints are not exposed
// to arbitrary cross-origin callers.
func (s *Server) corsMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", s.corsOrigin)
		w.Header().Set("Vary", "Origin")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}

		next(w, r)
	}
}

// authMiddleware enforces bearer-token auth on mutating control endpoints.
//
// It FAILS CLOSED: when HUGINN_API_TOKEN is unset the mutation is refused with
// 503 rather than passing through, so an unconfigured deployment can never
// expose the breaker/mock-fill controls open. When the token is set, a missing
// or incorrect bearer token returns 401. Read-only endpoints do not use this
// middleware and stay open.
func (s *Server) authMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Preflight requests carry no credentials; let CORS handle them.
		if r.Method == http.MethodOptions {
			next(w, r)
			return
		}
		if s.apiToken == "" {
			http.Error(w, "Control plane locked: HUGINN_API_TOKEN not configured", http.StatusServiceUnavailable)
			return
		}
		if !s.bearerTokenValid(r) {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

// bearerTokenValid reports whether the request carries the configured bearer
// token. The comparison is constant-time (crypto/subtle) so an attacker can't
// recover the token byte-by-byte from response-time differences;
// ConstantTimeCompare also runs in time independent of content and returns 0 on
// a length mismatch. Callers must have already checked that s.apiToken is set
// (the control plane fails closed when it is empty).
func (s *Server) bearerTokenValid(r *http.Request) bool {
	expected := "Bearer " + s.apiToken
	provided := r.Header.Get("Authorization")
	return subtle.ConstantTimeCompare([]byte(provided), []byte(expected)) == 1
}

func (s *Server) healthzHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	snap := s.portfolio.Snapshot()
	if err := json.NewEncoder(w).Encode(snap); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
}

// snapshotHandler returns the same per-tick payload the SSE /api/stream
// emits, but as a plain JSON response. Easier to scrape from cron-style
// monitors and load-testers than parsing an SSE stream. Phase 3 deliverable.
func (s *Server) snapshotHandler(w http.ResponseWriter, r *http.Request) {
	snap := s.portfolio.Snapshot()
	payload := map[string]interface{}{
		"portfolio":   snap,
		"halted":      s.riskMgr.IsHalted(),
		"halt_reason": string(s.riskMgr.HaltReason()),
		"fills":       s.portfolio.Fills(),
		"timestamp":   time.Now().Format(time.RFC3339),
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
}

func (s *Server) readyzHandler(w http.ResponseWriter, r *http.Request) {
	s.readyMutex.RLock()
	ready := s.isReady
	probe := s.readinessProbe
	s.readyMutex.RUnlock()

	if !ready {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("Not Ready"))
		return
	}

	// Deep readiness: a wedged/stale consumer loop trips 503 even though the
	// process is live. /healthz stays liveness-only and is unaffected.
	if probe != nil {
		if err := probe(); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("Not Ready: " + err.Error()))
			return
		}
	}

	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("OK"))
}

// streamHandler implements Server-Sent Events (SSE) to push live engine state to the web UI.
func (s *Server) streamHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", s.corsOrigin)
	w.Header().Set("Vary", "Origin")

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming unsupported", http.StatusInternalServerError)
		return
	}

	slog.Info("Client connected to SSE real-time state stream", "remote_addr", r.RemoteAddr)
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			slog.Info("Client disconnected from SSE stream", "remote_addr", r.RemoteAddr)
			return
		case <-ticker.C:
			snap := s.portfolio.Snapshot()

			// Construct extended UI payload including current breaker state
			payload := map[string]interface{}{
				"portfolio": snap,
				"halted":    s.riskMgr.IsHalted(),
				"fills":     s.portfolio.Fills(),
				"timestamp": time.Now().Format(time.RFC3339),
			}

			data, err := json.Marshal(payload)
			if err != nil {
				slog.Error("Failed to marshal SSE payload", "error", err)
				continue
			}

			_, _ = fmt.Fprintf(w, "data: %s\n\n", string(data))
			flusher.Flush()
		}
	}
}

// breakerTriggerHandler manually halts trading.
func (s *Server) breakerTriggerHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.riskMgr.Halt()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"halted","message":"Strategy execution manually halted."}`))
}

// breakerResetHandler manually resumes trading.
func (s *Server) breakerResetHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.riskMgr.Resume()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"running","message":"Strategy execution manually resumed."}`))
}

// mockFillHandler manually submits a mock fill.
func (s *Server) mockFillHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Instrument string  `json:"instrument"`
		Side       string  `json:"side"` // "BUY" or "SELL"
		Quantity   float64 `json:"quantity"`
		Price      float64 `json:"price"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if req.Instrument == "" || req.Quantity <= 0 || req.Price <= 0 {
		http.Error(w, "Invalid parameters", http.StatusBadRequest)
		return
	}

	side := model.Buy
	if req.Side == "SELL" {
		side = model.Sell
	}

	ts := time.Now()
	fill := model.Fill{
		OrderID:         fmt.Sprintf("mock-fill-%d", ts.UnixNano()),
		ExecutionID:     fmt.Sprintf("mock-exec-%d", ts.UnixNano()),
		Instrument:      req.Instrument,
		Side:            side,
		Quantity:        req.Quantity,
		FillPrice:       req.Price,
		TransactionCost: req.Price * req.Quantity * 0.0005, // 5 bps cost
		SlippageBps:     5,
		Timestamp:       ts,
	}

	// Run risk evaluation before applying. A real Sleipnir fill's originating
	// intent already cleared risk before publication; a mock fill has no prior
	// intent, so it must be gated here.
	snap := s.portfolio.Snapshot()
	if s.riskMgr != nil && !s.riskMgr.Evaluate(fill, snap) {
		http.Error(w, "Rejected by Risk Manager", http.StatusForbidden)
		return
	}

	// Apply through the same path a live execution fill takes, so the mock is
	// journaled, deduplicated, and triggers strategy-state persistence rather
	// than being a bare portfolio mutation. Without this the mock fill never
	// reached the journal and was lost on restart, silently diverging the
	// journal from the portfolio book. OnExecutionFill increments
	// FillsExecutedTotal and refreshes the cash/realized/total gauges; we
	// refresh the unrealized gauge below since it does not. The nil-executor
	// fallback keeps the endpoint working in a minimally-constructed server.
	if s.executor != nil {
		s.executor.OnExecutionFill(r.Context(), fill)
	} else {
		s.portfolio.ApplyFill(fill)
		metrics.FillsExecutedTotal.WithLabelValues(fill.Side.String()).Inc()
	}
	metrics.PortfolioUnrealizedPnL.Set(s.portfolio.Snapshot().UnrealizedPnL)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"status":  "success",
		"message": "Mock fill executed",
		"fill":    fill,
	})
}

// versionHandler returns the build identity as JSON.
func (s *Server) versionHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(version.Get())
}

// strategyConfigHandler handles GET and PUT for /api/strategy/config.
// GET is public; PUT requires the bearer token (if HUGINN_API_TOKEN is set).
func (s *Server) strategyConfigHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		cfg := s.executor.GetConfig()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(cfg)

	case http.MethodPut:
		// Auth check for mutating operation. Fails closed: an unset token
		// refuses the mutation rather than allowing it through.
		if s.apiToken == "" {
			http.Error(w, "Control plane locked: HUGINN_API_TOKEN not configured", http.StatusServiceUnavailable)
			return
		}
		if !s.bearerTokenValid(r) {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		var sc executor.SystemConfig
		if err := json.NewDecoder(r.Body).Decode(&sc); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		s.executor.UpdateConfig(sc)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "updated"})

	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

// snapshotHistoryHandler returns the last N equity samples from the ring buffer.
func (s *Server) snapshotHistoryHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(s.equity.snapshot())
}

// alphaSnapshotProvider is the optional capability a strategy exposes to drive
// the /api/alphas console panel with its live alpha breakdown. CompositeStrategy
// implements it; single strategies do not (the handler synthesizes a one-alpha
// view for those).
type alphaSnapshotProvider interface {
	AlphaSnapshot() strategy.AlphaSnapshot
}

// lastSignaler is an OPTIONAL capability: a single (non-composite) strategy that
// tracks its most recent signal value can implement it so /api/alphas reports a
// real contribution rather than null. Strategies that do not track a last signal
// simply don't implement it, and the handler reports a null contribution — never
// a fabricated number.
type lastSignaler interface {
	LastSignal() (value float64, ok bool)
}

// alphasHandler serves GET /api/alphas: the live alpha breakdown for the active
// strategy. Read-only monitoring endpoint (no auth), CORS via middleware.
//
//   - CompositeStrategy → its real AlphaSnapshot (per-alpha weight, last
//     contribution + confidence, rolling IC history).
//   - Any single strategy → a one-alpha view {name, weight:1, contribution:
//     last signal if the strategy tracks one (else null), confidence:null,
//     ic:[]}.
//   - No executor/strategy wired → {available:false}.
func (s *Server) alphasHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	if s.executor == nil {
		_ = json.NewEncoder(w).Encode(map[string]any{"available": false})
		return
	}
	active := s.executor.Strategy()
	if active == nil {
		_ = json.NewEncoder(w).Encode(map[string]any{"available": false})
		return
	}

	// Composite: real, fully-tracked breakdown.
	if cs, ok := active.(alphaSnapshotProvider); ok {
		_ = json.NewEncoder(w).Encode(cs.AlphaSnapshot())
		return
	}

	// Single strategy: expose it as one alpha. Contribution is the strategy's
	// last signal if it tracks one; otherwise null. Confidence/IC are not
	// tracked for single strategies, so they are null/[] (never fabricated).
	single := strategy.AlphaInfo{
		Name:   active.Name(),
		Weight: 1,
		IC:     []float64{},
	}
	if ls, ok := active.(lastSignaler); ok {
		if v, have := ls.LastSignal(); have {
			vv := v
			single.Contribution = &vv
		}
	}
	snap := strategy.AlphaSnapshot{
		CompositeScore: 0,
		EntryThreshold: 0,
		Blend:          "single",
		Alphas:         []strategy.AlphaInfo{single},
	}
	if single.Contribution != nil {
		snap.CompositeScore = *single.Contribution
	}
	_ = json.NewEncoder(w).Encode(snap)
}

// validationHandler serves GET /api/validation: the walk-forward validation
// results. Reads the artifact at WALKFORWARD_RESULTS_PATH (default
// data/walkforward_results.json), which cmd/walkforward writes as a JSON ARRAY
// of per-fold records. When the file is missing or empty it returns
// {available:false} — it never fabricates folds. When present, it parses the raw
// fold array and DERIVES the console-contract shape (folds, oosFoldsProfitable,
// totalOOSPnL, pbo, deflatedSharpe) from the real numbers.
func (s *Server) validationHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	path := os.Getenv("WALKFORWARD_RESULTS_PATH")
	if path == "" {
		path = defaultWalkforwardResultsPath
	}

	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 {
		_ = json.NewEncoder(w).Encode(map[string]any{"available": false})
		return
	}

	var raw []walkforwardRawFold
	if err := json.Unmarshal(data, &raw); err != nil || len(raw) == 0 {
		// Corrupt or empty-array artifact: treat as "no validation run yet"
		// rather than emitting a malformed/empty-fold response.
		_ = json.NewEncoder(w).Encode(map[string]any{"available": false})
		return
	}

	out := walkforwardResults{Folds: make([]walkforwardFold, 0, len(raw))}
	var dsrSum float64
	var dsrN int
	for _, f := range raw {
		out.Folds = append(out.Folds, walkforwardFold{
			Fold:   f.Fold,
			Train:  joinRange(f.TrainStart, f.TrainEnd),
			Test:   joinRange(f.TestStart, f.TestEnd),
			ISPnL:  f.TrainPnL,
			OOSPnL: f.TestPnL,
		})
		out.TotalOOSPnL += f.TestPnL
		if f.TestPnL > 0 {
			out.OOSFoldsProfitable++
		}
		// Deflated Sharpe is null (in the artifact) / NaN when an OOS window is
		// too short to define it; skip those so the average reflects only defined
		// folds — and stays null when none are defined, rather than a misleading 0.
		if f.DeflatedSharpe != nil && !math.IsNaN(*f.DeflatedSharpe) {
			dsrSum += *f.DeflatedSharpe
			dsrN++
		}
	}
	if dsrN > 0 {
		avg := dsrSum / float64(dsrN)
		out.DeflatedSharpe = &avg
	}
	// PBO (Probability of Backtest Overfitting) proxy: the fraction of folds
	// whose OOS PnL is non-positive — i.e. how often the in-sample selection
	// failed to carry out of sample. Derived from real fold outcomes, not
	// fabricated. (A full combinatorially-symmetric PBO needs the per-combo OOS
	// matrix, which the artifact does not persist; this is the honest fold-level
	// estimate.)
	if n := len(out.Folds); n > 0 {
		out.PBO = float64(n-out.OOSFoldsProfitable) / float64(n)
	}

	_ = json.NewEncoder(w).Encode(out)
}

// joinRange renders a "start → end" window label, tolerating empty bounds.
func joinRange(start, end string) string {
	switch {
	case start == "" && end == "":
		return ""
	case start == "":
		return end
	case end == "":
		return start
	default:
		return start + " → " + end
	}
}

// walkforwardRawFold mirrors the fields cmd/walkforward writes per fold. Only
// the fields the console contract needs are decoded; extras are ignored.
type walkforwardRawFold struct {
	Fold           int      `json:"fold"`
	TrainStart     string   `json:"train_start"`
	TrainEnd       string   `json:"train_end"`
	TestStart      string   `json:"test_start"`
	TestEnd        string   `json:"test_end"`
	TrainPnL       float64  `json:"train_pnl"`
	TestPnL        float64  `json:"test_pnl"`
	DeflatedSharpe *float64 `json:"deflated_sharpe"`
}

// walkforwardFold is one fold in the console-contract response.
type walkforwardFold struct {
	Fold   int     `json:"fold"`
	Train  string  `json:"train"`
	Test   string  `json:"test"`
	ISPnL  float64 `json:"isPnL"`
	OOSPnL float64 `json:"oosPnL"`
}

// walkforwardResults is the console-contract /api/validation response shape.
type walkforwardResults struct {
	Folds              []walkforwardFold `json:"folds"`
	OOSFoldsProfitable int               `json:"oosFoldsProfitable"`
	TotalOOSPnL        float64           `json:"totalOOSPnL"`
	PBO                float64           `json:"pbo"`
	DeflatedSharpe     *float64          `json:"deflatedSharpe"`
}

func (s *Server) Start() error {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.corsMiddleware(s.healthzHandler))
	mux.HandleFunc("/readyz", s.corsMiddleware(s.readyzHandler))
	mux.HandleFunc("/api/snapshot", s.corsMiddleware(s.snapshotHandler))
	mux.HandleFunc("/api/snapshot/history", s.corsMiddleware(s.snapshotHistoryHandler))
	mux.HandleFunc("/api/alphas", s.corsMiddleware(s.alphasHandler))
	mux.HandleFunc("/api/validation", s.corsMiddleware(s.validationHandler))
	mux.HandleFunc("/api/stream", s.streamHandler)
	mux.HandleFunc("/api/breaker/trigger", s.corsMiddleware(s.authMiddleware(s.breakerTriggerHandler)))
	mux.HandleFunc("/api/breaker/reset", s.corsMiddleware(s.authMiddleware(s.breakerResetHandler)))
	mux.HandleFunc("/api/fills/mock", s.corsMiddleware(s.authMiddleware(s.mockFillHandler)))
	mux.HandleFunc("/api/strategy/config", s.corsMiddleware(s.strategyConfigHandler))
	mux.HandleFunc("/version", s.corsMiddleware(s.versionHandler))
	mux.Handle("/metrics", promhttp.Handler())

	s.srv = &http.Server{
		Addr:              s.addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	slog.Info("Starting HTTP server", "addr", s.addr)
	return s.srv.ListenAndServe()
}

func (s *Server) Stop(ctx context.Context) error {
	if s.srv != nil {
		return s.srv.Shutdown(ctx)
	}
	return nil
}
