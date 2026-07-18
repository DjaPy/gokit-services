// Command orders-service is a realistic (if simplified) microservice built
// entirely from gokit-services primitives: an HTTP API and a gRPC API over
// a pluggable Store (in-memory by default, or PostgreSQL via dbservice when
// ORDERS_STORE=postgres), async order processing via workerpool, a periodic
// job that expires stale orders, a healthserver readiness check tied to
// that job, an htmx dashboard that exercises all of the above over real
// network calls, and entrypoint orchestrating everything with graceful
// shutdown. The domain and transport code lives in the importable orders
// package one level up; this file only wires it together.
//
// Try it:
//
//	go run ./example/orders-service/cmd/orders-service
//	# or against Postgres (see docker-compose.test.yml for a local instance):
//	ORDERS_STORE=postgres go run ./example/orders-service/cmd/orders-service
//	open http://localhost:8090          # dashboard
//	curl -s -X POST localhost:8080/orders -d '{"customer_id":"cust_1","items":[{"sku":"WIDGET","quantity":2}]}'
//	curl -s localhost:8080/orders
//	curl -s localhost:8082/healthz
//	curl -s localhost:8082/readyz
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	grpclib "google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/DjaPy/gokit-services/dbservice"
	"github.com/DjaPy/gokit-services/entrypoint"
	"github.com/DjaPy/gokit-services/example/orders-service"
	ordersv1 "github.com/DjaPy/gokit-services/example/orders-service/proto"
	"github.com/DjaPy/gokit-services/grpcclient"
	"github.com/DjaPy/gokit-services/grpcserver"
	"github.com/DjaPy/gokit-services/healthserver"
	"github.com/DjaPy/gokit-services/httpclient"
	"github.com/DjaPy/gokit-services/httpserver"
	"github.com/DjaPy/gokit-services/kafka/consumer"
	"github.com/DjaPy/gokit-services/kafka/producer"
	"github.com/DjaPy/gokit-services/redisservice"
	"github.com/DjaPy/gokit-services/service"
)

const (
	workerPoolSize    = 4
	orderProcessDelay = 300 * time.Millisecond
	cleanupInterval   = 5 * time.Second
	staleAfter        = 10 * time.Second

	httpAddr      = "127.0.0.1:8080"
	grpcAddr      = "127.0.0.1:9090"
	healthAddr    = "127.0.0.1:8082"
	dashboardPort = 8090

	defaultPostgresDSN = "postgresql://gokit:gokit@localhost:5432/gokit_test" //nolint:gosec // non-secret local dev default
	dbConnectTimeout   = 15 * time.Second

	defaultRedisDSN     = "redis://localhost:6379/0"
	defaultKafkaBrokers = "localhost:9092"
	cacheTTL            = 30 * time.Second
)

