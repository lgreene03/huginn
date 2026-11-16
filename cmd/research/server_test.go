package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lgreene03/huginn/internal/research"
)

// The tests that carry a name from norse-stack's docs/research/ledger-threat-model.md
// (G1.2 table) keep that name exactly: the threat model treats the names as a
// contract.

// testToken meets loadToken's rule (at least 32 printable, non-space ASCII).
const testToken = "test-token-0123456789abcdef0123456789abcdef"

const foreignOrigin = "http://evil.example"

// consoleOrigin is the origin the opt-in CORS tests configure.
const consoleOrigin = "http://localhost:8090"

// allowedHost is the Host header serve sends unless a test sets another.
const allowedHost = "127.0.0.1:8094"

// testConfig is the configuration newTestServer uses: the test token, no CORS
// origin (the default) and the default host allowlist for port 8094.
func testConfig(dir string) Config {
	return Config{ResultsDir: dir, Token: testToken, AllowedHosts: defaultAllowedHosts("8094")}
}

// newTestServer builds a gateway from testConfig and the given results dir (an
// empty temp dir when dir is "").
func newTestServer(t *testing.T, dir string) *Server {
	t.Helper()
	if dir == "" {
		dir = t.TempDir()
	}
	return NewServer(":0", testConfig(dir))
}

// serve sends one request through the real mux, so the middleware order (Host,
// CORS, token, handler) is what is tested. auth is the whole Authorization
// header value, or "" for none; headers are extra request headers, where "Host"
// sets the request's Host (allowedHost otherwise).
func serve(s *Server, method, path, body, auth string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Host = allowedHost
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	for k, v := range headers {
		if k == "Host" {
			req.Host = v
			continue
		}
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)
	return rec
}

func bearer(token string) string { return "Bearer " + token }

// writePersistedRun writes r to dir/<id>.json as an earlier gateway version did.
func writePersistedRun(t *testing.T, dir string, r run) {
	t.Helper()
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal run: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, r.ID+".json"), data, 0o600); err != nil {
		t.Fatalf("write run: %v", err)
	}
}

// assertNothingScheduledOrWritten checks a refused submission left no trace:
// no run in memory and no file in the results dir.
func assertNothingScheduledOrWritten(t *testing.T, s *Server, dir string) {
	t.Helper()
	if n := len(s.runs); n != 0 {
		t.Errorf("runs in memory = %d, want 0", n)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read results dir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("results dir holds %d entries, want 0", len(entries))
	}
}

const runBody = `{"strategy":"obi","thresholds":[0.5,0.7],"folds":3}`

func TestRunWithoutTokenIsRefused(t *testing.T) {
	dir := t.TempDir()
	s := newTestServer(t, dir)

	rec := serve(s, http.MethodPost, "/api/research/runs", runBody, "",
		map[string]string{"Content-Type": "application/json"})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("WWW-Authenticate"); !strings.HasPrefix(got, "Bearer") {
		t.Errorf("WWW-Authenticate = %q, want a Bearer challenge", got)
	}
	assertNothingScheduledOrWritten(t, s, dir)
}

func TestRunWithWrongTokenIsRefused(t *testing.T) {
	cases := []struct {
		name string
		auth string
	}{
		{"wrong token same length", bearer("x" + testToken[1:])},
		{"wrong token shorter", bearer("nope")},
		{"wrong token longer", bearer(testToken + "-extra")},
		{"token without bearer prefix", testToken},
		{"prefix only", "Bearer "},
		{"case-mismatched scheme", "bearer " + testToken},
		{"basic scheme", "Basic " + testToken},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			s := newTestServer(t, dir)
			rec := serve(s, http.MethodPost, "/api/research/runs", runBody, tc.auth,
				map[string]string{"Content-Type": "application/json"})
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401; body=%s", rec.Code, rec.Body.String())
			}
			assertNothingScheduledOrWritten(t, s, dir)
		})
	}
}

