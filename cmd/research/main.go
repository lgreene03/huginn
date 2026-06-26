// Command research is the Huginn research / validation gateway: an HTTP service
// that runs heavy walk-forward + PBO + Deflated-Sharpe backtests OUT of the
// live trading process. Operators submit a run (strategy + threshold grid +
// fold count), the gateway executes internal/research.Run asynchronously, and
// the read-only research UI polls for the derived results
// (oosFoldsProfitable / totalOOSPnL / pbo / deflatedSharpe).
//
// It owns no Kafka/Postgres dependency: it replays a JSONL dataset on disk, so
// it can run as a standalone sidecar. Finished runs are persisted to
// RESEARCH_RESULTS_DIR and reloaded on startup so results survive a restart.
package main

import (
	"context"
	"log/slog"
	"math/rand"
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
	dataPath := os.Getenv("RESEARCH_DATA_PATH")
	if dataPath == "" {
		dataPath = defaultDataPath
	}

	addr := ":" + port
	s := NewServer(addr, resultsDir, dataPath)

	srv := &http.Server{
		Addr:              addr,
		Handler:           s.routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		v := version.Get()
		slog.Info("Starting research gateway",
			"addr", addr, "results_dir", resultsDir, "data", dataPath,
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

// randSeq returns a small random integer to disambiguate run ids minted within
// the same nanosecond.
func randSeq() int {
	//nolint:gosec // G404: non-cryptographic run-id disambiguator, math/rand is intentional.
	return rand.Intn(10000)
}