func main() {
	// Own Prometheus registry shared by every metric-emitting service in
	// this process (httpSrv, healthSrv, dashboardSrv, and dbservice when
	// enabled) — a single explicit registerer keeps registration
	// deterministic regardless of start order.
	registry := prometheus.NewRegistry()

	// The order store is composed once here; every downstream component
	// depends only on the orders.Store interface, so nothing else changes
	// between backends or decorators. Optional infrastructure services
	// (Postgres, Redis, Kafka) are non-nil only when their env flag enables
	// them, and each is added to the entrypoint lifecycle and readiness
	// probes below.
	b := newBackends(registry)
	store := b.store

	processor := orders.NewOrderProcessor(store, workerPoolSize, orderProcessDelay)
	cleanup := orders.NewCleanupJob(store, staleAfter)

	httpAPI := orders.NewHTTPAPI(store, processor)
	httpSrv := httpserver.NewServer(httpAPI.Mux(),
		httpserver.WithHost("0.0.0.0"),
		httpserver.WithPort(8080),
		httpserver.WithAppName("orders-service"),
		httpserver.WithPrometheusRegisterer(registry),
	)

	grpcSrv := grpcserver.NewServer(grpcserver.WithPort(9090))
	ordersv1.RegisterOrdersServiceServer(grpcSrv.GRPCServer(), orders.NewGRPCAPI(store, processor))

	restClient := mustHTTPClient("http://" + httpAddr)
	healthClient := mustHTTPClient("http://" + healthAddr)
	grpcClient := grpcclient.NewClient(grpcAddr,
		grpcclient.WithDialOptions(grpclib.WithTransportCredentials(insecure.NewCredentials())),
	)

	dashboard := orders.NewDashboard(restClient, healthClient, grpcClient)
	dashboardSrv := httpserver.NewServer(dashboard.Mux(),
		httpserver.WithHost("0.0.0.0"),
		httpserver.WithPort(dashboardPort),
		httpserver.WithAppName("orders-service-dashboard"),
		httpserver.WithPrometheusRegisterer(registry),
	)

	probers := b.probers()
	healthOpts := make([]healthserver.Option, 0, 5+len(probers))
	healthOpts = append(healthOpts,
		healthserver.WithHost("0.0.0.0"),
		healthserver.WithPort(8082),
		healthserver.WithAppName("orders-service"),
		healthserver.WithProber(cleanup),
		healthserver.WithPrometheusRegisterer(registry),
	)
	for _, p := range probers {
		healthOpts = append(healthOpts, healthserver.WithProber(p))
	}
	healthSrv := healthserver.New(healthOpts...)

	optional := b.services()
	svcs := make([]service.Service, 0, 7+len(optional))
	svcs = append(svcs,
		httpSrv,
		grpcSrv,
		healthSrv,
		dashboardSrv,
		grpcClient,
		processor.Pool(),
		cleanup.Service(cleanupInterval),
	)
	svcs = append(svcs, optional...)

	ep := entrypoint.New(
		entrypoint.WithServices(svcs...),
		entrypoint.WithShutdownTimeout(10*time.Second),
		entrypoint.WithPostStart(func(ctx context.Context) error {
			if err := ensureSchemaOnceConnected(ctx, b.pg, b.db); err != nil {
				return err
			}
			slog.Info("orders-service ready",
				"dashboard", "http://localhost:8090",
				"http_addr", httpSrv.Addr(),
				"grpc_addr", grpcSrv.Addr(),
				"health_addr", "http://"+healthAddr,
			)
			return nil
		}),
	)
	if err := ep.Run(context.Background()); err != nil {
		slog.Info("orders-service stopped", "cause", err)
	}
}

// backends holds the composed order store and every optional infrastructure
// service enabled by an env flag. Each service field is nil unless its flag
// turned it on; main registers the non-nil ones into the entrypoint lifecycle
// and readiness probes.
type backends struct {
	store orders.Store          // possibly decorated (caching/publishing)
	pg    *orders.PostgresStore // undecorated Postgres base, for schema init; nil otherwise
	db    *dbservice.Service    // nil unless ORDERS_STORE=postgres
	redis *redisservice.Service // nil unless ORDERS_REDIS=on
	prod  *producer.Producer    // nil unless ORDERS_KAFKA=on
	cons  *consumer.Consumer    // nil unless ORDERS_KAFKA=on
}

// newBackends composes the order store from its base backend (ORDERS_STORE)
// and optional decorators: a Redis read-through cache (ORDERS_REDIS=on) and
// Kafka event publishing plus a consumer that echoes events back (ORDERS_KAFKA=on).
// With no flags set it returns the dependency-free in-memory store, so
// `go run` works out of the box.
func newBackends(registry prometheus.Registerer) backends {
	base, db := newStore(registry)
	b := backends{store: base, db: db}
	if db != nil {
		b.pg = base.(*orders.PostgresStore)
	}

	if os.Getenv("ORDERS_REDIS") == "on" {
		dsn := envOr("ORDERS_REDIS_DSN", defaultRedisDSN)
		b.redis = redisservice.New(dsn,
			redisservice.WithAppName("orders-service"),
			redisservice.WithPrometheusRegisterer(registry),
		)
		b.store = orders.NewCachingStore(b.store, b.redis, cacheTTL)
		slog.Info("orders-service cache backend", "backend", "redis")
	}

	if os.Getenv("ORDERS_KAFKA") == "on" {
		brokers := strings.Split(envOr("ORDERS_KAFKA_BROKERS", defaultKafkaBrokers), ",")
		b.prod = producer.New(brokers,
			producer.WithAppName("orders-service"),
			producer.WithPrometheusRegisterer(registry),
		)
		b.store = orders.NewPublishingStore(b.store, orders.NewKafkaPublisher(b.prod, orders.OrdersEventsTopic))

		b.cons = consumer.New(brokers, "orders-service",
			consumer.WithAppName("orders-service"),
			consumer.WithPrometheusRegisterer(registry),
		)
		b.cons.Handle(orders.OrdersEventsTopic, logOrderEvent)
		slog.Info("orders-service event backend", "backend", "kafka")
	}

	return b
}