// A cross-site "simple" POST (text/plain, no preflight) from a foreign page
// carries no token, so it is refused, and the response is not readable by that
// page because no Access-Control-Allow-Origin is sent.
func TestSimpleTextPlainPostWithoutTokenIsRefused(t *testing.T) {
	dir := t.TempDir()
	s := newTestServer(t, dir)

	rec := serve(s, http.MethodPost, "/api/research/runs", runBody, "",
		map[string]string{"Content-Type": "text/plain", "Origin": foreignOrigin})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("Access-Control-Allow-Origin = %q, want none for a foreign origin", got)
	}
	assertNothingScheduledOrWritten(t, s, dir)
}

// The run endpoint is disabled outright: a valid token and a well-formed body
// still get a JSON refusal pointing at the ledgered harness, and nothing runs.
func TestRunIsRefusedWithAValidToken(t *testing.T) {
	for _, ctype := range []string{"application/json", "text/plain"} {
		t.Run(ctype, func(t *testing.T) {
			dir := t.TempDir()
			s := newTestServer(t, dir)

			rec := serve(s, http.MethodPost, "/api/research/runs", runBody, bearer(testToken),
				map[string]string{"Content-Type": ctype})
			if rec.Code != http.StatusMethodNotAllowed {
				t.Fatalf("status = %d, want 405; body=%s", rec.Code, rec.Body.String())
			}
			if got := rec.Header().Get("Allow"); got != "GET, OPTIONS" {
				t.Errorf("Allow = %q, want GET, OPTIONS", got)
			}
			if got := rec.Header().Get("Content-Type"); got != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", got)
			}
			var body map[string]string
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("refusal is not JSON: %v; body=%s", err, rec.Body.String())
			}
			for _, want := range []string{"disabled", "norse_research", "ledger"} {
				if !strings.Contains(body["error"], want) {
					t.Errorf("error = %q, want it to mention %q", body["error"], want)
				}
			}
			assertNothingScheduledOrWritten(t, s, dir)
		})
	}
}

func TestResultsRequireToken(t *testing.T) {
	dir := t.TempDir()
	writePersistedRun(t, dir, run{ID: "run-1", Status: "done", SubmittedAt: time.Now().UTC()})
	s := newTestServer(t, dir)

	for _, path := range []string{"/api/research/runs", "/api/research/runs/run-1", "/api/research/runs/run-missing"} {
		for _, auth := range []string{"", bearer("wrong")} {
			rec := serve(s, http.MethodGet, path, "", auth, nil)
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("GET %s with auth %q: status = %d, want 401", path, auth, rec.Code)
			}
			if strings.Contains(rec.Body.String(), "run-1") {
				t.Errorf("GET %s with auth %q leaked a run id: %s", path, auth, rec.Body.String())
			}
		}
	}
}

// With no token configured every API route fails closed with 503, whatever the
// request carries, including an "Authorization: Bearer " with an empty token.
func TestNoTokenConfiguredAnswers503(t *testing.T) {
	dir := t.TempDir()
	writePersistedRun(t, dir, run{ID: "run-1", Status: "done", SubmittedAt: time.Now().UTC()})
	cfg := testConfig(dir)
	cfg.Token = ""
	s := NewServer(":0", cfg)

	cases := []struct {
		method, path string
	}{
		{http.MethodGet, "/api/research/runs"},
		{http.MethodGet, "/api/research/runs/run-1"},
		{http.MethodPost, "/api/research/runs"},
	}
	for _, tc := range cases {
		for _, auth := range []string{"", "Bearer ", bearer(testToken)} {
			rec := serve(s, tc.method, tc.path, runBody, auth, nil)
			if rec.Code != http.StatusServiceUnavailable {
				t.Errorf("%s %s with auth %q: status = %d, want 503", tc.method, tc.path, auth, rec.Code)
			}
		}
	}

	if rec := serve(s, http.MethodGet, "/healthz", "", "", nil); rec.Code != http.StatusOK {
		t.Errorf("healthz with no token configured: status = %d, want 200", rec.Code)
	}
}

