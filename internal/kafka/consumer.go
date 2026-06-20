package kafka

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"

	kgo "github.com/segmentio/kafka-go"

	"github.com/lgreene03/huginn/internal/metrics"
	"github.com/lgreene03/huginn/internal/model"
)

// Consumer reads from one or more Muninn feature topics and dispatches
// deserialized FeatureEvents to a handler function.
type Consumer struct {
	readers  []*kgo.Reader
	handler  func(model.FeatureEvent)
	progress *Progress
}

// Config holds the Kafka consumer configuration.
type Config struct {
	Brokers []string
	Topics  []string
	GroupID string
}

// NewConsumer creates a Kafka consumer for the specified Muninn feature topics.
func NewConsumer(cfg Config, handler func(model.FeatureEvent)) *Consumer {
	var readers []*kgo.Reader
	for _, topic := range cfg.Topics {
		reader := kgo.NewReader(kgo.ReaderConfig{
			Brokers:  cfg.Brokers,
			GroupID:  cfg.GroupID,
			Topic:    topic,
			MinBytes: 1,
			MaxBytes: 10e6,
		})
		readers = append(readers, reader)
	}

	return &Consumer{
		readers:  readers,
		handler:  handler,
		progress: NewProgress(),
	}
}

// Progress exposes the consumer's last-advance tracker so a readiness probe
// can detect a wedged loop. Never nil for a Consumer built via NewConsumer.
func (c *Consumer) Progress() *Progress { return c.progress }

// Run starts the consumption loop. It blocks until the context is cancelled.
func (c *Consumer) Run(ctx context.Context) error {
	slog.Info("Kafka multi-topic consumer started",
		"topics_count", len(c.readers),
	)

	var wg sync.WaitGroup

	// One goroutine per topic reader. Each fetches, dispatches synchronously,
	// then commits the offset ONLY after the handler has run — at-least-once
	// delivery (mirrors the sleipnir gateway pattern) so an in-flight message
	// is not lost if the process crashes mid-handler. The handler dispatch is
	// wrapped in safeDispatch so a panic logs + counts and the loop survives
	// instead of silently killing the consumer.
	for _, r := range c.readers {
		wg.Add(1)
		go func(reader *kgo.Reader) {
			defer wg.Done()
			bo := newBackoff()
			topic := reader.Config().Topic
			for {
				msg, err := reader.FetchMessage(ctx)
				if err != nil {
					if ctx.Err() != nil {
						return // Normal shutdown
					}
					// Bounded jittered backoff so a Redpanda outage does not
					// peg a core or flood logs with retry spam.
					slog.Error("Failed to read feature message", "topic", topic, "error", err)
					bo.sleep(ctx)
					continue
				}
				bo.reset()
				c.progress.Mark()

				var event model.FeatureEvent
				if err := json.Unmarshal(msg.Value, &event); err != nil {
					metrics.DeserializeFailedTotal.WithLabelValues("feature").Inc()
					slog.Warn("Failed to deserialize feature event",
						"topic", topic,
						"error", err,
						"offset", msg.Offset,
					)
					// Malformed frame: nothing to apply, but commit so we do
					// not redeliver the poison message forever.
					c.commit(ctx, reader, msg, topic)
					continue
				}

				metrics.FeaturesConsumedTotal.WithLabelValues(event.FeatureName).Inc()
				safeDispatch("feature", func() { c.handler(event) })

				// Commit AFTER the handler applied the event.
				c.commit(ctx, reader, msg, topic)
				c.progress.Mark()
			}
		}(r)
	}

	wg.Wait()
	slog.Info("Kafka consumer shutting down")
	return nil
}

// commit acknowledges a processed message. A commit failure is logged but not
// fatal: the offset stays uncommitted, so the message redelivers (handlers are
// idempotent — feature dispatch is stateless replay-safe, fills dedup by
// ExecutionID), which is the correct at-least-once behavior.
func (c *Consumer) commit(ctx context.Context, reader *kgo.Reader, msg kgo.Message, topic string) {
	if err := reader.CommitMessages(ctx, msg); err != nil {
		if ctx.Err() != nil {
			return // shutting down
		}
		slog.Error("Failed to commit feature offset", "topic", topic, "offset", msg.Offset, "error", err)
	}
}

// Close releases the consumer's resources.
func (c *Consumer) Close() error {
	var firstErr error
	for _, r := range c.readers {
		if err := r.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
