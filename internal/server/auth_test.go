package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// okHandler is a trivial next-handler that records it was reached.
func okHandler(reached *bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		*reached = true
		w.WriteHeader(http.StatusOK)
	}
}

// TestAuthMiddleware_FailsClosedWhenTokenUnset is the regression guard for
// sre-resilience-12 / sec-secrets-auth-2: with no HUGINN_API_TOKEN configured,
// a mutating control endpoint must be refused (503), not passed through.
func TestAuthMiddleware_FailsClosedWhenTokenUnset(t *testing.T) {
	s := &Server{apiToken: ""}
	reached := false
	h := s.authMiddleware(okHandler(&reached))

	rr := httptest.NewRecorder()
	h(rr, httptest.NewRequest(http.MethodPost, "/api/breaker/trigger", nil))

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when token unset, got %d", rr.Code)
	}
	if reached {
		t.Fatal("handler must not be reached when control plane is locked")
	}
}

func TestAuthMiddleware_RejectsMissingToken(t *testing.T) {
	s := &Server{apiToken: "secret"}
	reached := false
	h := s.authMiddleware(okHandler(&reached))

	rr := httptest.NewRecorder()
	h(rr, httptest.NewRequest(http.MethodPost, "/api/breaker/trigger", nil))

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 when token missing, got %d", rr.Code)
	}
	if reached {
		t.Fatal("handler must not be reached without a valid token")
	}
}

func TestAuthMiddleware_RejectsWrongToken(t *testing.T) {
	s := &Server{apiToken: "secret"}
	reached := false
	h := s.authMiddleware(okHandler(&reached))

	req := httptest.NewRequest(http.MethodPost, "/api/breaker/trigger", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	rr := httptest.NewRecorder()
	h(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 with wrong token, got %d", rr.Code)
	}
	if reached {
		t.Fatal("handler must not be reached with a wrong token")
	}
}

func TestAuthMiddleware_AllowsCorrectToken(t *testing.T) {
	s := &Server{apiToken: "secret"}
	reached := false
	h := s.authMiddleware(okHandler(&reached))

	req := httptest.NewRequest(http.MethodPost, "/api/breaker/trigger", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rr := httptest.NewRecorder()
	h(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 with correct token, got %d", rr.Code)
	}
	if !reached {
		t.Fatal("handler must be reached with a valid token")
	}
}

// TestAuthMiddleware_PreflightPassesThrough confirms CORS preflight is not
// blocked by the fail-closed gate (it carries no credentials).
func TestAuthMiddleware_PreflightPassesThrough(t *testing.T) {
	s := &Server{apiToken: ""}
	reached := false
	h := s.authMiddleware(okHandler(&reached))

	rr := httptest.NewRecorder()
	h(rr, httptest.NewRequest(http.MethodOptions, "/api/breaker/trigger", nil))

	if !reached {
		t.Fatal("OPTIONS preflight must pass through to the CORS layer")
	}
}

// TestCORSMiddleware_ScopesToConfiguredOrigin is the regression guard for
// sec-secrets-auth-6: the Allow-Origin header must echo the configured
// dashboard origin, never "*".
func TestCORSMiddleware_ScopesToConfiguredOrigin(t *testing.T) {
	s := &Server{corsOrigin: "http://localhost:8084"}
	h := s.corsMiddleware(func(w http.ResponseWriter, r *http.Request) {})

	rr := httptest.NewRecorder()
	h(rr, httptest.NewRequest(http.MethodGet, "/api/snapshot", nil))

	got := rr.Header().Get("Access-Control-Allow-Origin")
	if got != "http://localhost:8084" {
		t.Fatalf("Allow-Origin = %q, want http://localhost:8084", got)
	}
	if got == "*" {
		t.Fatal("Allow-Origin must never be * on token-gated stack")
	}
}
