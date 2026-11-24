package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	grpclib "google.golang.org/grpc"
	"google.golang.org/grpc/status"

	"github.com/DjaPy/gokit-services/pkg/internal/prom"
)

// Server wraps a *grpc.Server and implements service.Service and service.Shutdown.
// Use NewServer to construct, then register gRPC service implementations via GRPCServer().
type Server struct {
	host         string
	port         int
	appName      string
	srvOpts      []grpclib.ServerOption
	srv          *grpclib.Server
	logger       *slog.Logger
	registerer   prometheus.Registerer
	mu           sync.RWMutex
	listener     net.Listener
	shutdownOnce sync.Once

	handledTotal     *prometheus.CounterVec
	handlingDuration *prometheus.HistogramVec
	requestsInflight *prometheus.GaugeVec
}

// Option configures a Server.
type Option func(*Server)

// WithHost sets the listen host. Default: "0.0.0.0".
func WithHost(host string) Option {
	return func(s *Server) { s.host = host }
}

// WithPort sets the listen port. Default: 9090.
func WithPort(port int) Option {
	return func(s *Server) { s.port = port }
}

// WithLogger sets the logger.
func WithLogger(l *slog.Logger) Option {
	return func(s *Server) { s.logger = l }
}

// WithAppName sets the application name used in Prometheus metric labels.
func WithAppName(name string) Option {
	return func(s *Server) { s.appName = name }
}

// WithPrometheusRegisterer sets a custom Prometheus registerer.
func WithPrometheusRegisterer(r prometheus.Registerer) Option {
	return func(s *Server) { s.registerer = r }
}

// WithServerOptions adds gRPC server options (e.g. interceptors, credentials, keepalive params).
func WithServerOptions(opts ...grpclib.ServerOption) Option {
	return func(s *Server) { s.srvOpts = append(s.srvOpts, opts...) }
}

// NewServer creates a lifecycle-managed gRPC server.
// Register gRPC service implementations via GRPCServer() before calling Start.
func NewServer(opts ...Option) *Server {
	s := &Server{
		host:       "0.0.0.0",
		port:       9090,
		logger:     slog.Default(),
		registerer: prometheus.DefaultRegisterer,
	}
	for _, opt := range opts {
		opt(s)
	}
	s.initMetrics()
	srvOpts := append([]grpclib.ServerOption{
		grpclib.ChainUnaryInterceptor(s.unaryServerInterceptor()),
		grpclib.ChainStreamInterceptor(s.streamServerInterceptor()),
	}, s.srvOpts...)
	s.srv = grpclib.NewServer(srvOpts...)
	return s
}

func (s *Server) initMetrics() {
	s.handledTotal = prom.RegisterOrReuse(s.registerer, prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "grpc_server_handled_total",
			Help: "Total number of RPCs completed on the server, by method, type and status code.",
		},
		[]string{"grpc_service", "grpc_type", "grpc_method", "grpc_code"},
	))
	s.handlingDuration = prom.RegisterOrReuse(s.registerer, prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "grpc_server_handling_seconds",
			Help:    "The latency of RPCs handled by the server.",
			Buckets: []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10},
		},
		[]string{"grpc_service", "grpc_type", "grpc_method", "grpc_code"},
	))
	s.requestsInflight = prom.RegisterOrReuse(s.registerer, prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "grpc_server_requests_inflight",
			Help: "The number of RPCs currently being handled by the server.",
		},
		[]string{"grpc_service", "grpc_type", "grpc_method"},
	))
}

func (s *Server) unaryServerInterceptor() grpclib.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpclib.UnaryServerInfo, handler grpclib.UnaryHandler) (any, error) {
		const grpcType = "unary"
		s.requestsInflight.WithLabelValues(s.appName, grpcType, info.FullMethod).Inc()
		start := time.Now()

		resp, err := handler(ctx, req)

		code := status.Code(err).String()
		s.requestsInflight.WithLabelValues(s.appName, grpcType, info.FullMethod).Dec()
		s.handledTotal.WithLabelValues(s.appName, grpcType, info.FullMethod, code).Inc()
		s.handlingDuration.WithLabelValues(s.appName, grpcType, info.FullMethod, code).Observe(time.Since(start).Seconds())
		return resp, err
	}
}

func (s *Server) streamServerInterceptor() grpclib.StreamServerInterceptor {
	return func(srv any, ss grpclib.ServerStream, info *grpclib.StreamServerInfo, handler grpclib.StreamHandler) error {
		grpcType := streamType(info.IsClientStream, info.IsServerStream)
		s.requestsInflight.WithLabelValues(s.appName, grpcType, info.FullMethod).Inc()
		start := time.Now()

		err := handler(srv, ss)

		code := status.Code(err).String()
		s.requestsInflight.WithLabelValues(s.appName, grpcType, info.FullMethod).Dec()
		s.handledTotal.WithLabelValues(s.appName, grpcType, info.FullMethod, code).Inc()
		s.handlingDuration.WithLabelValues(s.appName, grpcType, info.FullMethod, code).Observe(time.Since(start).Seconds())
		return err
	}
}

// streamType maps the gRPC stream flags to a grpc_type label value.
func streamType(clientStream, serverStream bool) string {
	switch {
	case clientStream && serverStream:
		return "bidi_stream"
	case clientStream:
		return "client_stream"
	case serverStream:
		return "server_stream"
	default:
		return "unary"
	}
}

// GRPCServer returns the underlying *grpc.Server for registering gRPC service implementations.
// Must be called before Start.
func (s *Server) GRPCServer() *grpclib.Server {
	return s.srv
}

// Start listens on the configured address and serves gRPC traffic.
// It blocks until the context is canceled or the server stops.
func (s *Server) Start(ctx context.Context) error {
	return s.StartWithReady(ctx, func() {})
}

func (s *Server) StartWithReady(ctx context.Context, ready func()) error {
	ln, err := net.Listen("tcp", net.JoinHostPort(s.host, strconv.Itoa(s.port)))
	if err != nil {
		return fmt.Errorf("grpc server listen: %w", err)
	}
	s.mu.Lock()
	s.listener = ln
	s.mu.Unlock()
	s.logger.Info("gRPC server listening", slog.String("addr", ln.Addr().String()))

	quit := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		select {
		case <-ctx.Done():
			s.shutdownOnce.Do(func() { s.srv.GracefulStop() })
		case <-quit:
		}
	})

	ready()
	err = s.srv.Serve(ln)
	close(quit)
	wg.Wait()
	if err != nil && !errors.Is(err, grpclib.ErrServerStopped) {
		return fmt.Errorf("grpc server serve: %w", err)
	}
	return nil
}

// Stop implements service.Shutdown. It initiates a graceful shutdown; if ctx
// expires before all RPCs drain, it falls back to a forceful stop.
func (s *Server) Stop(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		s.srv.Stop()
		return fmt.Errorf("grpc server stop: %w", err)
	}

	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		s.shutdownOnce.Do(func() { s.srv.GracefulStop() })
		close(done)
	})
	select {
	case <-done:
		wg.Wait()
		return nil
	case <-ctx.Done():
		s.srv.Stop()
		wg.Wait()
		return fmt.Errorf("grpc server stop: %w", ctx.Err())
	}
}

// Addr returns the server's address. Before Start it returns the configured
// host:port; after Start it returns the actual bound address.
func (s *Server) Addr() string {
	s.mu.RLock()
	ln := s.listener
	s.mu.RUnlock()
	if ln != nil {
		return ln.Addr().String()
	}
	return net.JoinHostPort(s.host, strconv.Itoa(s.port))
}
