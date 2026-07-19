package orders

import (
	"context"
	"fmt"
	"log/slog"
	"time"
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

// EventPublisher publishes order domain events. Implementations live outside
// the domain package (e.g. store.KafkaPublisher); PublishingStore depends only
// on this interface, so the domain never imports a transport.
type EventPublisher interface {
	Publish(ctx context.Context, ev OrderEvent) error
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

func (s *PublishingStore) ConfirmPending(ctx context.Context, id string) (bool, error) {
	confirmed, err := s.Store.ConfirmPending(ctx, id)
	if err != nil {
		return false, fmt.Errorf("publishing store: confirm: %w", err)
	}

	if confirmed {
		s.emit(ctx, OrderEvent{Type: EventConfirmed, OrderID: id, Status: string(StatusConfirmed), At: time.Now()})
	}
	return confirmed, nil
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