// writeTokenFile writes content to a fresh file and returns its path.
func writeTokenFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadToken(t *testing.T) {
	if tok, err := loadToken(""); err != nil || tok != "" {
		t.Errorf("loadToken(\"\") = %q, %v; want empty token and no error", tok, err)
	}

	hex64 := strings.Repeat("0123456789abcdef", 4)
	for name, content := range map[string]string{
		"openssl rand -hex 32 output": hex64 + "\n",
		"no trailing newline":         hex64,
		"exactly 32 characters":       hex64[:32] + "\n",
		"printable punctuation":       "!~" + hex64[:30],
	} {
		want := strings.TrimSuffix(content, "\n")
		if tok, err := loadToken(writeTokenFile(t, content)); err != nil || tok != want {
			t.Errorf("%s: loadToken = %q, %v; want the token with one trailing newline removed", name, tok, err)
		}
	}

	if tok, err := loadToken(filepath.Join(t.TempDir(), "absent")); err == nil || tok != "" {
		t.Errorf("loadToken(missing file) = %q, %v; want empty token and an error", tok, err)
	}
}

// A weak or malformed token file is rejected (so the API answers 503) with an
// error naming the cause and never the file's content.
func TestLoadTokenRejectsWeakOrMalformedTokens(t *testing.T) {
	hex64 := strings.Repeat("0123456789abcdef", 4)
	cases := []struct {
		name, content string
		want          error
	}{
		{"empty file", "", errTokenTooShort},
		{"only a newline", "\n", errTokenTooShort},
		{"one character", "a\n", errTokenTooShort},
		{"31 characters", hex64[:31] + "\n", errTokenTooShort},
		{"two lines from >>", hex64 + "\n" + hex64 + "\n", errTokenBadChar},
		{"two trailing newlines", hex64 + "\n\n", errTokenBadChar},
		{"CRLF ending", hex64 + "\r\n", errTokenBadChar},
		{"leading space", " " + hex64, errTokenBadChar},
		{"internal space", hex64[:20] + " " + hex64[20:], errTokenBadChar},
		{"tab", hex64 + "\t", errTokenBadChar},
		{"control character", hex64 + "\x1f", errTokenBadChar},
		{"DEL", hex64 + "\x7f", errTokenBadChar},
		{"non-ASCII", hex64 + "é", errTokenBadChar},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tok, err := loadToken(writeTokenFile(t, tc.content))
			if tok != "" || !errors.Is(err, tc.want) {
				t.Fatalf("loadToken = %q, %v; want empty token and %v", tok, err, tc.want)
			}
			if strings.Contains(err.Error(), "0123456789") {
				t.Errorf("error %q carries the file's content", err)
			}
		})
	}
}

func TestHealthzIsOpen(t *testing.T) {
	s := newTestServer(t, "")
	rec := serve(s, http.MethodGet, "/healthz", "", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("healthz status = %d, want 200 with no token", rec.Code)
	}
}

