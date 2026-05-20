package config

import (
	"os"
	"time"

	"github.com/kelseyhightower/envconfig"
	"gopkg.in/yaml.v3"
)

type Config struct {
	LiveExecution bool           `yaml:"live_execution" envconfig:"LIVE_EXECUTION"`
	Kafka         KafkaConfig    `yaml:"kafka"`
	Strategy      StrategyConfig `yaml:"strategy"`
	Executor      ExecutorConfig `yaml:"executor"`
	Server        ServerConfig   `yaml:"server"`
	Capital       CapitalConfig  `yaml:"capital"`
	Risk          RiskConfig     `yaml:"risk"`
	Database      DatabaseConfig `yaml:"database"`
}

type KafkaConfig struct {
	Brokers      []string `yaml:"brokers" envconfig:"KAFKA_BROKERS"`
	Topics       []string `yaml:"topics" envconfig:"KAFKA_TOPICS"`
	GroupID      string   `yaml:"group_id" envconfig:"KAFKA_GROUP_ID"`
	IntentsTopic string   `yaml:"intents_topic" envconfig:"KAFKA_INTENTS_TOPIC"`
	FillsTopic   string   `yaml:"fills_topic" envconfig:"KAFKA_FILLS_TOPIC"`
}

type StrategyConfig struct {
	Name       string  `yaml:"name" envconfig:"STRATEGY_NAME"`
	Threshold  float64 `yaml:"threshold" envconfig:"STRATEGY_THRESHOLD"`
	OrderSize  float64 `yaml:"order_size" envconfig:"STRATEGY_ORDER_SIZE"`
	FastPeriod int     `yaml:"fast_period" envconfig:"STRATEGY_FAST_PERIOD"`
	SlowPeriod int     `yaml:"slow_period" envconfig:"STRATEGY_SLOW_PERIOD"`
}

type ExecutorConfig struct {
	TransactionCostBps float64 `yaml:"transaction_cost_bps" envconfig:"EXECUTOR_TX_COST_BPS"`
	SlippageBps        float64 `yaml:"slippage_bps" envconfig:"EXECUTOR_SLIPPAGE_BPS"`
}

type ServerConfig struct {
	Port string `yaml:"port" envconfig:"SERVER_PORT"`
}

type CapitalConfig struct {
	InitialCash float64 `yaml:"initial_cash" envconfig:"CAPITAL_INITIAL_CASH"`
}

type RiskConfig struct {
	MaxDrawdownPct    float64 `yaml:"max_drawdown_pct" envconfig:"RISK_MAX_DRAWDOWN_PCT"`
	DailyLossLimit    float64 `yaml:"daily_loss_limit" envconfig:"RISK_DAILY_LOSS_LIMIT"`
	PositionLimitHard float64 `yaml:"position_limit_hard" envconfig:"RISK_POSITION_LIMIT_HARD"`

	// PositionLimitPerInstrument is an optional override of PositionLimitHard
	// on a per-instrument basis. When the fill's instrument appears in the
	// map, the gross/vol-scaled cap is replaced with the literal value here.
	// Instruments not in the map continue to use the (vol-scaled) gross limit.
	PositionLimitPerInstrument map[string]float64 `yaml:"position_limit_per_instrument"`

	// StalenessTimeout is the maximum gap between consecutive feature events
	// before the risk manager auto-halts trading. Zero disables the watchdog.
	StalenessTimeout time.Duration `yaml:"staleness_timeout" envconfig:"RISK_STALENESS_TIMEOUT"`

	// AutoResumeAfterStaleness controls whether arrival of a fresh feature
	// event automatically clears a staleness-induced halt. Manual halts
	// (Halt()/circuit-breaker via HTTP) are never auto-cleared.
	AutoResumeAfterStaleness bool `yaml:"auto_resume_after_staleness" envconfig:"RISK_AUTO_RESUME_AFTER_STALENESS"`
}

type DatabaseConfig struct {
	Enabled bool   `yaml:"enabled" envconfig:"DATABASE_ENABLED"`
	URL     string `yaml:"url" envconfig:"DATABASE_URL"`
}

func Load(path string) (*Config, error) {
	cfg := &Config{}

	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}

		if err := yaml.Unmarshal(data, cfg); err != nil {
			return nil, err
		}
	}

	// Override with environment variables
	if err := envconfig.Process("", cfg); err != nil {
		return nil, err
	}

	// Set defaults if not provided
	if cfg.Server.Port == "" {
		cfg.Server.Port = "8081"
	}
	if cfg.Strategy.FastPeriod == 0 {
		cfg.Strategy.FastPeriod = 10
	}
	if cfg.Strategy.SlowPeriod == 0 {
		cfg.Strategy.SlowPeriod = 30
	}
	if cfg.Kafka.IntentsTopic == "" {
		cfg.Kafka.IntentsTopic = "executions.intents.v1"
	}
	if cfg.Kafka.FillsTopic == "" {
		cfg.Kafka.FillsTopic = "executions.fills.v1"
	}
	if cfg.Capital.InitialCash == 0 {
		cfg.Capital.InitialCash = 100_000.0
	}
	if cfg.Risk.MaxDrawdownPct == 0 {
		cfg.Risk.MaxDrawdownPct = 0.20
	}
	if cfg.Risk.DailyLossLimit == 0 {
		cfg.Risk.DailyLossLimit = 10000.0
	}
	if cfg.Risk.PositionLimitHard == 0 {
		cfg.Risk.PositionLimitHard = 500000.0
	}
	if cfg.Database.URL == "" {
		cfg.Database.URL = "postgres://postgres:postgres@localhost:5432/huginn?sslmode=disable"
	}

	return cfg, nil
}
