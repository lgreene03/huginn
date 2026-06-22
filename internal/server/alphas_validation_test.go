package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/lgreene03/huginn/internal/executor"
	"github.com/lgreene03/huginn/internal/portfolio"
	"github.com/lgreene03/huginn/internal/strategy"
)

// newTestServerWithStrategy wires a minimal server whose executor runs the given
// strategy, so the alphas handler can reach it via executor.Strategy().
func newTestServerWithStrategy(t *testing.T, s strategy.Strategy) *Server {
	t.Helper()
	port := portfolio.New(100_000)
	exec := executor.New(s, port, nil, nil, executor.Config{}, false, nil, "test")
	return New(":0", port, nil, exec)
}

// TestAlphasHandler_Composite asserts /api/alphas returns the composite's real
// alpha breakdown shape: compositeScore, entryThreshold, blend, and a per-alpha
// list with weight + (null-before-run) contribution/confidence and empty IC.
func TestAlphasHandler_Composite(t *testing.T) {
	comp := strategy.NewCompositeStrategy(strategy.CompositeConfig{
		Name: "Composite",
		Alphas: []strategy.WeightedAlpha{
			{Alpha: strategy.FieldAlpha{AlphaName: "obi", Field: "obi", Scale: 1, Conf: 1}, Weight: 0.7},
			{Alpha: strategy.MomentumAlpha{AlphaName: "momentum"}, Weight: 0.3},
		},
		BlendMode:      strategy.BlendWeightedSum,
		EntryThreshold: 0.5,
		OrderSize:      1,
	})
	srv := newTestServerWithStrategy(t, comp)

	req := httptest.NewRequest(http.MethodGet, "/api/alphas", nil)
	rec := httptest.NewRecorder()
	srv.alphasHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	var got strategy.AlphaSnapshot
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v; body=%s", err, rec.Body.String())
	}
	if got.Blend != "weighted_sum" {
		t.Errorf("blend = %q, want weighted_sum", got.Blend)
	}
	if got.EntryThreshold != 0.5 {
		t.Errorf("entryThreshold = %v, want 0.5", got.EntryThreshold)
	}
	if len(got.Alphas) != 2 {
		t.Fatalf("len(alphas) = %d, want 2", len(got.Alphas))
	}
	if got.Alphas[0].Name != "obi" || got.Alphas[0].Weight != 0.7 {
		t.Errorf("alpha[0] = %+v, want name=obi weight=0.7", got.Alphas[0])
	}
	// Before any OnFeature the contribution/confidence must be null, IC empty.
	if got.Alphas[0].Contribution != nil {
		t.Errorf("contribution = %v, want null before run", *got.Alphas[0].Contribution)
	}
	if got.Alphas[0].IC == nil {
		t.Errorf("ic = nil, want empty array (non-null) in JSON")
	}
}

// TestAlphasHandler_SingleStrategy asserts a non-composite strategy is exposed
// as a single alpha {name, weight:1, ic:[]}.
func TestAlphasHandler_SingleStrategy(t *testing.T) {
	obi := strategy.NewOBIThreshold(0.7, 0.01, 10)
	srv := newTestServerWithStrategy(t, obi)

	req := httptest.NewRequest(http.MethodGet, "/api/alphas", nil)
	rec := httptest.NewRecorder()
	srv.alphasHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got strategy.AlphaSnapshot
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Alphas) != 1 {
		t.Fatalf("len(alphas) = %d, want 1", len(got.Alphas))
	}
	if got.Alphas[0].Name != obi.Name() {
		t.Errorf("alpha name = %q, want %q", got.Alphas[0].Name, obi.Name())
	}
	if got.Alphas[0].Weight != 1 {
		t.Errorf("weight = %v, want 1", got.Alphas[0].Weight)
	}
	if got.Alphas[0].IC == nil || len(got.Alphas[0].IC) != 0 {
		t.Errorf("ic = %v, want empty array", got.Alphas[0].IC)
	}
}

