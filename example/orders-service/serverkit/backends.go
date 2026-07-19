package serverkit

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/DjaPy/gokit-services/dbservice"
	"github.com/DjaPy/gokit-services/example/orders-service"
	"github.com/DjaPy/gokit-services/example/orders-service/store"
	"github.com/DjaPy/gokit-services/kafka/consumer"
	"github.com/DjaPy/gokit-services/kafka/producer"
	"github.com/DjaPy/gokit-services/redisservice"
	"github.com/DjaPy/gokit-services/service"
)

const (
	defaultPostgresDSN = "postgresql://gokit:gokit@localhost:5432/gokit_test" //nolint:gosec // non-secret local dev default
	dbConnectTimeout   = 15 * time.Second

	defaultRedisDSN     = "redis://localhost:6379/0"
	defaultKafkaBrokers = "localhost:9092"
	cacheTTL            = 30 * time.Second

	appName = "orders-service"
)

// ManagedService is implemented by every optional infrastructure backend
// (dbservice, redisservice, kafka producer/consumer): each is a lifecycle
// Service and a readiness Prober, so callers can register them uniformly.
type ManagedService interface {
	service.Service
	service.Prober
}

// Backends is the composed order store together with every optional
// infrastructure service enabled by an env flag. Store is what the API and
// worker layers use; Optional is registered into the entrypoint lifecycle and
// readiness probes by the caller.
type Backends struct {
	Store    orders.Store
	Optional []ManagedService

	pg        *store.PostgresStore
	db        *dbservice.Service
	ready     chan struct{}
	readyOnce sync.Once
}

// NewBackends composes the order store from its base backend (ORDERS_STORE)
// and optional decorators: a Redis read-through cache (ORDERS_REDIS=on) and
// Kafka event publishing plus a consumer that echoes events back (ORDERS_KAFKA=on).
// With no flags set it returns the dependency-free in-memory store, so
// `go run` works out of the box.
func NewBackends(registry prometheus.Registerer) *Backends {
	b := &Backends{ready: make(chan struct{})}

	if os.Getenv("ORDERS_STORE") == "postgres" {
		dsn := envOr("ORDERS_POSTGRES_DSN", defaultPostgresDSN)
		b.db = dbservice.New(dsn,
			dbservice.WithAppName(appName),
			dbservice.WithPrometheusRegisterer(registry),
		)
		b.pg = store.NewPostgresStore(b.db)
		b.Store = b.pg
		b.Optional = append(b.Optional, b.db)
		slog.Info("orders-service store backend", "backend", "postgres")
	} else {
		b.Store = orders.NewInMemoryStore()
		slog.Info("orders-service store backend", "backend", "in-memory")
	}

	if os.Getenv("ORDERS_REDIS") == "on" {
		dsn := envOr("ORDERS_REDIS_DSN", defaultRedisDSN)
		redis := redisservice.New(dsn,
			redisservice.WithAppName(appName),
			redisservice.WithPrometheusRegisterer(registry),
		)
		b.Store = store.NewCachingStore(b.Store, redis, cacheTTL)
		b.Optional = append(b.Optional, redis)
		slog.Info("orders-service cache backend", "backend", "redis")
	}

	if os.Getenv("ORDERS_KAFKA") == "on" {
		brokers := strings.Split(envOr("ORDERS_KAFKA_BROKERS", defaultKafkaBrokers), ",")
		prod := producer.New(brokers,
			producer.WithAppName(appName),
			producer.WithPrometheusRegisterer(registry),
		)
		b.Store = orders.NewPublishingStore(b.Store, store.NewKafkaPublisher(prod, orders.OrdersEventsTopic))

		cons := consumer.New(brokers, appName,
			consumer.WithAppName(appName),
			consumer.WithPrometheusRegisterer(registry),
		)
		cons.Handle(orders.OrdersEventsTopic, logOrderEvent)
		b.Optional = append(b.Optional, prod, cons)
		slog.Info("orders-service event backend", "backend", "kafka")
	}

	return b
}

// Prepare makes the backend usable and then signals readiness: it waits
// (bounded by dbConnectTimeout) for dbservice to finish its retrying connect,
// ensures the orders schema exists (a no-op for the in-memory backend), and on
// success unblocks WaitReady waiters. It is idempotent — repeated calls signal
// readiness at most once. Call it from entrypoint's PostStart.
func (b *Backends) Prepare(ctx context.Context) error {
	if b.pg != nil && b.db != nil {
		waitCtx, cancel := context.WithTimeout(ctx, dbConnectTimeout)
		defer cancel()
		for b.db.Pool() == nil {
			select {
			case <-waitCtx.Done():
				return fmt.Errorf("waiting for postgres connection: %w", waitCtx.Err())
			case <-time.After(50 * time.Millisecond):
			}
		}
		if err := b.pg.EnsureSchema(ctx); err != nil {
			return fmt.Errorf("ensure postgres schema: %w", err)
		}
	}
	b.readyOnce.Do(func() { close(b.ready) })
	return nil
}

// WaitReady blocks until Prepare has completed (the backend is connected and
// its schema exists) or ctx is done. Background jobs like the cleanup sweep use
// it to avoid running against a store that is still connecting. After Prepare
// has run it returns immediately.
func (b *Backends) WaitReady(ctx context.Context) error {
	select {
	case <-b.ready:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("wait ready: %w", ctx.Err())
	}
}

func logOrderEvent(_ context.Context, msg consumer.Message) error {
	var ev orders.OrderEvent
	if err := json.Unmarshal(msg.Value, &ev); err != nil {
		return fmt.Errorf("decode order event: %w", err)
	}
	slog.Info("orders-service event consumed",
		"type", ev.Type, "order_id", ev.OrderID, "status", ev.Status)
	return nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
