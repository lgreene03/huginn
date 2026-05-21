package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
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
	"github.com/lgreene03/huginn/internal/version"
)

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
	// apiToken gates mutating endpoints. Read from HUGINN_API_TOKEN env var;
	// empty string disables auth (backward-compatible default).
	apiToken string
	equity   *equityRing
}

func New(addr string, portf *portfolio.Portfolio, riskMgr *risk.Manager, exec *executor.Executor) *Server {
	return &Server{
		addr:      addr,
		portfolio: portf,
		riskMgr:   riskMgr,
		executor:  exec,
		apiToken:  os.Getenv("HUGINN_API_TOKEN"),
		equity:    newEquityRing(720),
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

// corsMiddleware adds standard CORS headers to allow frontend integration.
func corsMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}

		next(w, r)
	}
}

// authMiddleware enforces bearer-token auth when HUGINN_API_TOKEN is set.
// A missing or incorrect token returns 401. When the env var is empty,
// all requests pass through (backward-compatible).
func (s *Server) authMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.apiToken != "" && r.Header.Get("Authorization") != "Bearer "+s.apiToken {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
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
	s.readyMutex.RUnlock()

	if ready {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	} else {
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte("Not Ready"))
	}
}

// streamHandler implements Server-Sent Events (SSE) to push live engine state to the web UI.
func (s *Server) streamHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")

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

			fmt.Fprintf(w, "data: %s\n\n", string(data))
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
	w.Write([]byte(`{"status":"halted","message":"Strategy execution manually halted."}`))
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
	w.Write([]byte(`{"status":"running","message":"Strategy execution manually resumed."}`))
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

	fill := model.Fill{
		OrderID:         fmt.Sprintf("mock-fill-%d", time.Now().UnixNano()),
		Instrument:      req.Instrument,
		Side:            side,
		Quantity:        req.Quantity,
		FillPrice:       req.Price,
		TransactionCost: req.Price * req.Quantity * 0.0005, // 5 bps cost
		SlippageBps:     5,
		Timestamp:       time.Now(),
	}

	// Run risk evaluation before applying!
	snap := s.portfolio.Snapshot()
	if s.riskMgr != nil && !s.riskMgr.Evaluate(fill, snap) {
		http.Error(w, "Rejected by Risk Manager", http.StatusForbidden)
		return
	}

	s.portfolio.ApplyFill(fill)

	// Update portfolio metrics gauges
	metricsSnap := s.portfolio.Snapshot()
	metrics.PortfolioCash.Set(metricsSnap.Cash)
	metrics.PortfolioRealizedPnL.Set(metricsSnap.RealizedPnL)
	metrics.PortfolioUnrealizedPnL.Set(metricsSnap.UnrealizedPnL)
	metrics.PortfolioTotalValue.Set(metricsSnap.TotalValue)
	metrics.FillsExecutedTotal.WithLabelValues(fill.Side.String()).Inc()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":  "success",
		"message": "Mock fill executed",
		"fill":    fill,
	})
}

// versionHandler returns the build identity as JSON.
func (s *Server) versionHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(version.Get())
}

// strategyConfigHandler handles GET and PUT for /api/strategy/config.
// GET is public; PUT requires the bearer token (if HUGINN_API_TOKEN is set).
func (s *Server) strategyConfigHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		cfg := s.executor.GetConfig()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(cfg)

	case http.MethodPut:
		// Auth check for mutating operation.
		if s.apiToken != "" && r.Header.Get("Authorization") != "Bearer "+s.apiToken {
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
		json.NewEncoder(w).Encode(map[string]string{"status": "updated"})

	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

// snapshotHistoryHandler returns the last N equity samples from the ring buffer.
func (s *Server) snapshotHistoryHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(s.equity.snapshot())
}

func (s *Server) Start() error {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", corsMiddleware(s.healthzHandler))
	mux.HandleFunc("/readyz", corsMiddleware(s.readyzHandler))
	mux.HandleFunc("/api/snapshot", corsMiddleware(s.snapshotHandler))
	mux.HandleFunc("/api/snapshot/history", corsMiddleware(s.snapshotHistoryHandler))
	mux.HandleFunc("/api/stream", s.streamHandler)
	mux.HandleFunc("/api/breaker/trigger", corsMiddleware(s.authMiddleware(s.breakerTriggerHandler)))
	mux.HandleFunc("/api/breaker/reset", corsMiddleware(s.authMiddleware(s.breakerResetHandler)))
	mux.HandleFunc("/api/fills/mock", corsMiddleware(s.authMiddleware(s.mockFillHandler)))
	mux.HandleFunc("/api/strategy/config", corsMiddleware(s.strategyConfigHandler))
	mux.HandleFunc("/version", corsMiddleware(s.versionHandler))
	mux.Handle("/metrics", promhttp.Handler())

	s.srv = &http.Server{
		Addr:    s.addr,
		Handler: mux,
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