// TestValidationHandler_MissingFile asserts a missing artifact yields
// {available:false} rather than fabricated folds.
func TestValidationHandler_MissingFile(t *testing.T) {
	t.Setenv("WALKFORWARD_RESULTS_PATH", filepath.Join(t.TempDir(), "does_not_exist.json"))
	srv := New(":0", portfolio.New(0), nil, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/validation", nil)
	rec := httptest.NewRecorder()
	srv.validationHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if avail, _ := got["available"].(bool); avail {
		t.Errorf("available = true, want false for missing file")
	}
}

// TestValidationHandler_EmptyFile asserts an empty (0-byte) artifact also yields
// {available:false}.
func TestValidationHandler_EmptyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "walkforward_results.json")
	if err := os.WriteFile(path, []byte{}, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WALKFORWARD_RESULTS_PATH", path)
	srv := New(":0", portfolio.New(0), nil, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/validation", nil)
	rec := httptest.NewRecorder()
	srv.validationHandler(rec, req)

	var got map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if avail, _ := got["available"].(bool); avail {
		t.Errorf("available = true, want false for empty file")
	}
}

// TestValidationHandler_PresentArtifact asserts a real fold-array artifact is
// parsed and the console-contract fields are DERIVED from the fold numbers.
func TestValidationHandler_PresentArtifact(t *testing.T) {
	// Raw artifact shape == what cmd/walkforward writes: a JSON array of folds.
	artifact := `[
		{"fold":1,"train_start":"t0","train_end":"t1","test_start":"t1","test_end":"t2","train_pnl":10.0,"test_pnl":5.0,"deflated_sharpe":0.8},
		{"fold":2,"train_start":"t1","train_end":"t2","test_start":"t2","test_end":"t3","train_pnl":8.0,"test_pnl":-3.0,"deflated_sharpe":0.2}
	]`
	path := filepath.Join(t.TempDir(), "walkforward_results.json")
	if err := os.WriteFile(path, []byte(artifact), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WALKFORWARD_RESULTS_PATH", path)
	srv := New(":0", portfolio.New(0), nil, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/validation", nil)
	rec := httptest.NewRecorder()
	srv.validationHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got walkforwardResults
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v; body=%s", err, rec.Body.String())
	}
	if len(got.Folds) != 2 {
		t.Fatalf("folds = %d, want 2", len(got.Folds))
	}
	if got.Folds[0].OOSPnL != 5.0 || got.Folds[0].ISPnL != 10.0 {
		t.Errorf("fold[0] pnl = oos %v is %v, want oos 5 is 10", got.Folds[0].OOSPnL, got.Folds[0].ISPnL)
	}
	if got.Folds[0].Train != "t0 → t1" {
		t.Errorf("fold[0] train = %q, want 't0 → t1'", got.Folds[0].Train)
	}
	if got.TotalOOSPnL != 2.0 { // 5 + (-3)
		t.Errorf("totalOOSPnL = %v, want 2.0", got.TotalOOSPnL)
	}
	if got.OOSFoldsProfitable != 1 {
		t.Errorf("oosFoldsProfitable = %d, want 1", got.OOSFoldsProfitable)
	}
	if got.PBO != 0.5 { // 1 of 2 folds non-positive OOS
		t.Errorf("pbo = %v, want 0.5", got.PBO)
	}
	if got.DeflatedSharpe == nil || *got.DeflatedSharpe != 0.5 { // mean(0.8, 0.2)
		t.Errorf("deflatedSharpe = %v, want 0.5", got.DeflatedSharpe)
	}
}

// TestValidationHandler_NullDeflatedSharpe asserts that when every fold's
// deflated_sharpe is null (undefined on a too-short OOS window — exactly what
// cmd/walkforward now writes), the derived deflatedSharpe is null rather than a
// misleading averaged 0. The other fields still derive from the real PnLs.
func TestValidationHandler_NullDeflatedSharpe(t *testing.T) {
	artifact := `[
		{"fold":1,"train_start":"t0","train_end":"t1","test_start":"t1","test_end":"t2","train_pnl":10.0,"test_pnl":-5.0,"deflated_sharpe":null},
		{"fold":2,"train_start":"t1","train_end":"t2","test_start":"t2","test_end":"t3","train_pnl":8.0,"test_pnl":-3.0,"deflated_sharpe":null}
	]`
	path := filepath.Join(t.TempDir(), "walkforward_results.json")
	if err := os.WriteFile(path, []byte(artifact), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WALKFORWARD_RESULTS_PATH", path)
	srv := New(":0", portfolio.New(0), nil, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/validation", nil)
	rec := httptest.NewRecorder()
	srv.validationHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got walkforwardResults
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v; body=%s", err, rec.Body.String())
	}
	if got.DeflatedSharpe != nil {
		t.Errorf("deflatedSharpe = %v, want null (no fold defines it)", *got.DeflatedSharpe)
	}
	if got.OOSFoldsProfitable != 0 || got.PBO != 1.0 {
		t.Errorf("oosProfitable=%d pbo=%v, want 0 and 1.0", got.OOSFoldsProfitable, got.PBO)
	}
}
