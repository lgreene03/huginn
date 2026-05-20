package kafka

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/lgreene/huginn/internal/model"
	kgo "github.com/segmentio/kafka-go"
)

// GatewayOrder represents the exchange-facing order layout that Sleipnir expects.
type GatewayOrder struct {
	OrderID    string  `json:"order_id"`
	Instrument string  `json:"instrument"`
	Side       string  `json:"side"`       // "BUY" or "SELL"
	Quantity   float64 `json:"quantity"`
	Price      float64 `json:"price"`      // Ignore for MARKET orders
	Type       string  `json:"order_type"` // "LIMIT" or "MARKET"
}

// Producer writes execution intents to the specified topic in Redpanda.
type Producer struct {
	writer *kgo.Writer
	topic  string
}

// NewProducer initializes a new Kafka intent writer.
func NewProducer(brokers []string, topic string) *Producer {
	w := &kgo.Writer{
		Addr:         kgo.TCP(brokers...),
		Topic:        topic,
		Balancer:     &kgo.LeastBytes{},
		RequiredAcks: kgo.RequireAll,
		WriteTimeout: 10 * time.Second,
		ReadTimeout:  10 * time.Second,
	}

	return &Producer{
		writer: w,
		topic:  topic,
	}
}

// PublishIntent serializes and writes a GatewayOrder execution intent to Kafka.
func (p *Producer) PublishIntent(ctx context.Context, order model.Order, orderID string) error {
	orderType := "LIMIT"
	if order.LimitPrice == 0 {
		orderType = "MARKET"
	}

	gatewayOrder := GatewayOrder{
		OrderID:    orderID,
		Instrument: order.Instrument,
		Side:       order.Side.String(),
		Quantity:   order.Quantity,
		Price:      order.LimitPrice,
		Type:       orderType,
	}

	payload, err := json.Marshal(gatewayOrder)
	if err != nil {
		return fmt.Errorf("failed to marshal order intent: %w", err)
	}

	slog.Info("Publishing order intent to gateway",
		"topic", p.topic,
		"orderID", orderID,
		"instrument", gatewayOrder.Instrument,
		"side", gatewayOrder.Side,
		"qty", gatewayOrder.Quantity,
	)

	err = p.writer.WriteMessages(ctx, kgo.Message{
		Key:   []byte(orderID),
		Value: payload,
	})
	if err != nil {
		return fmt.Errorf("failed to write intent message to Kafka: %w", err)
	}

	return nil
}

// Close gracefully closes the producer connection.
func (p *Producer) Close() error {
	slog.Info("Closing Kafka producer connection")
	return p.writer.Close()
}
