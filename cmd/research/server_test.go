package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lgreene03/huginn/internal/research"
)

// newTestServer builds a gateway with an isolated (empty) results dir and a fast
// stub walk-forward so handler tests don't replay a real dataset. The stub
// returns a fixed, recognisable result.
func newTestServer(t *testing.T) *Server {
	t.Helper()
	s := NewServer(":0", t.TempDir(), defaultDataPath)
	pbo := 0.25
	dsr := 1.5
	s.runWalkforward = func(req runRequest) (*runResult, error) {
		return &runResult{
			Folds: []research.FoldResult{
				{Fold: 1, TestPnL: 12.5},
				{Fold: 2, TestPnL: -3.0},
			},
			OOSFoldsProfitable: 1,
			TotalOOSPnL:        9.5,
			PBO:                &pbo,
			DeflatedSharpe:     &dsr,
		}, nil
	}
	return s
}

// submit posts a run and returns the decoded {id,status} body.
func submit(t *testing.T, s *Server, body string) map[string]string {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/research/runs", bytes.NewReader([]byte(body)))
	rec := httptest.NewRecorder()
	s.corsMiddleware(s.runsHandler)(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("submit status = %d, want 202; body=%s", rec.Code, rec.Body.String())
	}
	var out map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode submit: %v; body=%s", err, rec.Body.String())
	}
	return out
}

// getRun fetches a run by id and returns the recorder.
func getRun(t *testing.T, s *Server, id string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/research/runs/"+id, nil)
	rec := httptest.NewRecorder()
	s.corsMiddleware(s.getRunHandler)(rec, req)
	return rec
}

func TestSubmitPollUntilDone(t *testing.T) {
	s := newTestServer(t)

	resp := submit(t, s, `{"strategy":"obi","thresholds":[0.5,0.7],"folds":3}`)
	id := resp["id"]
	if id == "" {
		t.Fatal("empty id in submit response")
	}
	if resp["status"] != "running" {
		t.Errorf("submit status field = %q, want running", resp["status"])
	}

	// Poll until done (stub is fast; bound the wait so a hang fails loudly).
	var got run
	deadline := time.Now().Add(2 * time.Second)
	for {
		rec := getRun(t, s, id)
		if rec.Code != http.StatusOK {
			t.Fatalf("get status = %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
		got = run{}
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode run: %v; body=%s", err, rec.Body.String())
		}
		if got.Status != "running" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("run did not finish before deadline")
		}
		time.Sleep(5 * time.Millisecond)
	}

	if got.Status != "done" {
		t.Fatalf("final status = %q, want done; error=%q", got.Status, got.Error)
	}
	if got.FinishedAt == nil {
		t.Error("finishedAt is nil on a done run")
	}
	if got.Request.Strategy != "obi" {
		t.Errorf("request.strategy = %q, want obi", got.Request.Strategy)
	}
	if got.Result == nil {
		t.Fatal("result is nil on a done run")
	}
	if len(got.Result.Folds) != 2 {
		t.Errorf("result.folds len = %d, want 2", len(got.Result.Folds))
	}
	if got.Result.OOSFoldsProfitable != 1 {
		t.Errorf("oosFoldsProfitable = %d, want 1", got.Result.OOSFoldsProfitable)
	}
	if got.Result.TotalOOSPnL != 9.5 {
		t.Errorf("totalOOSPnL = %v, want 9.5", got.Result.TotalOOSPnL)
	}
	if got.Result.PBO == nil || *got.Result.PBO != 0.25 {
		t.Errorf("pbo = %v, want 0.25", got.Result.PBO)
	}
	if got.Result.DeflatedSharpe == nil || *got.Result.DeflatedSharpe != 1.5 {
		t.Errorf("deflatedSharpe = %v, want 1.5", got.Result.DeflatedSharpe)
	}
}

func TestListRunsNewestFirst(t *testing.T) {
	s := newTestServer(t)
	id1 := submit(t, s, `{"strategy":"obi","thresholds":[0.5],"folds":2}`)["id"]
	time.Sleep(2 * time.Millisecond)
	id2 := submit(t, s, `{"strategy":"ou","thresholds":[1.0],"folds":2}`)["id"]

	req := httptest.NewRequest(http.MethodGet, "/api/research/runs", nil)
	rec := httptest.NewRecorder()
	s.corsMiddleware(s.runsHandler)(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d, want 200", rec.Code)
	}
	var list []runSummary
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode list: %v; body=%s", err, rec.Body.String())
	}
	if len(list) != 2 {
		t.Fatalf("list len = %d, want 2", len(list))
	}
	// Newest-first: id2 submitted last must be first.
	if list[0].ID != id2 || list[1].ID != id1 {
		t.Errorf("order = [%s,%s], want [%s,%s] (newest first)", list[0].ID, list[1].ID, id2, id1)
	}
	if list[0].Strategy != "ou" {
		t.Errorf("list[0].Strategy = %q, want ou", list[0].Strategy)
	}
}