func (b backends) services() []service.Service {
	var out []service.Service
	if b.db != nil {
		out = append(out, b.db)
	}
	if b.redis != nil {
		out = append(out, b.redis)
	}
	if b.prod != nil {
		out = append(out, b.prod)
	}
	if b.cons != nil {
		out = append(out, b.cons)
	}
	return out
}

// probers returns the optional backends as readiness probers.
func (b backends) probers() []service.Prober {
	var out []service.Prober
	if b.db != nil {
		out = append(out, b.db)
	}
	if b.redis != nil {
		out = append(out, b.redis)
	}
	if b.prod != nil {
		out = append(out, b.prod)
	}
	if b.cons != nil {
		out = append(out, b.cons)
	}
	return out
}

// logOrderEvent is the consumer handler for the orders.events topic: it
// decodes the event and logs it, demonstrating the produce → consume path.
func logOrderEvent(_ context.Context, msg consumer.Message) error {
	var ev orders.OrderEvent
	if err := json.Unmarshal(msg.Value, &ev); err != nil {
		return fmt.Errorf("decode order event: %w", err)
	}
	slog.Info("orders-service event consumed",
		"type", ev.Type, "order_id", ev.OrderID, "status", ev.Status)
	return nil
}

// newStore selects the order repository backend from ORDERS_STORE. The
// default (unset or anything other than "postgres") is the dependency-free
// in-memory store, so `go run` works out of the box. "postgres" builds a
// dbservice-backed store; the returned *dbservice.Service is registered
// into the entrypoint lifecycle by the caller.
func newStore(registry prometheus.Registerer) (orders.Store, *dbservice.Service) {
	if os.Getenv("ORDERS_STORE") != "postgres" {
		slog.Info("orders-service store backend", "backend", "in-memory")
		return orders.NewInMemoryStore(), nil
	}

	dsn := os.Getenv("ORDERS_POSTGRES_DSN")
	if dsn == "" {
		dsn = defaultPostgresDSN
	}
	dbSvc := dbservice.New(dsn,
		dbservice.WithAppName("orders-service"),
		dbservice.WithPrometheusRegisterer(registry),
	)
	slog.Info("orders-service store backend", "backend", "postgres")
	return orders.NewPostgresStore(dbSvc), dbSvc
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// ensureSchemaOnceConnected waits (bounded by dbConnectTimeout) for
// dbservice to finish its retrying connect, then creates the orders schema.
// It is a no-op for the in-memory backend. Bounding the wait matters
// because PostStart runs before entrypoint begins listening for shutdown
// signals — an unbounded wait here would make the process unkillable while
// the database is down.
func ensureSchemaOnceConnected(ctx context.Context, pg *orders.PostgresStore, dbSvc *dbservice.Service) error {
	if pg == nil || dbSvc == nil {
		return nil
	}

	waitCtx, cancel := context.WithTimeout(ctx, dbConnectTimeout)
	defer cancel()
	for dbSvc.Pool() == nil {
		select {
		case <-waitCtx.Done():
			return fmt.Errorf("waiting for postgres connection: %w", waitCtx.Err())
		case <-time.After(50 * time.Millisecond):
		}
	}
	if err := pg.EnsureSchema(ctx); err != nil {
		return fmt.Errorf("ensure postgres schema: %w", err)
	}
	return nil
}

func mustHTTPClient(baseURL string) *httpclient.Client {
	c, err := httpclient.New(baseURL)
	if err != nil {
		panic(err)
	}
	return c
}
