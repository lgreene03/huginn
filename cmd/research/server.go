package main

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/lgreene03/huginn/internal/research"
)

// defaultResultsDir is where runs persisted by earlier versions of the gateway
// (one JSON file per run) are read from when RESEARCH_RESULTS_DIR is unset.
const defaultResultsDir = "data/research"

// defaultConsoleOrigin is the one browser origin CORS is sent to when
// RESEARCH_CONSOLE_ORIGIN is unset: the Norse console's own origin, which
// norse-stack's console/serve.py serves on port 8090 and announces as
// http://localhost:8090. The console proxies the gateway same-origin, so a
// browser never needs CORS from here; anything else gets none.
const defaultConsoleOrigin = "http://localhost:8090"

// tokenFileEnv names the environment variable that holds the PATH of the file
// containing the bearer token. The token itself is never read from a flag or a
// plain environment value (Norse goal G0.4, credentials by file).
const tokenFileEnv = "HUGINN_RESEARCH_TOKEN_FILE"

// runsDisabledMessage is the error every POST /api/research/runs receives (Norse
// goal G1.2). Trials are counted only when the hash-chained trial ledger records
// them, and only norse-stack's harness writes that ledger, so a run started here
// would be an unledgered trial.
const runsDisabledMessage = "run submission is disabled: H1 and H2 trials run only through " +
	"norse-stack's research/norse_research harness (harness.run), which writes every run to the " +
	"hash-chained trial ledger; this gateway serves earlier runs read-only"

// runRequest is the request recorded with a run persisted by an earlier version
// of the gateway. Nothing submits one any more; it is kept so those runs decode.
type runRequest struct {
	Strategy   string    `json:"strategy"`
	Thresholds []float64 `json:"thresholds"`
	Folds      int       `json:"folds"`
	Data       string    `json:"data,omitempty"`
}

// runResult is the console-contract subset surfaced for a finished run. It
// shares folds, oosFoldsProfitable, totalOOSPnL and meanFoldDeflatedSharpe with
// the shape internal/server/http.go validationHandler emits, with
// meanFoldDeflatedSharpe a *float64 so an undefined mean stays null rather than
// 0. Where validationHandler can only report unprofitableFoldShare, this carries
// foldPboProxy, the rank-based leave-one-fold-out proxy, because a gateway run
// had the per-config OOS matrix. Neither figure is the evaluation gate's PBO or
// deflated Sharpe (norse-stack research/norse_research, goal G1.3).
type runResult struct {
	Folds                  []research.FoldResult `json:"folds"`
	OOSFoldsProfitable     int                   `json:"oosFoldsProfitable"`
	TotalOOSPnL            float64               `json:"totalOOSPnL"`
	FoldPBOProxy           *float64              `json:"foldPboProxy"`
	MeanFoldDeflatedSharpe *float64              `json:"meanFoldDeflatedSharpe"`
}

// run is one persisted research job; the JSON tags define the GET-by-id
// response shape and the on-disk format.
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

// Server is the research gateway. It runs nothing: it loads the runs earlier
// versions persisted to resultsDir and serves them, read-only, to callers that
// present the bearer token.
type Server struct {
	addr          string
	resultsDir    string
	token         string // empty means no token configured: API routes answer 503
	consoleOrigin string // the only origin Access-Control-Allow-Origin is sent to

	mu   sync.Mutex
	runs map[string]*run
}

// NewServer constructs the gateway, loading any persisted runs from resultsDir.
// An empty token leaves every API route answering 503.
func NewServer(addr, resultsDir, token, consoleOrigin string) *Server {
	s := &Server{
		addr:          addr,
		resultsDir:    resultsDir,
		token:         token,
		consoleOrigin: consoleOrigin,
		runs:          make(map[string]*run),
	}
	s.loadPersisted()
	return s
}

// loadToken reads the bearer token from the file at path. An empty path means
// no token is configured, which is not an error: the API then answers 503.
// Surrounding whitespace is trimmed, so the trailing newline left by
// `openssl rand -hex 32 > file` is not part of the token.
func loadToken(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	//nolint:gosec // G703: path is the operator-configured HUGINN_RESEARCH_TOKEN_FILE env var, not request input.
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read token file: %w", err)
	}
	return strings.TrimSpace(string(data)), nil
}

// loadPersisted reads every *.json file in resultsDir into the runs map.
// Unreadable/corrupt files are skipped with a warning rather than failing
// startup.
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