func TestGetUnknownRun404(t *testing.T) {
	s := newTestServer(t)
	rec := getRun(t, s, "run-does-not-exist")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

func TestSubmitBadStrategy400(t *testing.T) {
	s := newTestServer(t)
	req := httptest.NewRequest(http.MethodPost, "/api/research/runs",
		bytes.NewReader([]byte(`{"strategy":"nope","thresholds":[0.5],"folds":2}`)))
	rec := httptest.NewRecorder()
	s.corsMiddleware(s.runsHandler)(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "unknown strategy") {
		t.Errorf("body = %q, want it to mention unknown strategy", rec.Body.String())
	}
}

func TestSubmitBadFolds400(t *testing.T) {
	s := newTestServer(t)
	req := httptest.NewRequest(http.MethodPost, "/api/research/runs",
		bytes.NewReader([]byte(`{"strategy":"obi","thresholds":[0.5],"folds":0}`)))
	rec := httptest.NewRecorder()
	s.corsMiddleware(s.runsHandler)(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHealthz(t *testing.T) {
	s := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	s.corsMiddleware(s.healthzHandler)(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("healthz status = %d, want 200", rec.Code)
	}
}

func TestCORSAllowsAllOrigins(t *testing.T) {
	s := newTestServer(t)
	req := httptest.NewRequest(http.MethodOptions, "/api/research/runs", nil)
	rec := httptest.NewRecorder()
	s.corsMiddleware(s.runsHandler)(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("preflight status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("Access-Control-Allow-Origin = %q, want *", got)
	}
}

// TestRealWalkforwardEndToEnd exercises the REAL runWalkforwardReal path (no
// stub) against the committed btc_test.jsonl dataset: submit → poll → assert the
// finished run carries a fold array and the console-contract aggregates. No
// Kafka/Postgres involved — it only replays the on-disk JSONL.
func TestRealWalkforwardEndToEnd(t *testing.T) {
	s := NewServer(":0", t.TempDir(), "../../data/btc_test.jsonl")

	id := submit(t, s, `{"strategy":"obi","thresholds":[0.5,0.7],"folds":4}`)["id"]
	deadline := time.Now().Add(15 * time.Second)
	var got run
	for {
		rec := getRun(t, s, id)
		got = run{}
		_ = json.Unmarshal(rec.Body.Bytes(), &got)
		if got.Status != "running" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("real walk-forward did not finish in time")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got.Status != "done" {
		t.Fatalf("status = %q, want done; error=%q", got.Status, got.Error)
	}
	if got.Result == nil || len(got.Result.Folds) == 0 {
		t.Fatalf("result missing folds: %+v", got.Result)
	}
	if got.Result.OOSFoldsProfitable < 0 || got.Result.OOSFoldsProfitable > len(got.Result.Folds) {
		t.Errorf("oosFoldsProfitable = %d, out of range for %d folds",
			got.Result.OOSFoldsProfitable, len(got.Result.Folds))
	}
}

// TestRunPersistedAndReloaded asserts a finished run is written to the results
// dir and re-loaded by a fresh Server (results survive restart).
func TestRunPersistedAndReloaded(t *testing.T) {
	dir := t.TempDir()
	s := NewServer(":0", dir, defaultDataPath)
	dsr := 2.0
	s.runWalkforward = func(req runRequest) (*runResult, error) {
		return &runResult{OOSFoldsProfitable: 3, TotalOOSPnL: 42.0, DeflatedSharpe: &dsr}, nil
	}

	id := submit(t, s, `{"strategy":"composite","thresholds":[0.5],"folds":2}`)["id"]
	deadline := time.Now().Add(2 * time.Second)
	for {
		rec := getRun(t, s, id)
		var r run
		_ = json.Unmarshal(rec.Body.Bytes(), &r)
		if r.Status == "done" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("run did not finish")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Fresh server over the same dir must reload the finished run.
	s2 := NewServer(":0", dir, defaultDataPath)
	rec := getRun(t, s2, id)
	if rec.Code != http.StatusOK {
		t.Fatalf("reloaded get status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var got run
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode reloaded run: %v", err)
	}
	if got.Status != "done" || got.Result == nil || got.Result.TotalOOSPnL != 42.0 {
		t.Errorf("reloaded run = %+v, want done with totalOOSPnL 42.0", got)
	}
}

// TestBaseConfigEnablesTrading pins the load-bearing risk/executor defaults on
// baseConfig. The risk manager treats PositionLimitHard==0 as a zero-notional
// cap that rejects EVERY fill, so a missing default silently turns every
// walk-forward run into 0 trades / 0 PnL (regression: the gateway reported a
// flat 0 result while the equivalent CLI run produced the real -146 PnL).
func TestBaseConfigEnablesTrading(t *testing.T) {
	cfg := baseConfig("obi")
	if cfg.Risk.PositionLimitHard <= 0 {
		t.Errorf("Risk.PositionLimitHard = %v, want > 0 (a zero hard limit rejects every fill → 0 trades)", cfg.Risk.PositionLimitHard)
	}
	if cfg.Risk.MaxDrawdownPct <= 0 || cfg.Risk.DailyLossLimit <= 0 {
		t.Errorf("Risk drawdown/daily-loss = %v/%v, want > 0 to match config.Load fallbacks", cfg.Risk.MaxDrawdownPct, cfg.Risk.DailyLossLimit)
	}
	if cfg.Executor.TransactionCostBps <= 0 || cfg.Executor.SlippageBps <= 0 {
		t.Errorf("Executor tx/slippage bps = %v/%v, want > 0 to match configs/default.yaml so PnL is cost-consistent with the CLI", cfg.Executor.TransactionCostBps, cfg.Executor.SlippageBps)
	}
}
