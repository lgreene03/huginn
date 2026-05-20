package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/lgreene03/huginn/internal/executor"
	"github.com/lgreene03/huginn/internal/metrics"
	"github.com/lgreene03/huginn/internal/model"
	"github.com/lgreene03/huginn/internal/portfolio"
	"github.com/lgreene03/huginn/internal/risk"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type Server struct {
	addr       string
	portfolio  *portfolio.Portfolio
	riskMgr    *risk.Manager
	executor   *executor.Executor
	isReady    bool
	readyMutex sync.RWMutex
	srv        *http.Server
}

func New(addr string, portf *portfolio.Portfolio, riskMgr *risk.Manager, exec *executor.Executor) *Server {
	return &Server{
		addr:      addr,
		portfolio: portf,
		riskMgr:   riskMgr,
		executor:  exec,
	}
}

func (s *Server) SetReady(ready bool) {
	s.readyMutex.Lock()
	defer s.readyMutex.Unlock()
	s.isReady = ready
}

// corsMiddleware adds standard CORS headers to allow frontend integration.
func corsMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
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

func (s *Server) Start() error {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", corsMiddleware(s.healthzHandler))
	mux.HandleFunc("/readyz", corsMiddleware(s.readyzHandler))
	mux.HandleFunc("/api/stream", s.streamHandler)
	mux.HandleFunc("/api/breaker/trigger", corsMiddleware(s.breakerTriggerHandler))
	mux.HandleFunc("/api/breaker/reset", corsMiddleware(s.breakerResetHandler))
	mux.HandleFunc("/api/fills/mock", corsMiddleware(s.mockFillHandler))
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
