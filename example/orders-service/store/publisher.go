package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/DjaPy/gokit-services/example/orders-service"
	"github.com/DjaPy/gokit-services/pkg/kafka/producer"
)

// KafkaPublisher is an orders.EventPublisher that writes events to Kafka
// through kafka/producer.
type KafkaPublisher struct {
	producer *producer.Producer
	topic    string
}

// NewKafkaPublisher builds a KafkaPublisher writing to topic via p.
func NewKafkaPublisher(p *producer.Producer, topic string) *KafkaPublisher {
	return &KafkaPublisher{producer: p, topic: topic}
}

// Publish marshals ev to JSON and produces it keyed by OrderID, so every
// event for one order lands on the same partition and keeps its order.
func (k *KafkaPublisher) Publish(ctx context.Context, ev orders.OrderEvent) error {
	payload, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("kafka publish %s: marshal: %w", ev.Type, err)
	}
	if errPr := k.producer.Produce(ctx, k.topic, []byte(ev.OrderID), payload, nil); errPr != nil {
		return fmt.Errorf("kafka publish %s: %w", ev.Type, errPr)
	}
	return nil
}
