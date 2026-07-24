package server

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"sync"

	grpclib "google.golang.org/grpc"
)

// Server wraps a *grpc.Server and implements service.Service and service.Shutdown.
// Use NewServer to construct, then register gRPC service implementations via GRPCServer().
type Server struct {
	host         string
	port         int
	srvOpts      []grpclib.ServerOption
	srv          *grpclib.Server
	logger       *slog.Logger
	mu           sync.RWMutex
	listener     net.Listener
	shutdownOnce sync.Once
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

// WithServerOptions adds gRPC server options (e.g. interceptors, credentials, keepalive params).
func WithServerOptions(opts ...grpclib.ServerOption) Option {
	return func(s *Server) { s.srvOpts = append(s.srvOpts, opts...) }
}

// NewServer creates a lifecycle-managed gRPC server.
// Register gRPC service implementations via GRPCServer() before calling Start.
func NewServer(opts ...Option) *Server {
	s := &Server{
		host:   "0.0.0.0",
		port:   9090,
		logger: slog.Default(),
	}
	for _, opt := range opts {
		opt(s)
	}
	s.srv = grpclib.NewServer(s.srvOpts...)
	return s
}

// GRPCServer returns the underlying *grpc.Server for registering gRPC service implementations.
// Must be called before Start.
func (s *Server) GRPCServer() *grpclib.Server {
	return s.srv
}

// Start listens on the configured address and serves gRPC traffic.
// It blocks until the context is canceled or the server stops.
func (s *Server) Start(ctx context.Context) error {
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

	if err := s.srv.Serve(ln); err != nil {
		s.logger.Error("gRPC server error", slog.Any("error", err))
	}
	close(quit)
	wg.Wait()
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
