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
// from a flag or a plain environment value; with no token configured, or an
// unreadable file, the API routes answer 503. CORS is sent only to
// RESEARCH_CONSOLE_ORIGIN (default http://localhost:8090, the Norse console).
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
	consoleOrigin := os.Getenv("RESEARCH_CONSOLE_ORIGIN")
	if consoleOrigin == "" {
		consoleOrigin = defaultConsoleOrigin
	}

	// Fail closed rather than refuse to start: an unreadable or absent token
	// leaves /healthz up and every API route answering 503.
	token, err := loadToken(os.Getenv(tokenFileEnv))
	if err != nil {
		slog.Error("Research token unreadable; API routes will answer 503", "env", tokenFileEnv, "error", err)
	} else if token == "" {
		slog.Warn("No research token configured; API routes will answer 503", "env", tokenFileEnv)
	}

	addr := ":" + port
	s := NewServer(addr, resultsDir, token, consoleOrigin)

	srv := &http.Server{
		Addr:              addr,
		Handler:           s.routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		v := version.Get()
		slog.Info("Starting research gateway (read-only; run submission disabled)",
			"addr", addr, "results_dir", resultsDir, "console_origin", consoleOrigin,
			"token_configured", token != "", "version", v.Version, "git_sha", v.GitSHA)
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
