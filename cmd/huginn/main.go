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

	"github.com/lgreene/huginn/internal/config"
	"github.com/lgreene/huginn/internal/executor"
	"github.com/lgreene/huginn/internal/journal"
	"github.com/lgreene/huginn/internal/kafka"
	"github.com/lgreene/huginn/internal/metrics"
	"github.com/lgreene/huginn/internal/portfolio"
	"github.com/lgreene/huginn/internal/risk"
	"github.com/lgreene/huginn/internal/server"
	"github.com/lgreene/huginn/internal/strategy"
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

		jWriter, err = journal.NewPostgresWriter(cfg.Database.URL)
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

	// Initialize risk manager
	riskManager := risk.NewManager(cfg.Risk, cfg.Capital.InitialCash)

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
	}, cfg.LiveExecution, producer)

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
