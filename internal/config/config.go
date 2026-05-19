package config

import (
	"os"

	"github.com/kelseyhightower/envconfig"
	"gopkg.in/yaml.v3"
)

type Config struct {
	Kafka    KafkaConfig    `yaml:"kafka"`
	Strategy StrategyConfig `yaml:"strategy"`
	Executor ExecutorConfig `yaml:"executor"`
	Server   ServerConfig   `yaml:"server"`
	Capital  CapitalConfig  `yaml:"capital"`
	Risk     RiskConfig     `yaml:"risk"`
	Database DatabaseConfig `yaml:"database"`
}

type KafkaConfig struct {
	Brokers []string `yaml:"brokers" envconfig:"KAFKA_BROKERS"`
	Topics  []string `yaml:"topics" envconfig:"KAFKA_TOPICS"`
	GroupID string   `yaml:"group_id" envconfig:"KAFKA_GROUP_ID"`
}

type StrategyConfig struct {
	Name      string  `yaml:"name" envconfig:"STRATEGY_NAME"`
	Threshold float64 `yaml:"threshold" envconfig:"STRATEGY_THRESHOLD"`
	OrderSize float64 `yaml:"order_size" envconfig:"STRATEGY_ORDER_SIZE"`
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
