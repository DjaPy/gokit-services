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
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	grpclib "google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/DjaPy/gokit-services/dbservice"
	"github.com/DjaPy/gokit-services/entrypoint"
	orders "github.com/DjaPy/gokit-services/example/orders-service"
	ordersv1 "github.com/DjaPy/gokit-services/example/orders-service/proto"
	"github.com/DjaPy/gokit-services/grpcclient"
	"github.com/DjaPy/gokit-services/grpcserver"
	"github.com/DjaPy/gokit-services/healthserver"
	"github.com/DjaPy/gokit-services/httpclient"
	"github.com/DjaPy/gokit-services/httpserver"
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

	// defaultPostgresDSN targets the local instance from
	// docker-compose.test.yml; override with ORDERS_POSTGRES_DSN. The
	// embedded credentials are the well-known local dev ones, not a secret.
	defaultPostgresDSN = "postgresql://gokit:gokit@localhost:5432/gokit_test" //nolint:gosec // non-secret local dev default
	// dbConnectTimeout bounds how long PostStart waits for dbservice to
	// finish its retrying connect before giving up and failing startup.
	dbConnectTimeout = 15 * time.Second
)

func main() {
	// Own Prometheus registry shared by every metric-emitting service in
	// this process (httpSrv, healthSrv, dashboardSrv, and dbservice when
	// enabled) — a single explicit registerer keeps registration
	// deterministic regardless of start order.
	registry := prometheus.NewRegistry()

	// store is chosen once here; every downstream component depends only on
	// the orders.Store interface, so nothing else changes between backends.
	// dbSvc is non-nil only in the Postgres path — it is added to the
	// entrypoint lifecycle (retrying connect, pool metrics, graceful close)
	// and to the readiness probes below.
	store, dbSvc := newStore(registry)

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

	// The dashboard reaches every other service the same way an external
	// caller would: httpclient for REST/health, a managed grpcclient
	// connection for gRPC. mustHTTPClient panics on a malformed base URL —
	// acceptable here since httpAddr/healthAddr are compile-time constants.
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

	healthOpts := []healthserver.Option{
		healthserver.WithHost("0.0.0.0"),
		healthserver.WithPort(8082),
		healthserver.WithAppName("orders-service"),
		healthserver.WithProber(cleanup),
		healthserver.WithPrometheusRegisterer(registry),
	}
	// dbservice implements service.Prober (Ping-based), so in the Postgres
	// path readyz stays red until the database is actually reachable.
	if dbSvc != nil {
		healthOpts = append(healthOpts, healthserver.WithProber(dbSvc))
	}
	healthSrv := healthserver.New(healthOpts...)

	svcs := []service.Service{
		httpSrv,
		grpcSrv,
		healthSrv,
		dashboardSrv,
		grpcClient,
		processor.Pool(),
		cleanup.Service(cleanupInterval),
	}
	if dbSvc != nil {
		svcs = append(svcs, dbSvc)
	}

	ep := entrypoint.New(
		entrypoint.WithServices(svcs...),
		entrypoint.WithShutdownTimeout(10*time.Second),
		entrypoint.WithPostStart(func(ctx context.Context) error {
			// With a Postgres backend, create the schema once the pool is
			// connected before announcing readiness. In-memory backend: no-op.
			if err := ensureSchemaOnceConnected(ctx, store, dbSvc); err != nil {
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

	// entrypoint.New already catches SIGINT/SIGTERM by default, so
	// context.Background() is the right ctx for a real long-running service
	// — Ctrl+C (or a container orchestrator's SIGTERM) triggers shutdown.
	if err := ep.Run(context.Background()); err != nil {
		slog.Info("orders-service stopped", "cause", err)
	}
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

// ensureSchemaOnceConnected waits (bounded by dbConnectTimeout) for
// dbservice to finish its retrying connect, then creates the orders schema.
// It is a no-op for the in-memory backend. Bounding the wait matters
// because PostStart runs before entrypoint begins listening for shutdown
// signals — an unbounded wait here would make the process unkillable while
// the database is down.
func ensureSchemaOnceConnected(ctx context.Context, store orders.Store, dbSvc *dbservice.Service) error {
	pg, ok := store.(*orders.PostgresStore)
	if !ok || dbSvc == nil {
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
