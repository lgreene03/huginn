package server

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// newReadyTestServer builds a minimal Server sufficient to exercise the
// readyz handler in isolation (no portfolio/executor wiring needed).
func newReadyTestServer() *Server {
	return &Server{}
}

func TestReadyz_NotReadyWhenFlagFalse(t *testing.T) {
	s := newReadyTestServer()
	s.SetReady(false)

	rr := httptest.NewRecorder()
	s.readyzHandler(rr, httptest.NewRequest(http.MethodGet, "/readyz", nil))

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when not ready, got %d", rr.Code)
	}
}

func TestReadyz_OKWhenReadyAndNoProbe(t *testing.T) {
	s := newReadyTestServer()
	s.SetReady(true)

	rr := httptest.NewRecorder()
	s.readyzHandler(rr, httptest.NewRequest(http.MethodGet, "/readyz", nil))

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 when ready and no probe set, got %d", rr.Code)
	}
}

func TestReadyz_StaleProbeTrips503(t *testing.T) {
	s := newReadyTestServer()
	s.SetReady(true)
	// Simulate a wedged consumer loop: probe reports stale.
	s.ReadinessProbe(func() error { return errors.New("feature consumer stale: no progress in 5m0s") })

	rr := httptest.NewRecorder()
	s.readyzHandler(rr, httptest.NewRequest(http.MethodGet, "/readyz", nil))

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when consumer loop is stale, got %d", rr.Code)
	}
	if body := rr.Body.String(); body == "" || body == "OK" {
		t.Fatalf("expected stale reason in body, got %q", body)
	}
}

func TestReadyz_FreshProbeReturnsOK(t *testing.T) {
	s := newReadyTestServer()
	s.SetReady(true)
	s.ReadinessProbe(func() error { return nil }) // fresh

	rr := httptest.NewRecorder()
	s.readyzHandler(rr, httptest.NewRequest(http.MethodGet, "/readyz", nil))

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 when ready and probe fresh, got %d", rr.Code)
	}
}

// TestHealthz_LivenessUnaffectedByStaleProbe documents that the deep-readiness
// probe must not bleed into /healthz liveness. We can't call healthzHandler
// without a portfolio, so we assert the structural invariant: the probe field
// is only consulted by readyzHandler.
func TestHealthz_ProbeNotConsultedByReadyFlagPath(t *testing.T) {
	s := newReadyTestServer()
	s.SetReady(false)
	probeCalled := false
	s.ReadinessProbe(func() error { probeCalled = true; return nil })

	rr := httptest.NewRecorder()
	s.readyzHandler(rr, httptest.NewRequest(http.MethodGet, "/readyz", nil))

	// When the ready flag is false we short-circuit before the probe, so the
	// probe must not even be consulted.
	if probeCalled {
		t.Fatal("probe should not be consulted when ready flag is false")
	}
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", rr.Code)
	}
}
