// Command orders-service is a realistic (if simplified) microservice built
// entirely from gokit-services primitives: an HTTP API and a gRPC API over
// one in-memory Store, async order processing via workerpool, a periodic
// job that expires stale orders, a healthserver readiness check tied to
// that job, an htmx dashboard that exercises all of the above over real
// network calls, and entrypoint orchestrating everything with graceful
// shutdown. The domain and transport code lives in the importable orders
// package one level up; this file only wires it together.
//
// Try it:
//
//	go run ./example/orders-service/cmd/orders-service
//	open http://localhost:8090          # dashboard
//	curl -s -X POST localhost:8080/orders -d '{"customer_id":"cust_1","items":[{"sku":"WIDGET","quantity":2}]}'
//	curl -s localhost:8080/orders
//	curl -s localhost:8082/healthz
//	curl -s localhost:8082/readyz
package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	grpclib "google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/DjaPy/gokit-services/entrypoint"
	orders "github.com/DjaPy/gokit-services/example/orders-service"
	ordersv1 "github.com/DjaPy/gokit-services/example/orders-service/proto"
	"github.com/DjaPy/gokit-services/grpcclient"
	"github.com/DjaPy/gokit-services/grpcserver"
	"github.com/DjaPy/gokit-services/healthserver"
	"github.com/DjaPy/gokit-services/httpclient"
	"github.com/DjaPy/gokit-services/httpserver"
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
)

func main() {
	store := orders.NewStore()
	processor := orders.NewOrderProcessor(store, workerPoolSize, orderProcessDelay)
	cleanup := orders.NewCleanupJob(store, staleAfter)

	// Own Prometheus registry shared by every httpserver-based service in
	// this process (httpSrv, healthSrv, dashboardSrv) — a single explicit
	// registerer keeps registration deterministic regardless of start order.
	registry := prometheus.NewRegistry()

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

	healthSrv := healthserver.New(
		healthserver.WithHost("0.0.0.0"),
		healthserver.WithPort(8082),
		healthserver.WithAppName("orders-service"),
		healthserver.WithProber(cleanup),
		healthserver.WithPrometheusRegisterer(registry),
	)

	ep := entrypoint.New(
		entrypoint.WithServices(
			httpSrv,
			grpcSrv,
			healthSrv,
			dashboardSrv,
			grpcClient,
			processor.Pool(),
			cleanup.Service(cleanupInterval),
		),
		entrypoint.WithShutdownTimeout(10*time.Second),
		entrypoint.WithPostStart(func(_ context.Context) error {
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

func mustHTTPClient(baseURL string) *httpclient.Client {
	c, err := httpclient.New(baseURL)
	if err != nil {
		panic(err)
	}
	return c
}
