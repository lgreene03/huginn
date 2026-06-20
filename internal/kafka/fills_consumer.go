package kafka

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	kgo "github.com/segmentio/kafka-go"
	"go.opentelemetry.io/otel/attribute"

	"github.com/lgreene03/huginn/internal/metrics"
	"github.com/lgreene03/huginn/internal/model"
	"github.com/lgreene03/huginn/internal/tracing"
)

// GatewayFill represents the verified trade execution layout published by Sleipnir.
// See sleipnir/docs/CONTRACTS.md for the wire spec. ExecutionID is the per-fill
// idempotency key — the executor drops duplicates by this field.
type GatewayFill struct {
	OrderID         string    `json:"order_id"`
	ExecutionID     string    `json:"execution_id"`
	Instrument      string    `json:"instrument"`
	Side            string    `json:"side"` // "BUY" or "SELL"
	Quantity        float64   `json:"quantity"`
	FillPrice       float64   `json:"fill_price"`
	TransactionCost float64   `json:"transaction_cost"`
	Timestamp       time.Time `json:"timestamp"`
}

// FillHandler is the per-fill callback type. Phase 7 extended the signature
// with a context.Context so the trace started by sleipnir on the producer
// side can propagate into huginn's executor (dedup → journal → portfolio).
type FillHandler func(context.Context, model.Fill)

// FillsConsumer reads execution fills from Sleipnir and dispatches them to a handler.
type FillsConsumer struct {
	reader   *kgo.Reader
	handler  FillHandler
	topic    string
	progress *Progress
}

// NewFillsConsumer creates a consumer for Sleipnir fills.
func NewFillsConsumer(brokers []string, topic, groupID string, handler FillHandler) *FillsConsumer {
	r := kgo.NewReader(kgo.ReaderConfig{
		Brokers:  brokers,
		GroupID:  groupID,
		Topic:    topic,
		MinBytes: 1,
		MaxBytes: 10e6,
	})

	return &FillsConsumer{
		reader:   r,
		handler:  handler,
		topic:    topic,
		progress: NewProgress(),
	}
}

// Progress exposes the consumer's last-advance tracker for readiness probes.
func (c *FillsConsumer) Progress() *Progress { return c.progress }

// Run starts the fills consumption loop. It blocks until the context is cancelled.
//
// Each message's W3C TraceContext headers are extracted into a per-message
// context so the handler's span attaches to the trace sleipnir started.
func (c *FillsConsumer) Run(ctx context.Context) error {
	slog.Info("Fills consumer started", "topic", c.topic)

	bo := newBackoff()
	for {
		msg, err := c.reader.FetchMessage(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil // Graceful shutdown
			}
			// Bounded jittered backoff replaces the fixed 1s sleep so a
			// Redpanda outage neither pegs a core nor floods logs.
			slog.Error("Fills consumer error reading message", "error", err)
			bo.sleep(ctx)
			continue
		}
		bo.reset()
		c.progress.Mark()

		var gatewayFill GatewayFill
		if err := json.Unmarshal(msg.Value, &gatewayFill); err != nil {
			metrics.DeserializeFailedTotal.WithLabelValues("fills").Inc()
			slog.Warn("Failed to deserialize execution fill event",
				"error", err,
				"offset", msg.Offset,
			)
			// Poison frame: commit so it does not redeliver forever.
			c.commit(ctx, msg)
			continue
		}

		slog.Info("Ingested verified execution fill event",
			"orderID", gatewayFill.OrderID,
			"instrument", gatewayFill.Instrument,
			"side", gatewayFill.Side,
			"qty", gatewayFill.Quantity,
			"price", gatewayFill.FillPrice,
		)

		fillSide := model.Buy
		if gatewayFill.Side == "SELL" {
			fillSide = model.Sell
		}

		fill := model.Fill{
			OrderID:         gatewayFill.OrderID,
			ExecutionID:     gatewayFill.ExecutionID,
			Instrument:      gatewayFill.Instrument,
			Side:            fillSide,
			Quantity:        gatewayFill.Quantity,
			FillPrice:       gatewayFill.FillPrice,
			TransactionCost: gatewayFill.TransactionCost,
			SlippageBps:     0.0, // Exchange-realized slippage
			Timestamp:       gatewayFill.Timestamp,
		}

		// Resume sleipnir's trace so OnExecutionFill's work links back to
		// the original intent's span tree.
		fillCtx := tracing.ExtractKafkaContext(ctx, msg.Headers)
		fillCtx, span := tracing.StartSpan(fillCtx, "kafka.fill_received",
			attribute.String("order_id", fill.OrderID),
			attribute.String("execution_id", fill.ExecutionID),
			attribute.String("instrument", fill.Instrument),
		)

		// Dispatch back into execution engine. Wrapped so a panic in the
		// executor (dedup → journal → portfolio) cannot kill the consumer
		// goroutine and silently halt fill ingestion.
		safeDispatch("fills", func() { c.handler(fillCtx, fill) })
		span.End()

		// Commit AFTER OnExecutionFill has journaled/applied the fill, so a
		// crash mid-handler redelivers the fill (dedup by ExecutionID makes
		// the replay safe) instead of losing it.
		c.commit(ctx, msg)
		c.progress.Mark()
	}
}

// commit acknowledges a processed fill message. Failures are logged, not
// fatal: the offset stays uncommitted and the fill redelivers (idempotent via
// ExecutionID dedup).
func (c *FillsConsumer) commit(ctx context.Context, msg kgo.Message) {
	if err := c.reader.CommitMessages(ctx, msg); err != nil {
		if ctx.Err() != nil {
			return
		}
		slog.Error("Failed to commit fill offset", "offset", msg.Offset, "error", err)
	}
}

// Close gracefully closes the fills consumer connection.
func (c *FillsConsumer) Close() error {
	slog.Info("Closing Kafka fills consumer connection")
	return c.reader.Close()
}
