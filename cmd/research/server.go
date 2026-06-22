package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/lgreene03/huginn/internal/config"
	"github.com/lgreene03/huginn/internal/research"
)

// defaultDataPath is the historical FeatureEvent JSONL used when a run request
// omits the data field. Mirrors cmd/walkforward's default dataset.
const defaultDataPath = "data/btc_test.jsonl"

// defaultResultsDir is where finished runs are persisted (one JSON file per
// run) when RESEARCH_RESULTS_DIR is unset, so results survive a restart.
const defaultResultsDir = "data/research"

// supportedStrategies is the set of strategy names the gateway accepts. Mirrors
// the names cmd/huginn / internal/research understand; the gateway intentionally
// exposes the three the research UI drives (obi mean-reversion, OU, composite).
var supportedStrategies = map[string]bool{
	"obi":       true,
	"ou":        true,
	"composite": true,
}

// runRequest is the POST /api/research/runs body.
type runRequest struct {
	Strategy   string    `json:"strategy"`
	Thresholds []float64 `json:"thresholds"`
	Folds      int       `json:"folds"`
	Data       string    `json:"data,omitempty"`
}

// runResult is the console-contract subset surfaced for a finished run. It
// mirrors the derived shape internal/server/http.go validationHandler emits
// (folds, oosFoldsProfitable, totalOOSPnL, pbo, deflatedSharpe), with
// deflatedSharpe a *float64 so an undefined DSR stays null rather than 0.
type runResult struct {
	Folds              []research.FoldResult `json:"folds"`
	OOSFoldsProfitable int                   `json:"oosFoldsProfitable"`
	TotalOOSPnL        float64               `json:"totalOOSPnL"`
	PBO                *float64              `json:"pbo"`
	DeflatedSharpe     *float64              `json:"deflatedSharpe"`
}

// run is one research job. Guarded by Server.mu when read/written through the
// runs map; the JSON tags define the GET-by-id response shape.
type run struct {
	ID          string     `json:"id"`
	Status      string     `json:"status"` // running | done | error
	Request     runRequest `json:"request"`
	SubmittedAt time.Time  `json:"submittedAt"`
	FinishedAt  *time.Time `json:"finishedAt,omitempty"`
	Result      *runResult `json:"result,omitempty"`
	Error       string     `json:"error,omitempty"`
}

// runSummary is one entry in the GET /api/research/runs list.
type runSummary struct {
	ID          string    `json:"id"`
	Status      string    `json:"status"`
	Strategy    string    `json:"strategy"`
	SubmittedAt time.Time `json:"submittedAt"`
}

// Server is the research gateway. It keeps finished + in-flight runs in an
// in-memory map guarded by a mutex, persists finished runs to resultsDir, and
// loads any existing persisted runs on construction.
type Server struct {
	addr       string
	resultsDir string
	dataPath   string

	mu   sync.Mutex
	runs map[string]*run

	// runWalkforward executes the walk-forward. A field (not a direct call) so
	// tests can inject a fast synthetic implementation instead of replaying a
	// real dataset. Defaults to runWalkforwardReal.
	runWalkforward func(req runRequest) (*runResult, error)
}

// NewServer constructs the gateway, loading any persisted runs from resultsDir.
func NewServer(addr, resultsDir, dataPath string) *Server {
	s := &Server{
		addr:       addr,
		resultsDir: resultsDir,
		dataPath:   dataPath,
		runs:       make(map[string]*run),
	}
	s.runWalkforward = s.runWalkforwardReal
	s.loadPersisted()
	return s
}

// loadPersisted reads every *.json file in resultsDir into the runs map so
// finished results survive a restart. Unreadable/corrupt files are skipped with
// a warning rather than failing startup.
func (s *Server) loadPersisted() {
	entries, err := os.ReadDir(s.resultsDir)
	if err != nil {
		// Missing dir is normal on first boot; nothing to load.
		return
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(s.resultsDir, e.Name()))
		if err != nil {
			slog.Warn("Skipping unreadable persisted run", "file", e.Name(), "error", err)
			continue
		}
		var r run
		if err := json.Unmarshal(data, &r); err != nil || r.ID == "" {
			slog.Warn("Skipping corrupt persisted run", "file", e.Name(), "error", err)
			continue
		}
		s.runs[r.ID] = &r
	}
	slog.Info("Loaded persisted research runs", "count", len(s.runs))
}

