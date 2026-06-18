package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadFromYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.yaml")
	yaml := `
strategy:
  name: obi
  threshold: 0.85
  order_size: 0.5
kafka:
  brokers:
    - localhost:9092
  topics:
    - features.obi.v1
  group_id: huginn-test
capital:
  initial_cash: 50000
server:
  port: "9999"
`
	if err := os.WriteFile(path, []byte(yaml), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	if cfg.Strategy.Name != "obi" {
		t.Errorf("Strategy.Name = %q, want %q", cfg.Strategy.Name, "obi")
	}
	if cfg.Strategy.Threshold != 0.85 {
		t.Errorf("Strategy.Threshold = %v, want 0.85", cfg.Strategy.Threshold)
	}
	if cfg.Capital.InitialCash != 50000 {
		t.Errorf("Capital.InitialCash = %v, want 50000", cfg.Capital.InitialCash)
	}
	if cfg.Server.Port != "9999" {
		t.Errorf("Server.Port = %q, want %q", cfg.Server.Port, "9999")
	}
	if len(cfg.Kafka.Brokers) != 1 || cfg.Kafka.Brokers[0] != "localhost:9092" {
		t.Errorf("Kafka.Brokers = %v, want [localhost:9092]", cfg.Kafka.Brokers)
	}
}

func TestLoadDefaults(t *testing.T) {
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load(\"\") error: %v", err)
	}

	if cfg.Server.Port != "8081" {
		t.Errorf("default Server.Port = %q, want %q", cfg.Server.Port, "8081")
	}
	if cfg.Strategy.FastPeriod != 10 {
		t.Errorf("default FastPeriod = %d, want 10", cfg.Strategy.FastPeriod)
	}
	if cfg.Strategy.SlowPeriod != 30 {
		t.Errorf("default SlowPeriod = %d, want 30", cfg.Strategy.SlowPeriod)
	}
	if cfg.Feed.Source != "kafka" {
		t.Errorf("default Feed.Source = %q, want %q", cfg.Feed.Source, "kafka")
	}
	if cfg.Feed.StreamURL != "http://localhost:8080" {
		t.Errorf("default Feed.StreamURL = %q, want %q", cfg.Feed.StreamURL, "http://localhost:8080")
	}
	if cfg.Capital.InitialCash != 100_000 {
		t.Errorf("default InitialCash = %v, want 100000", cfg.Capital.InitialCash)
	}
	if cfg.Risk.MaxDrawdownPct != 0.20 {
		t.Errorf("default MaxDrawdownPct = %v, want 0.20", cfg.Risk.MaxDrawdownPct)
	}
	if cfg.Kafka.IntentsTopic != "executions.intents.v1" {
		t.Errorf("default IntentsTopic = %q, want %q", cfg.Kafka.IntentsTopic, "executions.intents.v1")
	}
	if cfg.Kafka.FillsTopic != "executions.fills.v1" {
		t.Errorf("default FillsTopic = %q, want %q", cfg.Kafka.FillsTopic, "executions.fills.v1")
	}
}

func TestLoadInvalidPath(t *testing.T) {
	_, err := Load("/nonexistent/path.yaml")
	if err == nil {
		t.Error("expected error for nonexistent path, got nil")
	}
}

func TestLoadInvalidYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.yaml")
	if err := os.WriteFile(path, []byte(":::not valid yaml"), 0644); err != nil {
		t.Fatal(err)
	}

	_, err := Load(path)
	if err == nil {
		t.Error("expected error for invalid YAML, got nil")
	}
}
