package orders

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// InMemoryStore is a thread-safe in-memory Store implementation. It is the
// default backend for the example — no external dependencies, so the
// service runs with a bare `go run`. Its methods take a context to satisfy
// the Store interface but never block on it: all state lives behind a
// single mutex.
type InMemoryStore struct {
	mu     sync.RWMutex
	orders map[string]*Order
	nextID atomic.Uint64
}

// NewInMemoryStore creates an empty InMemoryStore.
func NewInMemoryStore() *InMemoryStore {
	return &InMemoryStore{orders: make(map[string]*Order)}
}

func (s *InMemoryStore) Create(_ context.Context, customerID string, items []Item) (Order, error) {
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
	return *o, nil
}

func (s *InMemoryStore) Get(_ context.Context, id string) (Order, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	o, ok := s.orders[id]
	if !ok {
		return Order{}, ErrOrderNotFound
	}
	return *o, nil
}

func (s *InMemoryStore) List(_ context.Context) ([]Order, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Order, 0, len(s.orders))
	for _, o := range s.orders {
		out = append(out, *o)
	}
	return out, nil
}

func (s *InMemoryStore) ConfirmPending(_ context.Context, id string) error {
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

func (s *InMemoryStore) CancelStalePending(_ context.Context, cutoff time.Time) ([]string, error) {
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
	return ids, nil
}