// persist writes a finished run to resultsDir/<id>.json. Best-effort: a write
// failure is logged but does not change the in-memory result.
func (s *Server) persist(r *run) {
	if err := os.MkdirAll(s.resultsDir, 0o755); err != nil {
		slog.Error("Failed to create results dir", "dir", s.resultsDir, "error", err)
		return
	}
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		slog.Error("Failed to marshal run for persistence", "id", r.ID, "error", err)
		return
	}
	path := filepath.Join(s.resultsDir, r.ID+".json")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		slog.Error("Failed to persist run", "id", r.ID, "path", path, "error", err)
	}
}

// corsMiddleware adds permissive CORS headers. Unlike internal/server's
// dashboard-scoped middleware, the research gateway is read-only (no mutating
// control endpoints) so it allows all origins ("*"), matching the open-CORS
// intent for a read-only research UI. Same OPTIONS-preflight short-circuit
// pattern as internal/server/http.go corsMiddleware.
func (s *Server) corsMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Vary", "Origin")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}

		next(w, r)
	}
}

func (s *Server) healthzHandler(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("OK"))
}

// runsHandler dispatches GET (list) and POST (submit) on /api/research/runs.
func (s *Server) runsHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.listRuns(w, r)
	case http.MethodPost:
		s.submitRun(w, r)
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

// submitRun validates the request, registers a running job, kicks off the
// walk-forward in a goroutine, and returns 202 {id, status:"running"}.
func (s *Server) submitRun(w http.ResponseWriter, r *http.Request) {
	var req runRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
		return
	}

	req.Strategy = strings.TrimSpace(req.Strategy)
	if !supportedStrategies[req.Strategy] {
		http.Error(w, fmt.Sprintf("unknown strategy %q (want obi|ou|composite)", req.Strategy), http.StatusBadRequest)
		return
	}
	if req.Folds <= 0 {
		http.Error(w, "folds must be > 0", http.StatusBadRequest)
		return
	}
	if req.Data == "" {
		req.Data = s.dataPath
	}

	id := newRunID()
	r0 := &run{
		ID:          id,
		Status:      "running",
		Request:     req,
		SubmittedAt: time.Now().UTC(),
	}

	s.mu.Lock()
	s.runs[id] = r0
	s.mu.Unlock()

	go s.execute(id, req)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]string{"id": id, "status": "running"})
}

// execute runs the walk-forward for a submitted job and records the outcome,
// then persists the finished run.
func (s *Server) execute(id string, req runRequest) {
	res, err := s.runWalkforward(req)

	now := time.Now().UTC()
	s.mu.Lock()
	r := s.runs[id]
	if r != nil {
		r.FinishedAt = &now
		if err != nil {
			r.Status = "error"
			r.Error = err.Error()
		} else {
			r.Status = "done"
			r.Result = res
		}
	}
	rCopy := r
	s.mu.Unlock()

	if rCopy != nil {
		s.persist(rCopy)
	}
}

// runWalkforwardReal builds a base config, overrides the strategy, constructs
// the grid from the request thresholds, loads the dataset, and runs
// internal/research.Run. It maps research.Result into the console-contract
// runResult (deflatedSharpe / pbo stay null when undefined).
func (s *Server) runWalkforwardReal(req runRequest) (*runResult, error) {
	cfg := baseConfig(req.Strategy)

	events, err := research.LoadEvents(req.Data)
	if err != nil {
		return nil, fmt.Errorf("load events: %w", err)
	}
	if len(events) < 100 {
		return nil, fmt.Errorf("not enough events for walk-forward: %d (need ≥100)", len(events))
	}

	grid := research.BuildGrid(cfg, req.Thresholds, nil)

	res, err := research.Run(research.Options{
		Config:  cfg,
		Events:  events,
		Folds:   req.Folds,
		TestPct: 0.2,
		Grid:    grid,
	})
	if err != nil {
		return nil, err
	}
	return toRunResult(res), nil
}