// Earlier gateway versions persisted runs to the results dir; with a valid token
// they are listed newest first and served by id.
func TestPersistedRunsAreServedWithAValidToken(t *testing.T) {
	dir := t.TempDir()
	older := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	newer := older.Add(time.Hour)
	meanDSR := 2.0
	writePersistedRun(t, dir, run{
		ID: "run-old", Status: "done", SubmittedAt: older,
		Request: runRequest{Strategy: "obi", Thresholds: []float64{0.5}, Folds: 2},
		Result:  &runResult{OOSFoldsProfitable: 3, TotalOOSPnL: 42.0, MeanFoldDeflatedSharpe: &meanDSR},
	})
	writePersistedRun(t, dir, run{
		ID: "run-new", Status: "error", SubmittedAt: newer, Error: "boom",
		Request: runRequest{Strategy: "ou", Thresholds: []float64{1.0}, Folds: 2},
	})
	s := newTestServer(t, dir)

	rec := serve(s, http.MethodGet, "/api/research/runs", "", bearer(testToken), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var list []runSummary
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode list: %v; body=%s", err, rec.Body.String())
	}
	if len(list) != 2 {
		t.Fatalf("list len = %d, want 2", len(list))
	}
	if list[0].ID != "run-new" || list[1].ID != "run-old" {
		t.Errorf("order = [%s,%s], want [run-new,run-old] (newest first)", list[0].ID, list[1].ID)
	}
	if list[0].Strategy != "ou" {
		t.Errorf("list[0].Strategy = %q, want ou", list[0].Strategy)
	}

	rec = serve(s, http.MethodGet, "/api/research/runs/run-old", "", bearer(testToken), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("get status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var got run
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode run: %v", err)
	}
	if got.Status != "done" || got.Result == nil || got.Result.TotalOOSPnL != 42.0 {
		t.Errorf("run = %+v, want done with totalOOSPnL 42.0", got)
	}
}

func TestGetUnknownRun404(t *testing.T) {
	s := newTestServer(t, "")
	rec := serve(s, http.MethodGet, "/api/research/runs/run-does-not-exist", "", bearer(testToken), nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

// TestRunResultJSONUsesFoldQualifiedKeys pins the wire names of a served run's
// result. huginn's figures are a leave-one-fold-out PBO proxy and a mean of
// per-fold deflated Sharpes, not the evaluation gate's CSCV PBO or deflated
// Sharpe (norse-stack research/norse_research, goal G1.3), so the encoded run
// must carry foldPboProxy and meanFoldDeflatedSharpe and must never carry the
// unqualified pbo or deflatedSharpe keys. The per-fold records must likewise use
// fold_deflated_sharpe rather than deflated_sharpe.
func TestRunResultJSONUsesFoldQualifiedKeys(t *testing.T) {
	s := newTestServer(t, "")
	foldPBO := 0.25
	meanDSR := 1.5
	const id = "run-json-keys"
	s.runs[id] = &run{
		ID: id, Status: "done", SubmittedAt: time.Now().UTC(),
		Result: &runResult{
			Folds:                  []research.FoldResult{{Fold: 1, TestPnL: 12.5}, {Fold: 2, TestPnL: -3.0}},
			OOSFoldsProfitable:     1,
			TotalOOSPnL:            9.5,
			FoldPBOProxy:           &foldPBO,
			MeanFoldDeflatedSharpe: &meanDSR,
		},
	}

	rec := serve(s, http.MethodGet, "/api/research/runs/"+id, "", bearer(testToken), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("get status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode run: %v; body=%s", err, rec.Body.String())
	}
	result, ok := body["result"].(map[string]any)
	if !ok {
		t.Fatalf("result missing or not an object; body=%s", rec.Body.String())
	}

	if v, ok := result["foldPboProxy"].(float64); !ok || v != 0.25 {
		t.Errorf("result.foldPboProxy = %v, want 0.25", result["foldPboProxy"])
	}
	if v, ok := result["meanFoldDeflatedSharpe"].(float64); !ok || v != 1.5 {
		t.Errorf("result.meanFoldDeflatedSharpe = %v, want 1.5", result["meanFoldDeflatedSharpe"])
	}
	for _, stale := range []string{"pbo", "deflatedSharpe"} {
		if _, present := result[stale]; present {
			t.Errorf("result carries the unqualified key %q; it must not", stale)
		}
	}

	folds, ok := result["folds"].([]any)
	if !ok || len(folds) == 0 {
		t.Fatalf("result.folds missing or empty; body=%s", rec.Body.String())
	}
	fold0, ok := folds[0].(map[string]any)
	if !ok {
		t.Fatalf("result.folds[0] is not an object; body=%s", rec.Body.String())
	}
	if _, present := fold0["fold_deflated_sharpe"]; !present {
		t.Errorf("result.folds[0] lacks fold_deflated_sharpe")
	}
	if _, present := fold0["deflated_sharpe"]; present {
		t.Errorf("result.folds[0] carries the unqualified key deflated_sharpe; it must not")
	}
}

// By default no origin is trusted and no response carries
// Access-Control-Allow-Origin. With RESEARCH_CONSOLE_ORIGIN set, only that exact
// origin is echoed. "*" is never sent, Vary: Origin always is, and a preflight
// is answered (204) without a token.
func TestCorsAllowsOnlyTheConsoleOrigin(t *testing.T) {
	byDefault := newTestServer(t, "")
	cfg := testConfig(t.TempDir())
	cfg.ConsoleOrigin = consoleOrigin
	optedIn := NewServer(":0", cfg)

	type want struct {
		status int
		acao   string
	}
	cases := []struct {
		name   string
		s      *Server
		method string
		path   string
		origin string
		auth   string
		want   want
	}{
		{"default: preflight from the console", byDefault, http.MethodOptions, "/api/research/runs", consoleOrigin, "", want{http.StatusNoContent, ""}},
		{"default: preflight from a foreign origin", byDefault, http.MethodOptions, "/api/research/runs", foreignOrigin, "", want{http.StatusNoContent, ""}},
		{"default: preflight with no origin", byDefault, http.MethodOptions, "/api/research/runs", "", "", want{http.StatusNoContent, ""}},
		{"default: GET from the console", byDefault, http.MethodGet, "/api/research/runs", consoleOrigin, bearer(testToken), want{http.StatusOK, ""}},
		{"default: GET from a foreign origin", byDefault, http.MethodGet, "/api/research/runs", foreignOrigin, bearer(testToken), want{http.StatusOK, ""}},
		{"opt-in: preflight from the console", optedIn, http.MethodOptions, "/api/research/runs", consoleOrigin, "", want{http.StatusNoContent, consoleOrigin}},
		{"opt-in: preflight by id from the console", optedIn, http.MethodOptions, "/api/research/runs/run-1", consoleOrigin, "", want{http.StatusNoContent, consoleOrigin}},
		{"opt-in: preflight from a foreign origin", optedIn, http.MethodOptions, "/api/research/runs", foreignOrigin, "", want{http.StatusNoContent, ""}},
		{"opt-in: preflight from the console on another port", optedIn, http.MethodOptions, "/api/research/runs", "http://localhost:8091", "", want{http.StatusNoContent, ""}},
		{"opt-in: preflight from a lookalike origin", optedIn, http.MethodOptions, "/api/research/runs", consoleOrigin + ".evil.example", "", want{http.StatusNoContent, ""}},
		{"opt-in: preflight from a null origin", optedIn, http.MethodOptions, "/api/research/runs", "null", "", want{http.StatusNoContent, ""}},
		{"opt-in: preflight with no origin", optedIn, http.MethodOptions, "/api/research/runs", "", "", want{http.StatusNoContent, ""}},
		{"opt-in: GET from the console", optedIn, http.MethodGet, "/api/research/runs", consoleOrigin, bearer(testToken), want{http.StatusOK, consoleOrigin}},
		{"opt-in: GET from a foreign origin with the token", optedIn, http.MethodGet, "/api/research/runs", foreignOrigin, bearer(testToken), want{http.StatusOK, ""}},
		{"opt-in: GET from a foreign origin without the token", optedIn, http.MethodGet, "/api/research/runs", foreignOrigin, "", want{http.StatusUnauthorized, ""}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := tc.s
			headers := map[string]string{"Access-Control-Request-Method": http.MethodGet}
			if tc.origin != "" {
				headers["Origin"] = tc.origin
			}
			rec := serve(s, tc.method, tc.path, "", tc.auth, headers)
			if rec.Code != tc.want.status {
				t.Errorf("status = %d, want %d", rec.Code, tc.want.status)
			}
			got := rec.Header().Get("Access-Control-Allow-Origin")
			if got == "*" {
				t.Fatal("Access-Control-Allow-Origin is *; it must never be")
			}
			if got != tc.want.acao {
				t.Errorf("Access-Control-Allow-Origin = %q, want %q", got, tc.want.acao)
			}
			if !strings.Contains(rec.Header().Get("Vary"), "Origin") {
				t.Errorf("Vary = %q, want it to include Origin", rec.Header().Get("Vary"))
			}
			if tc.want.acao != "" && strings.Contains(rec.Header().Get("Access-Control-Allow-Methods"), http.MethodPost) {
				t.Errorf("Access-Control-Allow-Methods = %q; POST is disabled and must not be offered",
					rec.Header().Get("Access-Control-Allow-Methods"))
			}
		})
	}
}

// The allowed origin is whatever the gateway is configured with
// (RESEARCH_CONSOLE_ORIGIN), not a hard-coded value.
func TestCorsOriginIsConfigurable(t *testing.T) {
	const custom = "http://127.0.0.1:9000"
	cfg := testConfig(t.TempDir())
	cfg.ConsoleOrigin = custom
	s := NewServer(":0", cfg)

	rec := serve(s, http.MethodOptions, "/api/research/runs", "", "", map[string]string{"Origin": custom})
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != custom {
		t.Errorf("configured origin: Access-Control-Allow-Origin = %q, want %q", got, custom)
	}
	rec = serve(s, http.MethodOptions, "/api/research/runs", "", "", map[string]string{"Origin": consoleOrigin})
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("another origin when one is configured: Access-Control-Allow-Origin = %q, want none", got)
	}
}

// A Host header outside the allowlist is refused with 403 on every route and
// method, token or not, before CORS or the token check, so a DNS-rebinding page
// gets nothing (threat model T26). The allowed names pass, case-insensitively.
func TestForeignHostHeaderIsRefused(t *testing.T) {
	dir := t.TempDir()
	writePersistedRun(t, dir, run{ID: "run-1", Status: "done", SubmittedAt: time.Now().UTC()})
	s := newTestServer(t, dir)

	foreign := []string{
		"rebind.attacker.example", "rebind.attacker.example:8094", "evil.example:8094",
		"localhost", "localhost:8095", "127.0.0.1:8094.evil.example", "127.0.0.2:8094", "",
	}
	routes := []struct{ method, path, auth string }{
		{http.MethodGet, "/api/research/runs", bearer(testToken)},
		{http.MethodGet, "/api/research/runs/run-1", bearer(testToken)},
		{http.MethodGet, "/healthz", ""},
		{http.MethodPost, "/api/research/runs", bearer(testToken)},
		{http.MethodOptions, "/api/research/runs", ""},
	}
	for _, host := range foreign {
		for _, rt := range routes {
			rec := serve(s, rt.method, rt.path, runBody, rt.auth,
				map[string]string{"Host": host, "Origin": consoleOrigin})
			if rec.Code != http.StatusForbidden {
				t.Errorf("%s %s with Host %q: status = %d, want 403", rt.method, rt.path, host, rec.Code)
			}
			if strings.Contains(rec.Body.String(), "run-1") {
				t.Errorf("%s %s with Host %q leaked a run id: %s", rt.method, rt.path, host, rec.Body.String())
			}
			if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
				t.Errorf("%s %s with Host %q: Access-Control-Allow-Origin = %q, want none", rt.method, rt.path, host, got)
			}
		}
	}

	for _, host := range []string{"127.0.0.1:8094", "localhost:8094", "research:8094", "LOCALHOST:8094"} {
		rec := serve(s, http.MethodGet, "/api/research/runs/run-1", "", bearer(testToken), map[string]string{"Host": host})
		if rec.Code != http.StatusOK {
			t.Errorf("GET with allowed Host %q: status = %d, want 200", host, rec.Code)
		}
	}
}

