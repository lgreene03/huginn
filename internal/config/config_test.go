package config

import (
	"os"
	"path/filepath"
	"strings"
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

// validConfig returns a Config that passes Validate(), so each invalid-field
// test can mutate exactly one field and assert the failure is attributable to
// it (and only it).
func validConfig() *Config {
	c := &Config{}
	c.Strategy.Name = "obi"
	c.Strategy.Threshold = 0.85
	c.Strategy.OrderSize = 0.5
	c.Strategy.MLMinConfidence = 0.35
	c.Executor.TransactionCostBps = 1.0
	c.Executor.SlippageBps = 2.0
	c.Executor.SlippageImpactK = 0.0
	c.Executor.SlippageImpactScale = 1.0
	c.Executor.CostHurdleK = 0.0
	c.Capital.InitialCash = 100_000
	c.Feed.Source = "kafka"
	c.Kafka.Brokers = []string{"localhost:9092"}
	c.Kafka.Topics = []string{"features.obi.v1"}
	return c
}

func TestValidateValid(t *testing.T) {
	if err := validConfig().Validate(); err != nil {
		t.Fatalf("Validate() on a valid config returned error: %v", err)
	}
}

// TestValidateStreamFeedSkipsKafka confirms the SSE feed source is exempt from
// the broker/topic requirement.
func TestValidateStreamFeedSkipsKafka(t *testing.T) {
	c := validConfig()
	c.Feed.Source = "stream"
	c.Kafka.Brokers = nil
	c.Kafka.Topics = nil
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate() with stream feed and no kafka returned error: %v", err)
	}
}

func TestValidateInvalidFields(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*Config)
		wantSub string
	}{
		{"order_size zero", func(c *Config) { c.Strategy.OrderSize = 0 }, "order_size"},
		{"order_size negative", func(c *Config) { c.Strategy.OrderSize = -1 }, "order_size"},
		{"threshold negative", func(c *Config) { c.Strategy.Threshold = -0.1 }, "threshold"},
		{"threshold too large", func(c *Config) { c.Strategy.Threshold = 11 }, "threshold"},
		{"ml floor negative", func(c *Config) { c.Strategy.MLMinConfidence = -0.01 }, "ml_min_confidence"},
		{"ml floor above one", func(c *Config) { c.Strategy.MLMinConfidence = 1.5 }, "ml_min_confidence"},
		{"tx cost negative", func(c *Config) { c.Executor.TransactionCostBps = -1 }, "transaction_cost_bps"},
		{"slippage negative", func(c *Config) { c.Executor.SlippageBps = -1 }, "slippage_bps"},
		{"impact k negative", func(c *Config) { c.Executor.SlippageImpactK = -1 }, "slippage_impact_k"},
		{"impact scale negative", func(c *Config) { c.Executor.SlippageImpactScale = -1 }, "slippage_impact_scale"},
		{"cost hurdle k negative", func(c *Config) { c.Executor.CostHurdleK = -0.5 }, "cost_hurdle_k"},
		{"initial cash zero", func(c *Config) { c.Capital.InitialCash = 0 }, "initial_cash"},
		{"initial cash negative", func(c *Config) { c.Capital.InitialCash = -100 }, "initial_cash"},
		{"missing brokers", func(c *Config) { c.Kafka.Brokers = nil }, "kafka.brokers"},
		{"empty broker entry", func(c *Config) { c.Kafka.Brokers = []string{"  "} }, "kafka.brokers"},
		{"missing topics", func(c *Config) { c.Kafka.Topics = nil }, "kafka.topics"},
		{"empty topic entry", func(c *Config) { c.Kafka.Topics = []string{""} }, "kafka.topics"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := validConfig()
			tc.mutate(c)
			err := c.Validate()
			if err == nil {
				t.Fatalf("Validate() = nil, want error mentioning %q", tc.wantSub)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("Validate() error = %q, want it to mention %q", err.Error(), tc.wantSub)
			}
		})
	}
}

// TestValidateAggregatesMultiple confirms several problems are reported in a
// single aggregated error rather than just the first one.
func TestValidateAggregatesMultiple(t *testing.T) {
	c := validConfig()
	c.Strategy.OrderSize = 0
	c.Capital.InitialCash = 0
	c.Kafka.Brokers = nil

	err := c.Validate()
	if err == nil {
		t.Fatal("Validate() = nil, want aggregated error")
	}
	for _, sub := range []string{"order_size", "initial_cash", "kafka.brokers"} {
		if !strings.Contains(err.Error(), sub) {
			t.Errorf("aggregated error %q missing mention of %q", err.Error(), sub)
		}
	}
}
