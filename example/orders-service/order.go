// Package orders contains the domain logic and HTTP/gRPC transport adapters
// for the orders-service example. It is imported by
// cmd/orders-service/main.go, which only wires it together.
package orders

import (
	"context"
	"errors"
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
// adapter over shared business logic, and the storage layer is just an
// implementation of the Store interface below.
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

// Store is the order repository contract. The API and worker layers depend
// only on this interface, so the example can run against either an
// InMemoryStore (the default) or a PostgresStore (backed by dbservice)
// without any change to the transport or business-logic code — the choice
// is made once, in main. Every method takes a context so a real
// database-backed implementation can honor cancellation and deadlines.
type Store interface {
	// Create inserts a new order in PENDING status and returns it.
	Create(ctx context.Context, customerID string, items []Item) (Order, error)
	// Get returns the order with the given ID, or ErrOrderNotFound.
	Get(ctx context.Context, id string) (Order, error)
	// List returns every known order.
	List(ctx context.Context) ([]Order, error)
	// ConfirmPending transitions an order to CONFIRMED, but only if it is
	// still PENDING — a no-op if the cleanup job already canceled it, and
	// ErrOrderNotFound if no such order exists. This makes order processing
	// and expiry race-free: whichever transition commits first wins.
	ConfirmPending(ctx context.Context, id string) error
	// CancelStalePending atomically cancels every order still PENDING with
	// CreatedAt before cutoff and returns the affected IDs. Used by the
	// periodic cleanup job; it must not race with a concurrent
	// ConfirmPending on the same order.
	CancelStalePending(ctx context.Context, cutoff time.Time) ([]string, error)
}
