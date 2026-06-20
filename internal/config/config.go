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
	Feed          FeedConfig     `yaml:"feed"`
	Strategy      StrategyConfig `yaml:"strategy"`
	Executor      ExecutorConfig `yaml:"executor"`
	Server        ServerConfig   `yaml:"server"`
	Capital       CapitalConfig  `yaml:"capital"`
	Risk          RiskConfig     `yaml:"risk"`
	Database      DatabaseConfig `yaml:"database"`
}

// FeedConfig selects where strategy-driving feature events come from. Both
// sources dispatch through the same handler (executor.OnFeature), so strategy,
// risk, and staleness behaviour are identical regardless of choice.
type FeedConfig struct {
	// Source is "kafka" (default) to consume Muninn's Redpanda topics, or
	// "stream" to tail Muninn's SSE feature stream (ADR-0009). The default
	// stays "kafka" until the stream path is proven in paper trading.
	Source string `yaml:"source" envconfig:"FEED_SOURCE"`
	// StreamURL is Muninn's base URL for the SSE source (e.g.
	// "http://localhost:8080"). Used only when Source == "stream".
	StreamURL string `yaml:"stream_url" envconfig:"FEED_STREAM_URL"`
	// StreamFeature optionally restricts the SSE stream to one feature name
	// (?feature=). Empty streams all features.
	StreamFeature string `yaml:"stream_feature" envconfig:"FEED_STREAM_FEATURE"`
}

type KafkaConfig struct {
	Brokers      []string `yaml:"brokers" envconfig:"KAFKA_BROKERS"`
	Topics       []string `yaml:"topics" envconfig:"KAFKA_TOPICS"`
	GroupID      string   `yaml:"group_id" envconfig:"KAFKA_GROUP_ID"`
	IntentsTopic string   `yaml:"intents_topic" envconfig:"KAFKA_INTENTS_TOPIC"`
	FillsTopic   string   `yaml:"fills_topic" envconfig:"KAFKA_FILLS_TOPIC"`
	PriceTopic   string   `yaml:"price_topic" envconfig:"KAFKA_PRICE_TOPIC"`
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

	// SlippageImpactK is the coefficient of an optional square-root market
	// impact term added on top of SlippageBps:
	//   effective_slip_bps = SlippageBps + SlippageImpactK*sqrt(qty/SlippageImpactScale)
	// Zero (default) keeps the original flat-constant slippage behaviour.
	SlippageImpactK float64 `yaml:"slippage_impact_k" envconfig:"EXECUTOR_SLIPPAGE_IMPACT_K"`
	// SlippageImpactScale normalises order quantity in the impact term (the
	// quantity at which the term contributes exactly SlippageImpactK bps).
	// Only used when SlippageImpactK > 0; non-positive falls back to 1.0.
	SlippageImpactScale float64 `yaml:"slippage_impact_scale" envconfig:"EXECUTOR_SLIPPAGE_IMPACT_SCALE"`

	// FillLatencyMs defers the fill timestamp by this many milliseconds in
	// paper-trading mode. Zero (default) uses the raw event timestamp.
	// Positive values model realistic signal-to-fill delays in backtests.
	FillLatencyMs int64 `yaml:"fill_latency_ms" envconfig:"EXECUTOR_FILL_LATENCY_MS"`

	// SizingMode selects the OPT-IN equity-aware position-sizing rule (quant-4):
	// "fixed" (default — keep the strategy's OrderSize), "kelly", or
	// "inverse_vol". Any other / empty value is treated as "fixed".
	SizingMode string `yaml:"sizing_mode" envconfig:"EXECUTOR_SIZING_MODE"`
	// SizingKellyFraction is the Kelly fraction of equity per order when
	// SizingMode == "kelly" (e.g. from strategy.KellyFraction offline). Zero
	// falls back to the strategy's OrderSize.
	SizingKellyFraction float64 `yaml:"sizing_kelly_fraction" envconfig:"EXECUTOR_SIZING_KELLY_FRACTION"`
	// SizingVolTarget is the per-position volatility budget for inverse-vol
	// sizing. Zero disables that mode (falls back to OrderSize).
	SizingVolTarget float64 `yaml:"sizing_vol_target" envconfig:"EXECUTOR_SIZING_VOL_TARGET"`
	// SizingMaxNotionalFraction caps any sized order at this fraction of equity.
	// Zero disables the cap.
	SizingMaxNotionalFraction float64 `yaml:"sizing_max_notional_fraction" envconfig:"EXECUTOR_SIZING_MAX_NOTIONAL_FRACTION"`
}

type ServerConfig struct {
	Port     string `yaml:"port" envconfig:"SERVER_PORT"`
	GRPCPort string `yaml:"grpc_port" envconfig:"GRPC_PORT"`
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

	// Connection-pool tunables. Zero values use pgxpool defaults.
	MaxConns        int32         `yaml:"max_conns" envconfig:"DATABASE_MAX_CONNS"`
	MinConns        int32         `yaml:"min_conns" envconfig:"DATABASE_MIN_CONNS"`
	MaxConnLifetime time.Duration `yaml:"max_conn_lifetime" envconfig:"DATABASE_MAX_CONN_LIFETIME"`
	MaxConnIdleTime time.Duration `yaml:"max_conn_idle_time" envconfig:"DATABASE_MAX_CONN_IDLE_TIME"`
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
	if cfg.Feed.Source == "" {
		cfg.Feed.Source = "kafka"
	}
	if cfg.Feed.StreamURL == "" {
		cfg.Feed.StreamURL = "http://localhost:8080"
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
