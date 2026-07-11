// Package orders contains the domain logic and HTTP/gRPC transport adapters
// for the orders-service example. It is imported by
// cmd/orders-service/main.go, which only wires it together.
package orders

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// Status is the lifecycle state of an Order.
type Status string

const (
	StatusPending   Status = "PENDING"
	StatusConfirmed Status = "CONFIRMED"
	StatusCanceled  Status = "CANCELED"
)

// Item is a single line item within an Order.
type Item struct {
	SKU      string
	Quantity int32
}

// Order is the core domain entity. Both the HTTP and gRPC APIs read and
// write Orders through the same Store — the transport layer is just an
// adapter over shared business logic.
type Order struct {
	ID         string
	CustomerID string
	Items      []Item
	Status     Status
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// ErrOrderNotFound is returned by Store.Get when no order has the given ID.
var ErrOrderNotFound = errors.New("order not found")

// Store is a thread-safe in-memory order repository. It stands in for a
// real database in this example — the API and worker layers only depend on
// its exported methods, so swapping in Postgres/etc later would not change
// their code.
type Store struct {
	mu     sync.RWMutex
	orders map[string]*Order
	nextID atomic.Uint64
}

// NewStore creates an empty Store.
func NewStore() *Store {
	return &Store{orders: make(map[string]*Order)}
}

// Create inserts a new order in PENDING status and returns it.
func (s *Store) Create(customerID string, items []Item) *Order {
	id := fmt.Sprintf("ord_%d", s.nextID.Add(1))
	now := time.Now()
	o := &Order{
		ID:         id,
		CustomerID: customerID,
		Items:      items,
		Status:     StatusPending,
		CreatedAt:  now,
		UpdatedAt:  now,
	}

	s.mu.Lock()
	s.orders[id] = o
	s.mu.Unlock()
	return o
}

// Get returns a copy of the order with the given ID, or ErrOrderNotFound.
func (s *Store) Get(id string) (Order, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	o, ok := s.orders[id]
	if !ok {
		return Order{}, ErrOrderNotFound
	}
	return *o, nil
}

// List returns a copy of every known order.
func (s *Store) List() []Order {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Order, 0, len(s.orders))
	for _, o := range s.orders {
		out = append(out, *o)
	}
	return out
}

// ConfirmPending transitions an order to CONFIRMED, but only if it is still
// PENDING — a no-op if the periodic cleanup job already canceled it. This
// makes order processing and expiry race-free without an external lock:
// whichever of the two wins the store's mutex first determines the outcome.
func (s *Store) ConfirmPending(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.orders[id]
	if !ok {
		return ErrOrderNotFound
	}
	if o.Status != StatusPending {
		return nil
	}
	o.Status = StatusConfirmed
	o.UpdatedAt = time.Now()
	return nil
}

// CancelStalePending atomically finds every order still PENDING with
// CreatedAt before cutoff, cancels it, and returns the affected IDs. Used
// by the periodic cleanup job — scan and mutation happen under one lock so
// it can't race with a concurrent ConfirmPending on the same order.
func (s *Store) CancelStalePending(cutoff time.Time) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var ids []string
	for id, o := range s.orders {
		if o.Status == StatusPending && o.CreatedAt.Before(cutoff) {
			o.Status = StatusCanceled
			o.UpdatedAt = time.Now()
			ids = append(ids, id)
		}
	}
	return ids
}
