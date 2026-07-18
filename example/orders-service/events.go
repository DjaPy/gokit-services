package orders

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/DjaPy/gokit-services/kafka/producer"
)

// OrdersEventsTopic is the Kafka topic order domain events are published to.
const OrdersEventsTopic = "orders.events"

// Order event types carried in OrderEvent.Type.
const (
	EventCreated   = "created"
	EventConfirmed = "confirmed"
	EventCanceled  = "canceled"
)

// OrderEvent is a domain event emitted when an order changes state. It is a
// plain domain type with no dependency on any infrastructure package, so the
// event model stays independent of how (or whether) events are transported.
type OrderEvent struct {
	Type       string    `json:"type"`
	OrderID    string    `json:"order_id"`
	CustomerID string    `json:"customer_id"`
	Status     string    `json:"status"`
	At         time.Time `json:"at"`
}

// EventPublisher publishes order domain events. The implementation is chosen
// once in main: NopPublisher by default, KafkaPublisher when ORDERS_KAFKA=on.
type EventPublisher interface {
	Publish(ctx context.Context, ev OrderEvent) error
}

// NopPublisher is the default no-op EventPublisher, so the example runs
// without Kafka and PublishingStore always has a non-nil publisher.
type NopPublisher struct{}

// Publish discards the event.
func (NopPublisher) Publish(context.Context, OrderEvent) error { return nil }

// KafkaPublisher publishes events to Kafka through kafka/producer.
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
func (k *KafkaPublisher) Publish(ctx context.Context, ev OrderEvent) error {
	payload, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("kafka publish %s: marshal: %w", ev.Type, err)
	}
	if err := k.producer.Produce(ctx, k.topic, []byte(ev.OrderID), payload, nil); err != nil {
		return fmt.Errorf("kafka publish %s: %w", ev.Type, err)
	}
	return nil
}

// PublishingStore decorates a Store, emitting an OrderEvent after each
// successful mutation. Publishing is best-effort: a publish failure is logged
// but never fails the domain operation — the order write has already
// committed, and events are a secondary concern. The embedded Store provides
// the read methods (Get/List) unchanged; only the mutating methods are
// overridden to emit events.
type PublishingStore struct {
	Store
	publisher EventPublisher
	logger    *slog.Logger
}

// NewPublishingStore wraps inner so its mutations emit events through publisher.
func NewPublishingStore(inner Store, publisher EventPublisher) *PublishingStore {
	return &PublishingStore{Store: inner, publisher: publisher, logger: slog.Default()}
}

func (s *PublishingStore) emit(ctx context.Context, ev OrderEvent) {
	if err := s.publisher.Publish(ctx, ev); err != nil {
		s.logger.Error("orders: publish event", "type", ev.Type, "order_id", ev.OrderID, "error", err)
	}
}

func (s *PublishingStore) Create(ctx context.Context, customerID string, items []Item) (Order, error) {
	o, err := s.Store.Create(ctx, customerID, items)
	if err != nil {
		return Order{}, fmt.Errorf("publishing store: create: %w", err)
	}
	s.emit(ctx, OrderEvent{
		Type:       EventCreated,
		OrderID:    o.ID,
		CustomerID: o.CustomerID,
		Status:     string(o.Status),
		At:         time.Now(),
	})
	return o, nil
}

func (s *PublishingStore) ConfirmPending(ctx context.Context, id string) error {
	if err := s.Store.ConfirmPending(ctx, id); err != nil {
		return fmt.Errorf("publishing store: confirm: %w", err)
	}
	s.emit(ctx, OrderEvent{Type: EventConfirmed, OrderID: id, Status: string(StatusConfirmed), At: time.Now()})
	return nil
}

func (s *PublishingStore) CancelStalePending(ctx context.Context, cutoff time.Time) ([]string, error) {
	ids, err := s.Store.CancelStalePending(ctx, cutoff)
	if err != nil {
		return nil, fmt.Errorf("publishing store: cancel stale: %w", err)
	}
	for _, id := range ids {
		s.emit(ctx, OrderEvent{Type: EventCanceled, OrderID: id, Status: string(StatusCanceled), At: time.Now()})
	}
	return ids, nil
}
