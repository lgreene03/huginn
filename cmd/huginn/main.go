// Huginn — Quantitative Strategy Execution Engine
//
// Named after Odin's raven of "thought," Huginn is the downstream companion
// to Muninn ("memory"). It consumes deterministic features from Muninn's
// Redpanda topics and executes paper-trading strategies.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/lgreene03/huginn/internal/config"
	"github.com/lgreene03/huginn/internal/executor"
	"github.com/lgreene03/huginn/internal/feed"
	"github.com/lgreene03/huginn/internal/journal"
	"github.com/lgreene03/huginn/internal/kafka"
	"github.com/lgreene03/huginn/internal/metrics"
	"github.com/lgreene03/huginn/internal/portfolio"
	"github.com/lgreene03/huginn/internal/risk"
	"github.com/lgreene03/huginn/internal/server"
	"github.com/lgreene03/huginn/internal/strategy"
	"github.com/lgreene03/huginn/internal/tracing"
	"github.com/lgreene03/huginn/internal/version"
)

func main() {
	// CLI config flag
	configPath := flag.String("config", "configs/default.yaml", "Path to YAML config file")
	flag.Parse()

	// Structured logging
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	// Load configuration
	cfg, err := config.Load(*configPath)
	if err != nil {
		slog.Error("Failed to load configuration", "error", err)
		os.Exit(1)
	}

	// Fail closed: reject nonsensical configuration before wiring anything up.
	if err := cfg.Validate(); err != nil {
		slog.Error("Invalid configuration — refusing to boot", "error", err)
		os.Exit(1)
	}

	v := version.Get()
	slog.Info("Starting Huginn",
		"version", v.Version,
		"git_sha", v.GitSHA,
		"build_time", v.BuildTime,
		"config", *configPath,
		"broker", cfg.Kafka.Brokers[0], // Simplified logging for first broker
		"topic", cfg.Kafka.Topics[0],
		"strategy", cfg.Strategy.Name,
		"initial_cash", fmt.Sprintf("%.2f", cfg.Capital.InitialCash),
		"threshold", cfg.Strategy.Threshold,
		"order_size", cfg.Strategy.OrderSize,
	)

	// Recover portfolio and initialize journal writer based on DB configuration
	var port *portfolio.Portfolio
	var jWriter journal.Writer

	if cfg.Database.Enabled {
		slog.Info("Database persistence enabled. Connecting to PostgreSQL...")
		var err error
		port, err = journal.RecoverPortfolioFromPostgres(cfg.Database.URL, cfg.Capital.InitialCash)
		if err != nil {
			slog.Error("Failed to recover portfolio from PostgreSQL", "error", err)
			os.Exit(1)
		}

		jWriter, err = journal.NewPostgresWriter(cfg.Database.URL, journal.PoolConfig{
			MaxConns:        cfg.Database.MaxConns,
			MinConns:        cfg.Database.MinConns,
			MaxConnLifetime: cfg.Database.MaxConnLifetime,
			MaxConnIdleTime: cfg.Database.MaxConnIdleTime,
		})
		if err != nil {
			slog.Error("Failed to initialize PostgreSQL journal writer", "error", err)
			os.Exit(1)
		}
	} else {
		slog.Info("File persistence enabled. Using JSONL trade journal...")
		journalPath := "data/trades.jsonl"
		var err error
		port, err = journal.RecoverPortfolio(journalPath, cfg.Capital.InitialCash)
		if err != nil {
			slog.Error("Failed to recover portfolio from journal", "error", err)
			os.Exit(1)
		}

		jWriter, err = journal.NewJSONLWriter(journalPath)
		if err != nil {
			slog.Error("Failed to initialize journal writer", "error", err)
			os.Exit(1)
		}
	}
	// We handle explicit close during shutdown so we don't defer here, or we defer to catch panics.
	// We'll close explicitly at the end.

	// Set initial metrics gauges from recovered portfolio state
	snap := port.Snapshot()
	metrics.PortfolioCash.Set(snap.Cash)
	metrics.PortfolioRealizedPnL.Set(snap.RealizedPnL)
	metrics.PortfolioUnrealizedPnL.Set(snap.UnrealizedPnL)
	metrics.PortfolioTotalValue.Set(snap.TotalValue)

	// Select strategy
	var activeStrategy strategy.Strategy
	switch cfg.Strategy.Name {
	case "obi":
		obiParams := strategy.DefaultOBIParams()
		if cfg.Strategy.MLMinConfidence > 0 {
			obiParams.MLMinConfidence = cfg.Strategy.MLMinConfidence
		}
		obiParams.MakerEntries = cfg.Strategy.OBIMaker
		activeStrategy = strategy.NewOBIThresholdWithParams(cfg.Strategy.Threshold, cfg.Strategy.OrderSize, cfg.Strategy.OrderSize*10, obiParams)
	case "vpin":
		activeStrategy = strategy.NewVPINBreakout(cfg.Strategy.Threshold, cfg.Strategy.OrderSize, time.Minute)
	case "vwap_deviation":
		activeStrategy = strategy.NewVWAPDeviation(cfg.Strategy.Threshold, cfg.Strategy.OrderSize, cfg.Strategy.OrderSize*10)
	case "ema_crossover":
		activeStrategy = strategy.NewEMACrossover(cfg.Strategy.FastPeriod, cfg.Strategy.SlowPeriod, cfg.Strategy.OrderSize, cfg.Strategy.OrderSize*10)
	case "ou":
		// OU mean-reversion: SlowPeriod is the rolling OLS window; Threshold is
		// the |z| entry band (defaults inside NewOUReversion if unset). Mirrors
		// cmd/backtest so live and backtest construct the strategy identically.
		// OUReversion implements strategy.Stateful, so the generic strategy-state
		// restore path below (keyed on cfg.Strategy.Name) recovers its rolling
		// window / open position on restart with no extra wiring.
		activeStrategy = strategy.NewOUReversion(cfg.Strategy.SlowPeriod, cfg.Strategy.Threshold, cfg.Strategy.OrderSize, cfg.Strategy.OrderSize*10)
	case "composite":
		// Pluggable-alpha composite: blends a default weighted alpha set (OBI +
		// multi-timeframe momentum + EMA mean-reversion) into one signed signal,
		// reusing the same cost-hurdle + signed-position + risk path as OBI.
		// Threshold is the |combined score| entry band (defaults to 0.5 if unset).
		// CompositeStrategy implements strategy.Stateful, so the generic
		// strategy-state restore path below recovers its netPosition / open
		// positions on restart with no extra wiring.
		activeStrategy = strategy.NewCompositeStrategy(strategy.DefaultCompositeConfig(cfg.Strategy.Threshold, cfg.Strategy.OrderSize))
	default:
		slog.Error("Unknown strategy", "strategy", cfg.Strategy.Name)
		os.Exit(1)
	}

	// Restore strategy state from prior run, if any. The key is the config
	// strategy name (stable across threshold tweaks). Loader backend matches
	// the persistence backend selected above.
	strategyKey := cfg.Strategy.Name
	var priorState []byte
	if cfg.Database.Enabled {
		if pw, ok := jWriter.(*journal.PostgresWriter); ok {
			priorState, err = pw.LoadStrategyState(strategyKey)
		}
	} else {
		priorState, err = journal.LoadStrategyStateFromJSONL("data/trades.jsonl", strategyKey)
	}
	if err != nil {
		slog.Error("Failed to load prior strategy state", "error", err, "strategy_key", strategyKey)
		os.Exit(1)
	}
	if sf, ok := activeStrategy.(strategy.Stateful); ok && len(priorState) > 0 {
		if err := sf.RestoreState(priorState); err != nil {
			slog.Error("Failed to restore strategy state — refusing to boot (delete the strategy_state row to start fresh)",
				"error", err, "strategy_key", strategyKey)
			os.Exit(1)
		}
		slog.Info("Restored strategy state from prior run", "strategy_key", strategyKey, "bytes", len(priorState))
	} else if len(priorState) == 0 {
		slog.Info("No prior strategy state found — starting fresh", "strategy_key", strategyKey)
	}

	// Initialize risk manager
	riskManager := risk.NewManager(cfg.Risk, cfg.Capital.InitialCash)

	// Restore risk state (peakValue, dayStart baseline, last feature event)
	// from the same backend the strategy state came from. Without this, every
	// restart resets peakValue to initial cash and replays the daily-loss
	// window from zero — both misfire on the first post-restart trade.
	var priorRisk []byte
	if cfg.Database.Enabled {
		if pw, ok := jWriter.(*journal.PostgresWriter); ok {
			priorRisk, err = pw.LoadStrategyState(executor.RiskStateKey())
		}
	} else {
		priorRisk, err = journal.LoadStrategyStateFromJSONL("data/trades.jsonl", executor.RiskStateKey())
	}
	if err != nil {
		slog.Error("Failed to load prior risk state", "error", err)
		os.Exit(1)
	}
	if len(priorRisk) > 0 {
		if err := riskManager.RestoreState(priorRisk); err != nil {
			slog.Error("Failed to restore risk state — refusing to boot", "error", err)
			os.Exit(1)
		}
		slog.Info("Restored risk state from prior run", "bytes", len(priorRisk))
	} else {
		slog.Info("No prior risk state found — starting with fresh peakValue + daily-loss baseline")
		// Fallback: seed peak and daily baseline from daily_pnl_snapshots when the
		// _risk blob is absent (e.g. first boot, corrupt state, or migrated instance).
		if pw, ok := jWriter.(*journal.PostgresWriter); ok {
			if baseline, found, bErr := pw.LoadLatestDailyBaseline(); bErr != nil {
				slog.Warn("Failed to load daily PnL baseline (non-fatal)", "error", bErr)
			} else if found {
				riskManager.SeedFromBaseline(baseline.PeakValue, baseline.DayStartRealizedPnL)
			}
		}
	}

	// Initialize Kafka producer if live execution is enabled
	var producer *kafka.Producer
	if cfg.LiveExecution {
		slog.Info("Live execution enabled. Initializing Kafka Producer...", "brokers", cfg.Kafka.Brokers, "topic", cfg.Kafka.IntentsTopic)
		producer = kafka.NewProducer(cfg.Kafka.Brokers, cfg.Kafka.IntentsTopic)
	}

	// Resolve the opt-in sizing mode (default fixed = keep strategy OrderSize).
	sizingMode, ok := strategy.ParseSizingMode(cfg.Executor.SizingMode)
	if !ok {
		slog.Warn("Unknown executor.sizing_mode; falling back to fixed OrderSize", "value", cfg.Executor.SizingMode)
	}

	// Initialize executor
	exec := executor.New(activeStrategy, port, jWriter, riskManager, executor.Config{
		TransactionCostBps:        cfg.Executor.TransactionCostBps,
		SlippageBps:               cfg.Executor.SlippageBps,
		SlippageImpactK:           cfg.Executor.SlippageImpactK,
		SlippageImpactScale:       cfg.Executor.SlippageImpactScale,
		FillLatencyMs:             cfg.Executor.FillLatencyMs,
		Sizing:                    sizingMode,
		SizingKellyFraction:       cfg.Executor.SizingKellyFraction,
		SizingVolTarget:           cfg.Executor.SizingVolTarget,
		SizingMaxNotionalFraction: cfg.Executor.SizingMaxNotionalFraction,
		MakerFeeBps:               cfg.Executor.MakerFeeBps,
		TakerFeeBps:               cfg.Executor.TakerFeeBps,
	}, cfg.LiveExecution, producer, strategyKey)

	// Net-of-cost signal gate (quant-alpha-1). Inert by default: with
	// COST_HURDLE_K == 0 the hurdle never suppresses, so behaviour is unchanged.
	// Only attach it to the OBI strategy (the strategy with a real edge model);
	// other strategies keep their existing behaviour. The cost primitives mirror
	// the executor's fill-cost model so the gate's estimate matches actual cost.
	if obi, ok := activeStrategy.(*strategy.OBIThreshold); ok && cfg.Executor.CostHurdleK > 0 {
		obi.SetCostHurdle(&strategy.CostHurdle{
			K:                   cfg.Executor.CostHurdleK,
			TransactionCostBps:  cfg.Executor.TransactionCostBps,
			SlippageBps:         cfg.Executor.SlippageBps,
			SlippageImpactK:     cfg.Executor.SlippageImpactK,
			SlippageImpactScale: cfg.Executor.SlippageImpactScale,
			Edge:                strategy.OBIEdgeModel{BpsPerUnit: cfg.Executor.OBIEdgeBpsPerUnit},
		})
		slog.Info("Net-of-cost signal gate enabled",
			"cost_hurdle_k", cfg.Executor.CostHurdleK,
			"obi_edge_bps_per_unit", cfg.Executor.OBIEdgeBpsPerUnit,
		)
	}

	// Initialize Kafka consumer for incoming feature events
	consumer := kafka.NewConsumer(kafka.Config{
		Brokers: cfg.Kafka.Brokers,
		Topics:  cfg.Kafka.Topics,
		GroupID: cfg.Kafka.GroupID,
	}, exec.OnFeature)

	// Initialize Kafka fills consumer if live execution is enabled
	var fillsConsumer *kafka.FillsConsumer
	if cfg.LiveExecution {
		slog.Info("Live execution enabled. Initializing Kafka Fills Consumer...", "brokers", cfg.Kafka.Brokers, "topic", cfg.Kafka.FillsTopic, "group_id", cfg.Kafka.GroupID)
		fillsConsumer = kafka.NewFillsConsumer(cfg.Kafka.Brokers, cfg.Kafka.FillsTopic, cfg.Kafka.GroupID, exec.OnExecutionFill)
	}

	// Initialize real-time price tick consumer for sub-second exit monitoring
	var priceConsumer *kafka.PriceConsumer
	if cfg.Kafka.PriceTopic != "" {
		slog.Info("Real-time price feed enabled", "topic", cfg.Kafka.PriceTopic)
		priceConsumer = kafka.NewPriceConsumer(cfg.Kafka.Brokers, cfg.Kafka.PriceTopic, cfg.Kafka.GroupID, exec.OnFeature)
	}

	// Initialize HTTP server
	srv := server.New(":"+cfg.Server.Port, port, riskManager, exec)

	// Deep /readyz: when consuming from Kafka, report 503 if the feature
	// consumer loop has not advanced within the staleness window. A generous
	// default (5m) applies when SERVER_READYZ_STALENESS is unset, so a normal
	// quiet market never trips it; set the env var to tighten. /healthz stays
	// liveness-only and is unaffected. The SSE "stream" feed has no kafka
	// Progress, so the deep check is only wired for the Kafka path.
	if cfg.Feed.Source != "stream" {
		staleness := cfg.Server.ReadyzStaleness
		if staleness <= 0 {
			staleness = 5 * time.Minute
		}
		prog := consumer.Progress()
		srv.ReadinessProbe(func() error {
			if prog.Stale(staleness) {
				return fmt.Errorf("feature consumer stale: no progress in %s", staleness)
			}
			return nil
		})
	}

	go func() {
		if err := srv.Start(); err != nil {
			slog.Info("HTTP server stopped", "error", err)
		}
	}()

	// Initialize gRPC server for programmatic access
	var grpcSrv *server.GRPCServer
	if cfg.Server.GRPCPort != "" {
		grpcSrv = server.NewGRPCServer(cfg.Server.GRPCPort, func() map[string]interface{} {
			snap := port.Snapshot()
			return map[string]interface{}{
				"portfolio": snap,
				"strategy":  activeStrategy.Name(),
				"halted":    riskManager.IsHalted(),
			}
		})
		if err := grpcSrv.Start(); err != nil {
			slog.Error("gRPC server failed to start", "error", err)
		}
	}

	// Graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		sig := <-sigCh
		slog.Info("Received shutdown signal", "signal", sig.String())
		cancel()
	}()

	// OpenTelemetry tracing: if OTEL_EXPORTER_OTLP_ENDPOINT is set, spans
	// are exported via OTLP/gRPC. Unset → no-op (still propagates W3C
	// TraceContext in-process, so kafka headers carry the trace id even
	// when nothing's recording locally).
	tracingShutdown, err := tracing.Init(ctx, "dev")
	if err != nil {
		slog.Error("Failed to initialize OTel tracing", "error", err)
		os.Exit(1)
	}
	defer func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		if err := tracingShutdown(shutdownCtx); err != nil {
			slog.Error("OTel tracing shutdown error", "error", err)
		}
	}()

	// Run
	srv.SetReady(true)

	// Persist strategy state on a 5s ticker to bound RPO for strategies (like
	// EMACrossover) that mutate continuously between fills. Stops cleanly on
	// ctx cancel; the final tick fires on graceful shutdown.
	go exec.RunStatePersister(ctx, 5*time.Second)

	// Sample equity into the server-side ring buffer every 30 s so the
	// /api/snapshot/history endpoint can hydrate the UI on page load.
	go srv.RunEquitySampler(ctx, 30*time.Second)

	// Feature-staleness watchdog: auto-halts if no event arrives within
	// cfg.Risk.StalenessTimeout. Zero timeout (default) disables it; configure
	// in YAML when you want it.
	go riskManager.RunStalenessWatchdog(ctx)

	if fillsConsumer != nil {
		go func() {
			if err := fillsConsumer.Run(ctx); err != nil {
				slog.Error("Fills consumer error", "error", err)
			}
		}()
	}

	if priceConsumer != nil {
		go func() {
			if err := priceConsumer.Run(ctx); err != nil {
				slog.Error("Price consumer error", "error", err)
			}
		}()
	}

	// Select the feature-event source. "kafka" (default) consumes Muninn's
	// Redpanda topics; "stream" tails Muninn's SSE feature stream (ADR-0009).
	// Both dispatch through exec.OnFeature, so the staleness watchdog and all
	// strategy/risk wiring are identical regardless of source. Each blocks
	// until ctx is cancelled.
	switch cfg.Feed.Source {
	case "stream":
		slog.Info("Feature source: Muninn SSE stream",
			"url", cfg.Feed.StreamURL, "feature", cfg.Feed.StreamFeature)
		src := feed.NewSSESource(feed.SSEConfig{
			BaseURL: cfg.Feed.StreamURL,
			Feature: cfg.Feed.StreamFeature,
		}, exec.OnFeature)
		if err := src.Run(ctx); err != nil {
			slog.Error("SSE feature source error", "error", err)
		}
	default:
		slog.Info("Feature source: Kafka topics", "topics", cfg.Kafka.Topics)
		if err := consumer.Run(ctx); err != nil {
			slog.Error("Consumer error", "error", err)
		}
	}
	srv.SetReady(false)

	// Print final summary
	exec.PrintSummary()
	_ = consumer.Close()
	if fillsConsumer != nil {
		_ = fillsConsumer.Close()
	}
	if priceConsumer != nil {
		_ = priceConsumer.Close()
	}
	if producer != nil {
		_ = producer.Close()
	}
	if grpcSrv != nil {
		grpcSrv.Stop()
	}
	_ = srv.Stop(context.Background())
	_ = jWriter.Close()

	slog.Info("Huginn shutdown complete")
}
