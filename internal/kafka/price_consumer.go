package kafka

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	kgo "github.com/segmentio/kafka-go"

	"github.com/lgreene03/huginn/internal/metrics"
	"github.com/lgreene03/huginn/internal/model"
)

// PriceTick is the wire format published by the bridge's WebSocket feed.
type PriceTick struct {
	Type       string  `json:"type"`
	Instrument string  `json:"instrument"`
	Price      float64 `json:"price"`
	Quantity   float64 `json:"quantity"`
	Timestamp  string  `json:"timestamp"`
}

// PriceHandler receives a lightweight FeatureEvent with only midPrice set,
// suitable for triggering exit checks without full signal evaluation.
type PriceHandler func(model.FeatureEvent)

// PriceConsumer reads real-time price ticks from the bridge's WebSocket feed
// and dispatches them as minimal FeatureEvents for sub-second exit monitoring.
type PriceConsumer struct {
	reader   *kgo.Reader
	handler  PriceHandler
	topic    string
	progress *Progress
}

func NewPriceConsumer(brokers []string, topic, groupID string, handler PriceHandler) *PriceConsumer {
	r := kgo.NewReader(kgo.ReaderConfig{
		Brokers:  brokers,
		GroupID:  groupID + "-prices",
		Topic:    topic,
		MinBytes: 1,
		MaxBytes: 10e6,
	})

	return &PriceConsumer{
		reader:   r,
		handler:  handler,
		topic:    topic,
		progress: NewProgress(),
	}
}

// Progress exposes the consumer's last-advance tracker for readiness probes.
func (c *PriceConsumer) Progress() *Progress { return c.progress }

func (c *PriceConsumer) Run(ctx context.Context) error {
	slog.Info("Price tick consumer started", "topic", c.topic)

	bo := newBackoff()
	for {
		msg, err := c.reader.FetchMessage(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			// Bounded jittered backoff replaces the fixed 1s sleep.
			slog.Error("Price consumer error", "error", err)
			bo.sleep(ctx)
			continue
		}
		bo.reset()
		c.progress.Mark()

		var tick PriceTick
		if err := json.Unmarshal(msg.Value, &tick); err != nil {
			metrics.DeserializeFailedTotal.WithLabelValues("price").Inc()
			slog.Warn("Failed to deserialize price tick", "error", err, "offset", msg.Offset)
			c.commit(ctx, msg)
			continue
		}

		if tick.Price <= 0 || tick.Instrument == "" {
			// Semantically empty tick (not a deserialize failure): skip the
			// handler but still commit so the offset advances.
			c.commit(ctx, msg)
			continue
		}

		event := model.FeatureEvent{
			Instrument: tick.Instrument,
			EventTime:  time.Now(),
			Values: map[string]float64{
				"midPrice": tick.Price,
			},
		}

		// Wrapped so a panic in the exit-monitoring handler cannot kill the
		// price consumer goroutine.
		safeDispatch("price", func() { c.handler(event) })

		c.commit(ctx, msg)
		c.progress.Mark()
	}
}

// commit acknowledges a processed price message. Price ticks are advisory
// (exit-monitoring only) and the handler is idempotent, so redelivery on a
// commit failure is harmless.
func (c *PriceConsumer) commit(ctx context.Context, msg kgo.Message) {
	if err := c.reader.CommitMessages(ctx, msg); err != nil {
		if ctx.Err() != nil {
			return
		}
		slog.Error("Failed to commit price offset", "offset", msg.Offset, "error", err)
	}
}

func (c *PriceConsumer) Close() error {
	slog.Info("Closing price tick consumer")
	return c.reader.Close()
}
