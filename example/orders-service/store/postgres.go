// Package store holds the infrastructure-backed orders.Store adapters
// (PostgreSQL, Redis cache, Kafka event publishing). Keeping them out of the
// orders package means a consumer that only wants the domain and the in-memory
// store does not transitively pull in pgx, redis, or kafka.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	orders "github.com/DjaPy/gokit-services/example/orders-service"
)

// PostgresStore is an orders.Store backed by a PostgreSQL table. It reads the
// live *pgxpool.Pool from a poolProvider (satisfied by *dbservice.Service) on
// every call rather than capturing it once, so it tolerates the pool being
// established asynchronously after Start — Create/Get/etc. return an error
// until the pool is connected instead of dereferencing nil.
//
// Orders are stored one row per order with the line items as a JSONB
// column; this keeps the example self-contained (one table, no joins)
// while still exercising a real database round-trip for every operation.
type PostgresStore struct {
	pool poolProvider
}

// poolProvider yields the currently-connected pool, or nil before Start has
// connected. *dbservice.Service satisfies it via its Pool() accessor.
type poolProvider interface {
	Pool() *pgxpool.Pool
}

// NewPostgresStore creates a PostgresStore that draws its connection pool
// from the given provider. Call EnsureSchema once the pool is connected,
// before serving traffic.
func NewPostgresStore(pool poolProvider) *PostgresStore {
	return &PostgresStore{pool: pool}
}

// errStoreNotConnected is returned while the underlying pool is still nil
// (dbservice has not finished its retrying Start yet).
var errStoreNotConnected = errors.New("postgres store: database not connected")

func (s *PostgresStore) db() (*pgxpool.Pool, error) {
	p := s.pool.Pool()
	if p == nil {
		return nil, errStoreNotConnected
	}
	return p, nil
}

// EnsureSchema creates the orders table and its ID sequence if they do not
// already exist. It is idempotent and safe to call on every startup.
func (s *PostgresStore) EnsureSchema(ctx context.Context) error {
	p, err := s.db()
	if err != nil {
		return err
	}
	const ddl = `
CREATE SEQUENCE IF NOT EXISTS orders_id_seq;
CREATE TABLE IF NOT EXISTS orders (
    id          TEXT        PRIMARY KEY,
    customer_id TEXT        NOT NULL,
    items       JSONB       NOT NULL,
    status      TEXT        NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL,
    updated_at  TIMESTAMPTZ NOT NULL
);`
	if _, err := p.Exec(ctx, ddl); err != nil {
		return fmt.Errorf("postgres store: ensure schema: %w", err)
	}
	return nil
}

func (s *PostgresStore) Create(ctx context.Context, customerID string, items []orders.Item) (orders.Order, error) {
	p, err := s.db()
	if err != nil {
		return orders.Order{}, err
	}
	itemsJSON, err := json.Marshal(items)
	if err != nil {
		return orders.Order{}, fmt.Errorf("postgres store: marshal items: %w", err)
	}

	var q = `
INSERT INTO orders (id, customer_id, items, status, created_at, updated_at)
VALUES ('ord_' || nextval('orders_id_seq'), $1, $2, $3, now(), now())
RETURNING id, created_at, updated_at`

	o := orders.Order{CustomerID: customerID, Items: items, Status: orders.StatusPending}
	if err := p.QueryRow(ctx, q, customerID, itemsJSON, orders.StatusPending).
		Scan(&o.ID, &o.CreatedAt, &o.UpdatedAt); err != nil {
		return orders.Order{}, fmt.Errorf("postgres store: create: %w", err)
	}
	return o, nil
}

func (s *PostgresStore) Get(ctx context.Context, id string) (orders.Order, error) {
	p, err := s.db()
	if err != nil {
		return orders.Order{}, err
	}
	var q = `SELECT id, customer_id, items, status, created_at, updated_at FROM orders WHERE id = $1`
	o, err := scanOrder(p.QueryRow(ctx, q, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return orders.Order{}, orders.ErrOrderNotFound
	}
	if err != nil {
		return orders.Order{}, fmt.Errorf("postgres store: get: %w", err)
	}
	return o, nil
}

func (s *PostgresStore) List(ctx context.Context) ([]orders.Order, error) {
	p, err := s.db()
	if err != nil {
		return nil, err
	}
	const q = `SELECT id, customer_id, items, status, created_at, updated_at FROM orders ORDER BY created_at`
	rows, err := p.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("postgres store: list: %w", err)
	}
	defer rows.Close()

	var out []orders.Order
	for rows.Next() {
		o, err := scanOrder(rows)
		if err != nil {
			return nil, fmt.Errorf("postgres store: list scan: %w", err)
		}
		out = append(out, o)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres store: list: %w", err)
	}
	return out, nil
}

func (s *PostgresStore) ConfirmPending(ctx context.Context, id string) (bool, error) {
	p, err := s.db()
	if err != nil {
		return false, err
	}

	const q = `UPDATE orders SET status = $1, updated_at = now() WHERE id = $2 AND status = $3`
	tag, err := p.Exec(ctx, q, orders.StatusConfirmed, id, orders.StatusPending)
	if err != nil {
		return false, fmt.Errorf("postgres store: confirm: %w", err)
	}
	if tag.RowsAffected() > 0 {
		return true, nil
	}

	var exists bool
	if err := p.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM orders WHERE id = $1)`, id).Scan(&exists); err != nil {
		return false, fmt.Errorf("postgres store: confirm existence check: %w", err)
	}
	if !exists {
		return false, orders.ErrOrderNotFound
	}
	return false, nil
}

func (s *PostgresStore) CancelStalePending(ctx context.Context, cutoff time.Time) ([]string, error) {
	p, err := s.db()
	if err != nil {
		return nil, err
	}
	const q = `
UPDATE orders SET status = $1, updated_at = now()
WHERE status = $2 AND created_at < $3
RETURNING id`
	rows, err := p.Query(ctx, q, orders.StatusCanceled, orders.StatusPending, cutoff)
	if err != nil {
		return nil, fmt.Errorf("postgres store: cancel stale: %w", err)
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("postgres store: cancel stale scan: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres store: cancel stale: %w", err)
	}
	return ids, nil
}

// scanOrder decodes one row (id, customer_id, items, status, created_at,
// updated_at) into an orders.Order, unmarshaling the JSONB items column.
func scanOrder(row pgx.Row) (orders.Order, error) {
	var (
		o         orders.Order
		itemsJSON []byte
	)
	if err := row.Scan(&o.ID, &o.CustomerID, &itemsJSON, &o.Status, &o.CreatedAt, &o.UpdatedAt); err != nil {
		// Wrapped with %w so callers can still errors.Is(..., pgx.ErrNoRows).
		return orders.Order{}, fmt.Errorf("scan order row: %w", err)
	}
	if err := json.Unmarshal(itemsJSON, &o.Items); err != nil {
		return orders.Order{}, fmt.Errorf("unmarshal items for order %s: %w", o.ID, err)
	}
	return o, nil
}
