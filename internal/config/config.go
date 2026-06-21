package config

import (
	"errors"
	"fmt"
	"os"
	"strings"
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
	// MLMinConfidence is the OBI strategy's ML-confidence floor (0 => use the
	// 0.35 default). Lower it to trade while the ML model is undertrained.
	MLMinConfidence float64 `yaml:"ml_min_confidence" envconfig:"STRATEGY_ML_MIN_CONFIDENCE"`
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

	// CostHurdleK is the safety multiple of round-trip trading cost an entry's
	// expected edge must clear before it is allowed through (quant-alpha-1, the
	// net-of-cost signal gate). DEFAULT 0.0 is INERT: no entry is ever
	// suppressed, so existing behaviour and all current tests are unchanged.
	// Only K > 0 activates the gate. K == 1 requires the edge to break even on
	// cost; K > 1 demands a margin. Swept as a calibrate grid key (cost_hurdle_k)
	// to find the K that maximises NET realized PnL on the existing OBI edge.
	CostHurdleK float64 `yaml:"cost_hurdle_k" envconfig:"COST_HURDLE_K"`

	// OBIEdgeBpsPerUnit is the linear coefficient mapping OBI signal strength
	// (|obi - effectiveThreshold|) to expected mean-reversion edge in basis
	// points, used by the cost hurdle's OBI edge model. Zero falls back to
	// strategy.DefaultEdgeBpsPerUnit. Only consulted when CostHurdleK > 0.
	OBIEdgeBpsPerUnit float64 `yaml:"obi_edge_bps_per_unit" envconfig:"OBI_EDGE_BPS_PER_UNIT"`

	// MakerFeeBps / TakerFeeBps are the per-fill fees (basis points) for maker
	// (passive, rests at the touch) and taker (aggressive, crosses the spread)
	// liquidity respectively (quant-alpha-2, the maker/taker fee lever). A
	// negative MakerFeeBps models a rebate. Both DEFAULT to 0, which the
	// executor resolves to TransactionCostBps — so unless you set these, every
	// fill costs exactly TransactionCostBps regardless of liquidity and all
	// existing backtest numbers are unchanged. Only meaningful once a strategy
	// emits maker-liquidity orders.
	MakerFeeBps float64 `yaml:"maker_fee_bps" envconfig:"EXECUTOR_MAKER_FEE_BPS"`
	TakerFeeBps float64 `yaml:"taker_fee_bps" envconfig:"EXECUTOR_TAKER_FEE_BPS"`
}

type ServerConfig struct {
	Port     string `yaml:"port" envconfig:"SERVER_PORT"`
	GRPCPort string `yaml:"grpc_port" envconfig:"GRPC_PORT"`
	// ReadyzStaleness is how long the feature consumer loop may go without
	// advancing before /readyz returns 503 (deep readiness). It does NOT
	// affect /healthz liveness. Zero (default) disables the check, so
	// behavior is unchanged unless explicitly configured. Default applied in
	// main.go is generous (5m) when the field is left at zero AND a feature
	// consumer is active — see cmd/huginn/main.go.
	ReadyzStaleness time.Duration `yaml:"readyz_staleness" envconfig:"SERVER_READYZ_STALENESS"`
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

// Validate performs fail-closed sanity checks on a fully-loaded Config and
// returns a single aggregated error describing every problem found (or nil when
// the config is sound). It is intentionally NOT called from Load — callers wire
// it in explicitly (cmd/huginn/main.go does so right after Load and exits 1 on
// error) so that offline tooling (backtest/calibrate/walkforward) can keep
// loading partially-specified configs without tripping the live-boot guards.
//
// The checks reject nonsensical configuration that would otherwise fail later
// in confusing ways or silently produce garbage results:
//   - order_size <= 0 (no position can ever be taken)
//   - threshold outside a sane [0, 10] band
//   - negative transaction-cost / slippage basis points
//   - negative slippage-impact coefficient or scale
//   - COST_HURDLE_K < 0 (the net-of-cost gate multiple)
//   - ML confidence floor outside [0, 1]
//   - initial cash <= 0
//   - missing Kafka brokers/topics unless the feed source is the SSE stream
//     (the only non-Kafka feature source the live engine supports)
func (c *Config) Validate() error {
	var errs []error

	if c.Strategy.OrderSize <= 0 {
		errs = append(errs, fmt.Errorf("strategy.order_size must be > 0, got %v", c.Strategy.OrderSize))
	}

	// Threshold is used as an absolute signal level by every strategy; a
	// negative value is always a mistake and an absurdly large one disables
	// trading entirely. 10 is a generous upper bound (OBI/VWAP/VPIN thresholds
	// live well below 1; EMA periods are configured separately).
	if c.Strategy.Threshold < 0 || c.Strategy.Threshold > 10 {
		errs = append(errs, fmt.Errorf("strategy.threshold must be within [0, 10], got %v", c.Strategy.Threshold))
	}

	if c.Strategy.MLMinConfidence < 0 || c.Strategy.MLMinConfidence > 1 {
		errs = append(errs, fmt.Errorf("strategy.ml_min_confidence must be within [0, 1], got %v", c.Strategy.MLMinConfidence))
	}

	if c.Executor.TransactionCostBps < 0 {
		errs = append(errs, fmt.Errorf("executor.transaction_cost_bps must be >= 0, got %v", c.Executor.TransactionCostBps))
	}
	if c.Executor.SlippageBps < 0 {
		errs = append(errs, fmt.Errorf("executor.slippage_bps must be >= 0, got %v", c.Executor.SlippageBps))
	}
	if c.Executor.SlippageImpactK < 0 {
		errs = append(errs, fmt.Errorf("executor.slippage_impact_k must be >= 0, got %v", c.Executor.SlippageImpactK))
	}
	if c.Executor.SlippageImpactScale < 0 {
		errs = append(errs, fmt.Errorf("executor.slippage_impact_scale must be >= 0, got %v", c.Executor.SlippageImpactScale))
	}
	if c.Executor.CostHurdleK < 0 {
		errs = append(errs, fmt.Errorf("executor.cost_hurdle_k (COST_HURDLE_K) must be >= 0, got %v", c.Executor.CostHurdleK))
	}

	if c.Capital.InitialCash <= 0 {
		errs = append(errs, fmt.Errorf("capital.initial_cash must be > 0, got %v", c.Capital.InitialCash))
	}

	// Kafka is the live feature transport. The SSE "stream" feed (ADR-0009) is
	// the only source that does not need brokers/topics, so only exempt that
	// path. Empty-string brokers/topics entries are treated as missing.
	if c.Feed.Source != "stream" {
		if len(nonEmpty(c.Kafka.Brokers)) == 0 {
			errs = append(errs, errors.New("kafka.brokers must be set (feed.source is not \"stream\")"))
		}
		if len(nonEmpty(c.Kafka.Topics)) == 0 {
			errs = append(errs, errors.New("kafka.topics must be set (feed.source is not \"stream\")"))
		}
	}

	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("invalid configuration: %w", errors.Join(errs...))
}

// nonEmpty returns the input slice with empty/whitespace-only entries removed.
func nonEmpty(in []string) []string {
	out := in[:0:0]
	for _, s := range in {
		if strings.TrimSpace(s) != "" {
			out = append(out, s)
		}
	}
	return out
}