// RESEARCH_ALLOWED_HOSTS replaces the default allowlist; unset or blank gives
// 127.0.0.1, localhost and research on the listen port.
func TestAllowedHostsFromEnv(t *testing.T) {
	cases := []struct {
		env, port string
		want      []string
	}{
		{"", "8094", []string{"127.0.0.1:8094", "localhost:8094", "research:8094"}},
		{"  ", "9000", []string{"127.0.0.1:9000", "localhost:9000", "research:9000"}},
		{" Gateway.internal:8094 , 10.0.0.5:8094 ,", "8094", []string{"gateway.internal:8094", "10.0.0.5:8094"}},
	}
	for _, tc := range cases {
		got := parseAllowedHosts(tc.env, tc.port)
		if strings.Join(got, ",") != strings.Join(tc.want, ",") {
			t.Errorf("parseAllowedHosts(%q, %q) = %v, want %v", tc.env, tc.port, got, tc.want)
		}
	}

	cfg := testConfig(t.TempDir())
	cfg.AllowedHosts = parseAllowedHosts("gateway.internal:8094", "8094")
	s := NewServer(":0", cfg)
	if rec := serve(s, http.MethodGet, "/healthz", "", "", map[string]string{"Host": "gateway.internal:8094"}); rec.Code != http.StatusOK {
		t.Errorf("configured host: status = %d, want 200", rec.Code)
	}
	if rec := serve(s, http.MethodGet, "/healthz", "", "", map[string]string{"Host": "127.0.0.1:8094"}); rec.Code != http.StatusForbidden {
		t.Errorf("default host when the list is replaced: status = %d, want 403", rec.Code)
	}
}
