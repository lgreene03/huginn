package kafka

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	kgo "github.com/segmentio/kafka-go"

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
	reader  *kgo.Reader
	handler PriceHandler
	topic   string
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
		reader:  r,
		handler: handler,
		topic:   topic,
	}
}

func (c *PriceConsumer) Run(ctx context.Context) error {
	slog.Info("Price tick consumer started", "topic", c.topic)

	for {
		msg, err := c.reader.ReadMessage(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			slog.Error("Price consumer error", "error", err)
			time.Sleep(1 * time.Second)
			continue
		}

		var tick PriceTick
		if err := json.Unmarshal(msg.Value, &tick); err != nil {
			continue
		}

		if tick.Price <= 0 || tick.Instrument == "" {
			continue
		}

		event := model.FeatureEvent{
			Instrument: tick.Instrument,
			EventTime:  time.Now(),
			Values: map[string]float64{
				"midPrice": tick.Price,
			},
		}

		c.handler(event)
	}
}

func (c *PriceConsumer) Close() error {
	slog.Info("Closing price tick consumer")
	return c.reader.Close()
}