// toRunResult maps a research.Result into the console-contract response. PBO is
// surfaced as *float64 so a NaN (too few folds/configs) becomes null instead of
// failing the JSON encode.
func toRunResult(res research.Result) *runResult {
	out := &runResult{
		Folds:              res.Folds,
		OOSFoldsProfitable: res.OOSFoldsProfitable,
		TotalOOSPnL:        res.TotalOOSPnL,
		DeflatedSharpe:     res.DeflatedSharpe,
	}
	if !math.IsNaN(res.PBO) && !math.IsInf(res.PBO, 0) {
		pbo := res.PBO
		out.PBO = &pbo
	}
	return out
}

// baseConfig returns a self-contained config (no YAML file dependency, so tests
// need no configs/ on disk) with the given strategy name and sane defaults that
// match config.Load's fallbacks.
//
// The Risk and Executor defaults are LOAD-BEARING and must mirror config.Load:
// the risk manager treats PositionLimitHard==0 as a hard cap of zero notional,
// so omitting it rejects EVERY fill and the walk-forward reports 0 trades / 0
// PnL. The executor cost/slippage match configs/default.yaml so a gateway run
// reproduces the same net result as `go run ./cmd/walkforward`.
func baseConfig(strategyName string) *config.Config {
	cfg := &config.Config{}
	cfg.Strategy.Name = strategyName
	cfg.Strategy.Threshold = 0.5
	cfg.Strategy.OrderSize = 0.01
	cfg.Strategy.FastPeriod = 10
	cfg.Strategy.SlowPeriod = 30
	cfg.Capital.InitialCash = 100_000.0
	// Executor: match configs/default.yaml so PnL is cost-consistent with the CLI.
	cfg.Executor.TransactionCostBps = 5.0
	cfg.Executor.SlippageBps = 2.0
	// Risk: match config.Load's post-parse fallbacks (config.go). Without a
	// non-zero PositionLimitHard the risk manager throttles every order to a
	// zero-notional cap → no fills.
	cfg.Risk.MaxDrawdownPct = 0.20
	cfg.Risk.DailyLossLimit = 10000.0
	cfg.Risk.PositionLimitHard = 500000.0
	return cfg
}

// getRunHandler serves GET /api/research/runs/{id}: the full run record, or 404.
func (s *Server) getRunHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/research/runs/")
	if id == "" || strings.Contains(id, "/") {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	s.mu.Lock()
	r0 := s.runs[id]
	s.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	if r0 == nil {
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "run not found"})
		return
	}
	_ = json.NewEncoder(w).Encode(r0)
}

// listRuns serves GET /api/research/runs: a newest-first array of summaries.
func (s *Server) listRuns(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	summaries := make([]runSummary, 0, len(s.runs))
	for _, rn := range s.runs {
		summaries = append(summaries, runSummary{
			ID:          rn.ID,
			Status:      rn.Status,
			Strategy:    rn.Request.Strategy,
			SubmittedAt: rn.SubmittedAt,
		})
	}
	s.mu.Unlock()

	sort.Slice(summaries, func(i, j int) bool {
		return summaries[i].SubmittedAt.After(summaries[j].SubmittedAt)
	})

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(summaries)
}

// routes builds the mux. /api/research/runs handles list+submit; the trailing
// slash route handles GET-by-id (Go's ServeMux longest-prefix match routes
// /api/research/runs/{id} here while /api/research/runs hits the exact handler).
func (s *Server) routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.corsMiddleware(s.healthzHandler))
	mux.HandleFunc("/api/research/runs", s.corsMiddleware(s.runsHandler))
	mux.HandleFunc("/api/research/runs/", s.corsMiddleware(s.getRunHandler))
	return mux
}

// newRunID returns a time-ordered, collision-resistant run id.
func newRunID() string {
	return fmt.Sprintf("run-%d-%04d", time.Now().UnixNano(), randSeq())
}
