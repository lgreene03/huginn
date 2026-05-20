package kafka

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/lgreene/huginn/internal/model"
	kgo "github.com/segmentio/kafka-go"
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

// FillsConsumer reads execution fills from Sleipnir and dispatches them to a handler.
type FillsConsumer struct {
	reader  *kgo.Reader
	handler func(model.Fill)
	topic   string
}

// NewFillsConsumer creates a consumer for Sleipnir fills.
func NewFillsConsumer(brokers []string, topic, groupID string, handler func(model.Fill)) *FillsConsumer {
	r := kgo.NewReader(kgo.ReaderConfig{
		Brokers:  brokers,
		GroupID:  groupID,
		Topic:    topic,
		MinBytes: 1,
		MaxBytes: 10e6,
	})

	return &FillsConsumer{
		reader:  r,
		handler: handler,
		topic:   topic,
	}
}

// Run starts the fills consumption loop. It blocks until the context is cancelled.
func (c *FillsConsumer) Run(ctx context.Context) error {
	slog.Info("Fills consumer started", "topic", c.topic)

	for {
		msg, err := c.reader.ReadMessage(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil // Graceful shutdown
			}
			slog.Error("Fills consumer error reading message", "error", err)
			time.Sleep(1 * time.Second)
			continue
		}

		var gatewayFill GatewayFill
		if err := json.Unmarshal(msg.Value, &gatewayFill); err != nil {
			slog.Warn("Failed to deserialize execution fill event",
				"error", err,
				"offset", msg.Offset,
			)
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

		// Dispatch back into execution engine
		c.handler(fill)
	}
}

// Close gracefully closes the fills consumer connection.
func (c *FillsConsumer) Close() error {
	slog.Info("Closing Kafka fills consumer connection")
	return c.reader.Close()
}
