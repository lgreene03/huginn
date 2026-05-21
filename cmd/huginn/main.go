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
	"github.com/lgreene03/huginn/internal/journal"
	"github.com/lgreene03/huginn/internal/kafka"
	"github.com/lgreene03/huginn/internal/metrics"
	"github.com/lgreene03/huginn/internal/portfolio"
	"github.com/lgreene03/huginn/internal/risk"
	"github.com/lgreene03/huginn/internal/server"
	"github.com/lgreene03/huginn/internal/strategy"
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

	slog.Info("Starting Huginn",
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
		activeStrategy = strategy.NewOBIThreshold(cfg.Strategy.Threshold, cfg.Strategy.OrderSize, cfg.Strategy.OrderSize*10)
	case "vpin":
		activeStrategy = strategy.NewVPINBreakout(cfg.Strategy.Threshold, cfg.Strategy.OrderSize, time.Minute)
	case "vwap_deviation":
		activeStrategy = strategy.NewVWAPDeviation(cfg.Strategy.Threshold, cfg.Strategy.OrderSize, cfg.Strategy.OrderSize*10)
	case "ema_crossover":
		activeStrategy = strategy.NewEMACrossover(cfg.Strategy.FastPeriod, cfg.Strategy.SlowPeriod, cfg.Strategy.OrderSize, cfg.Strategy.OrderSize*10)
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
	}

	// Initialize Kafka producer if live execution is enabled
	var producer *kafka.Producer
	if cfg.LiveExecution {
		slog.Info("Live execution enabled. Initializing Kafka Producer...", "brokers", cfg.Kafka.Brokers, "topic", cfg.Kafka.IntentsTopic)
		producer = kafka.NewProducer(cfg.Kafka.Brokers, cfg.Kafka.IntentsTopic)
	}

	// Initialize executor
	exec := executor.New(activeStrategy, port, jWriter, riskManager, executor.Config{
		TransactionCostBps: cfg.Executor.TransactionCostBps,
		SlippageBps:        cfg.Executor.SlippageBps,
		FillLatencyMs:      cfg.Executor.FillLatencyMs,
	}, cfg.LiveExecution, producer, strategyKey)

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

	// Initialize HTTP server
	srv := server.New(":"+cfg.Server.Port, port, riskManager, exec)
	go func() {
		if err := srv.Start(); err != nil {
			slog.Info("HTTP server stopped", "error", err)
		}
	}()

	// Graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		sig := <-sigCh
		slog.Info("Received shutdown signal", "signal", sig.String())
		cancel()
	}()

	// Run
	srv.SetReady(true)

	// Persist strategy state on a 5s ticker to bound RPO for strategies (like
	// EMACrossover) that mutate continuously between fills. Stops cleanly on
	// ctx cancel; the final tick fires on graceful shutdown.
	go exec.RunStatePersister(ctx, 5*time.Second)

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

	if err := consumer.Run(ctx); err != nil {
		slog.Error("Consumer error", "error", err)
	}
	srv.SetReady(false)

	// Print final summary
	exec.PrintSummary()
	_ = consumer.Close()
	if fillsConsumer != nil {
		_ = fillsConsumer.Close()
	}
	if producer != nil {
		_ = producer.Close()
	}
	_ = srv.Stop(context.Background())
	_ = jWriter.Close()

	slog.Info("Huginn shutdown complete")
}
