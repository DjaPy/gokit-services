package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	grpclib "google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/DjaPy/gokit-services/example/orders-service"
	ordersv1 "github.com/DjaPy/gokit-services/example/orders-service/proto"
	"github.com/DjaPy/gokit-services/example/orders-service/serverkit"
	"github.com/DjaPy/gokit-services/pkg/core/entrypoint"
	"github.com/DjaPy/gokit-services/pkg/core/service"
	grpccli "github.com/DjaPy/gokit-services/pkg/grpc/client"
	grpcsrv "github.com/DjaPy/gokit-services/pkg/grpc/server"
	"github.com/DjaPy/gokit-services/pkg/healthserver"
)

const (
	workerPoolSize    = 4
	orderProcessDelay = 300 * time.Millisecond
	cleanupInterval   = 5 * time.Second
	staleAfter        = 10 * time.Second

	bindHost = "0.0.0.0"   // servers bind all interfaces
	dialHost = "127.0.0.1" // in-process clients dial loopback

	httpPort      = 8080
	grpcPort      = 9090
	healthPort    = 8082
	dashboardPort = 8090
)

func main() {
	registry := prometheus.NewRegistry()
	backend := serverkit.NewBackends(registry)

	processor := orders.NewOrderProcessor(backend.Store, workerPoolSize, orderProcessDelay)
	cleanup := orders.NewCleanupJob(backend.Store, staleAfter, orders.WithReadyGate(backend.WaitReady))

	httpSrv := serverkit.NewHTTPServer(bindHost, httpPort, "orders-service", orders.NewHTTPAPI(backend.Store, processor).Mux(), registry)

	grpcSrv := grpcsrv.NewServer(grpcsrv.WithPort(grpcPort))
	ordersv1.RegisterOrdersServiceServer(grpcSrv.GRPCServer(), orders.NewGRPCAPI(backend.Store, processor))

	healthAddr := serverkit.Addr(dialHost, healthPort)
	grpcClient := grpccli.NewClient(serverkit.Addr(dialHost, grpcPort),
		grpccli.WithDialOptions(grpclib.WithTransportCredentials(insecure.NewCredentials())),
	)
	dashboard := orders.NewDashboard(
		serverkit.MustHTTPClient("http://"+serverkit.Addr(dialHost, httpPort)),
		serverkit.MustHTTPClient("http://"+healthAddr),
		grpcClient,
	)
	dashboardSrv := serverkit.NewHTTPServer(bindHost, dashboardPort, "orders-service-dashboard", dashboard.Mux(), registry)

	healthOpts := make([]healthserver.Option, 0, 5+len(backend.Optional))
	healthOpts = append(healthOpts,
		healthserver.WithHost(bindHost),
		healthserver.WithPort(healthPort),
		healthserver.WithAppName("orders-service"),
		healthserver.WithProber(cleanup),
		healthserver.WithPrometheusRegisterer(registry),
	)
	for _, s := range backend.Optional {
		healthOpts = append(healthOpts, healthserver.WithProber(s))
	}
	healthSrv := healthserver.New(healthOpts...)

	svcs := make([]service.Service, 0, 7+len(backend.Optional))
	svcs = append(svcs,
		httpSrv,
		grpcSrv,
		healthSrv,
		dashboardSrv,
		grpcClient,
		processor.Pool(),
		cleanup.Service(cleanupInterval),
	)
	for _, s := range backend.Optional {
		svcs = append(svcs, s)
	}

	ep := entrypoint.New(
		entrypoint.WithServices(svcs...),
		entrypoint.WithShutdownTimeout(10*time.Second),
		entrypoint.WithPostStart(func(ctx context.Context) error {
			if err := backend.Prepare(ctx); err != nil {
				return err //nolint:wrapcheck // Prepare already wraps with context
			}
			slog.Info("orders-service ready",
				"dashboard", "http://"+serverkit.Addr(dialHost, dashboardPort),
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
