package kafka

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"

	"github.com/lgreene/huginn/internal/metrics"
	"github.com/lgreene/huginn/internal/model"
	kgo "github.com/segmentio/kafka-go"
)

// Consumer reads from one or more Muninn feature topics and dispatches
// deserialized FeatureEvents to a handler function.
type Consumer struct {
	readers []*kgo.Reader
	handler func(model.FeatureEvent)
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
		readers: readers,
		handler: handler,
	}
}

// Run starts the consumption loop. It blocks until the context is cancelled.
func (c *Consumer) Run(ctx context.Context) error {
	slog.Info("Kafka multi-topic consumer started",
		"topics_count", len(c.readers),
	)

	var wg sync.WaitGroup
	eventCh := make(chan model.FeatureEvent, 1000)

	// Start a goroutine for each topic reader
	for _, r := range c.readers {
		wg.Add(1)
		go func(reader *kgo.Reader) {
			defer wg.Done()
			for {
				msg, err := reader.ReadMessage(ctx)
				if err != nil {
					if ctx.Err() != nil {
						return // Normal shutdown
					}
					slog.Error("Failed to read message", "topic", reader.Config().Topic, "error", err)
					continue
				}

				var event model.FeatureEvent
				if err := json.Unmarshal(msg.Value, &event); err != nil {
					slog.Warn("Failed to deserialize feature event",
						"topic", reader.Config().Topic,
						"error", err,
						"offset", msg.Offset,
					)
					continue
				}

				select {
				case eventCh <- event:
				case <-ctx.Done():
					return
				}
			}
		}(r)
	}

	// Dispatcher goroutine
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case event := <-eventCh:
				metrics.FeaturesConsumedTotal.WithLabelValues(event.FeatureName).Inc()
				c.handler(event)
			case <-ctx.Done():
				return
			}
		}
	}()

	wg.Wait()
	slog.Info("Kafka consumer shutting down")
	return nil
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