// corsMiddleware sends CORS headers to the Norse console's own origin and to
// no other. Access-Control-Allow-Origin echoes the request's Origin only when it
// equals s.consoleOrigin, and is never "*". Vary: Origin is always set so a
// cache cannot serve one origin's response to another. A preflight (OPTIONS) is
// answered here with 204 and never reaches the token check, because browsers
// send no credentials on a preflight; without the allowed origin it carries no
// Access-Control-Allow-Origin, so the browser blocks the real request.
func (s *Server) corsMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Vary", "Origin")
		if origin := r.Header.Get("Origin"); origin != "" && origin == s.consoleOrigin {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization")
		}

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		next(w, r)
	}
}

// requireToken enforces the bearer token on an API route. It FAILS CLOSED, as
// huginn's control plane does with an empty HUGINN_API_TOKEN: with no token
// configured every request is refused with 503 rather than let through. With a
// token configured, a missing or wrong Authorization header gets 401. Reading
// results needs the token too, because viewing a result is what makes a trial
// (docs/research/ledger-threat-model.md in norse-stack, T26).
func (s *Server) requireToken(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.token == "" {
			writeJSONError(w, http.StatusServiceUnavailable,
				"research gateway locked: no token configured (set "+tokenFileEnv+" to the token file's path)")
			return
		}
		if !bearerTokenValid(r, s.token) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="huginn-research"`)
			writeJSONError(w, http.StatusUnauthorized, "unauthorized: send Authorization: Bearer <token>")
			return
		}
		next(w, r)
	}
}

// bearerTokenValid reports whether the request carries exactly
// "Authorization: Bearer <token>". The comparison is constant-time
// (crypto/subtle), as in internal/server, so the token cannot be recovered byte
// by byte from response times; ConstantTimeCompare returns 0 on a length
// mismatch. The caller must already have refused an empty token.
func bearerTokenValid(r *http.Request, token string) bool {
	expected := "Bearer " + token
	provided := r.Header.Get("Authorization")
	return subtle.ConstantTimeCompare([]byte(provided), []byte(expected)) == 1
}

// writeJSONError writes {"error": msg} with the given status.
func writeJSONError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func (s *Server) healthzHandler(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("OK"))
}

// runsHandler dispatches GET (list) and POST (refused) on /api/research/runs.
func (s *Server) runsHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.listRuns(w, r)
	case http.MethodPost:
		refuseRun(w)
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

// refuseRun answers every run submission with 405 and runsDisabledMessage,
// whatever the token or body. The body is never read and nothing is scheduled
// or written. There is deliberately no flag or environment variable that turns
// submission back on: the code that ran a walk-forward has been removed, so
// re-enabling runs means writing the ledgered path G1.2 describes (datasets by
// manifest hash, refusal when the ledger entry cannot be written).
func refuseRun(w http.ResponseWriter) {
	w.Header().Set("Allow", "GET, OPTIONS")
	writeJSONError(w, http.StatusMethodNotAllowed, runsDisabledMessage)
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

	// Marshal the run under the lock, so its fields are read in one critical
	// section with every other access to the runs map.
	s.mu.Lock()
	r0 := s.runs[id]
	var data []byte
	var marshalErr error
	if r0 != nil {
		data, marshalErr = json.Marshal(r0)
	}
	s.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	if r0 == nil {
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "run not found"})
		return
	}
	if marshalErr != nil {
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "failed to encode run"})
		return
	}
	_, _ = w.Write(data)
}

// listRuns serves GET /api/research/runs: a newest-first array of summaries.
//
//nolint:unparam // r is required by the http.HandlerFunc signature even though this handler ignores the request.
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

// routes builds the mux. /healthz is the only route without the token, so
// container healthchecks need no credential. /api/research/runs handles list
// (and refuses submission); the trailing slash route handles GET-by-id (Go's
// ServeMux longest-prefix match routes /api/research/runs/{id} there while
// /api/research/runs hits the exact handler). CORS wraps the token check so a
// preflight is answered before credentials are demanded.
func (s *Server) routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.corsMiddleware(s.healthzHandler))
	mux.HandleFunc("/api/research/runs", s.corsMiddleware(s.requireToken(s.runsHandler)))
	mux.HandleFunc("/api/research/runs/", s.corsMiddleware(s.requireToken(s.getRunHandler)))
	return mux
}
