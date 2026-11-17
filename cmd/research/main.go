// Command research is the Huginn research gateway. It runs nothing. Under Norse
// goal G1.2 every run submission (POST /api/research/runs) is refused, with or
// without a valid token, because H1 and H2 trials run only through norse-stack's
// research/norse_research harness, which writes every run to the hash-chained
// trial ledger. What remains serves the runs earlier versions persisted to
// RESEARCH_RESULTS_DIR, read-only. Walk-forward itself is still available
// offline through cmd/walkforward.
//
// Every route except /healthz requires "Authorization: Bearer <token>". The
// token is read from the file whose path is in HUGINN_RESEARCH_TOKEN_FILE, never
// from a flag or a plain environment value; it must be at least 32 printable,
// non-space ASCII characters after one trailing newline. With no token
// configured, or a file that is unreadable or fails that rule, the API routes
// answer 503. Every route refuses a Host header outside RESEARCH_ALLOWED_HOSTS
// (default 127.0.0.1, localhost and research, on the listen port). No CORS
// headers are sent unless RESEARCH_CONSOLE_ORIGIN names one exact origin.
package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/lgreene03/huginn/internal/version"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	port := os.Getenv("RESEARCH_PORT")
	if port == "" {
		port = "8094"
	}
	resultsDir := os.Getenv("RESEARCH_RESULTS_DIR")
	if resultsDir == "" {
		resultsDir = defaultResultsDir
	}
	// Opt-in, exact match. Unset sends no CORS headers at all.
	consoleOrigin := os.Getenv("RESEARCH_CONSOLE_ORIGIN")
	allowedHosts := parseAllowedHosts(os.Getenv("RESEARCH_ALLOWED_HOSTS"), port)

	// Fail closed rather than refuse to start: an absent, unreadable or invalid
	// token leaves /healthz up and every API route answering 503. The one log
	// line names the cause and never the token.
	token, err := loadToken(os.Getenv(tokenFileEnv))
	if err != nil {
		slog.Error("Research token rejected; API routes will answer 503", "env", tokenFileEnv, "cause", err.Error())
	} else if token == "" {
		slog.Warn("No research token configured; API routes will answer 503", "env", tokenFileEnv)
	}

	addr := ":" + port
	s := NewServer(addr, Config{
		ResultsDir:    resultsDir,
		Token:         token,
		ConsoleOrigin: consoleOrigin,
		AllowedHosts:  allowedHosts,
	})

	srv := &http.Server{
		Addr:              addr,
		Handler:           s.routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		v := version.Get()
		slog.Info("Starting research gateway (read-only; run submission disabled)",
			"addr", addr, "results_dir", resultsDir, "cors_origin", consoleOrigin,
			"allowed_hosts", allowedHosts, "token_configured", token != "",
			"version", v.Version, "git_sha", v.GitSHA)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("HTTP server failed", "error", err)
			os.Exit(1)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop

	slog.Info("Shutting down research gateway")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		slog.Error("Graceful shutdown failed", "error", err)
	}
}
