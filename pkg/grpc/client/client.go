package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	grpclib "google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/DjaPy/gokit-services/pkg/internal/prom"
)

// Client is a lifecycle-managed gRPC client. The connection is established in
// Start and closed by Stop. Conn() returns nil until Start is called.
type Client struct {
	target     string
	appName    string
	dialOpts   []grpclib.DialOption
	mu         sync.RWMutex
	conn       *grpclib.ClientConn
	logger     *slog.Logger
	registerer prometheus.Registerer

	handledTotal     *prometheus.CounterVec
	handlingDuration *prometheus.HistogramVec
	requestsInflight *prometheus.GaugeVec
}

// Option configures a Client.
type Option func(*Client)

// WithDialOptions appends gRPC dial options (e.g. credentials, interceptors).
func WithDialOptions(opts ...grpclib.DialOption) Option {
	return func(c *Client) { c.dialOpts = append(c.dialOpts, opts...) }
}

// WithLogger sets the logger.
func WithLogger(l *slog.Logger) Option {
	return func(c *Client) { c.logger = l }
}

// WithAppName sets the application name used in Prometheus metric labels.
func WithAppName(name string) Option {
	return func(c *Client) { c.appName = name }
}

// WithPrometheusRegisterer sets a custom Prometheus registerer.
func WithPrometheusRegisterer(r prometheus.Registerer) Option {
	return func(c *Client) { c.registerer = r }
}

// NewClient constructs a Client that will connect to target when Started.
func NewClient(target string, opts ...Option) *Client {
	c := &Client{
		target:     target,
		logger:     slog.Default(),
		registerer: prometheus.DefaultRegisterer,
	}
	for _, opt := range opts {
		opt(c)
	}
	c.initMetrics()
	c.dialOpts = append([]grpclib.DialOption{
		grpclib.WithChainUnaryInterceptor(c.unaryClientInterceptor()),
		grpclib.WithChainStreamInterceptor(c.streamClientInterceptor()),
	}, c.dialOpts...)
	return c
}

func (c *Client) initMetrics() {
	c.handledTotal = prom.RegisterOrReuse(c.registerer, prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "grpc_client_handled_total",
			Help: "Total number of RPCs completed by the client, by method, type and status code.",
		},
		[]string{"grpc_service", "grpc_type", "grpc_method", "grpc_code"},
	))
	c.handlingDuration = prom.RegisterOrReuse(c.registerer, prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "grpc_client_handling_seconds",
			Help:    "The latency of RPCs issued by the client.",
			Buckets: []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10},
		},
		[]string{"grpc_service", "grpc_type", "grpc_method", "grpc_code"},
	))
	c.requestsInflight = prom.RegisterOrReuse(c.registerer, prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "grpc_client_requests_inflight",
			Help: "The number of RPCs currently in flight from the client.",
		},
		[]string{"grpc_service", "grpc_type", "grpc_method"},
	))
}

func (c *Client) unaryClientInterceptor() grpclib.UnaryClientInterceptor {
	return func(
		ctx context.Context,
		method string,
		req, reply any,
		cc *grpclib.ClientConn,
		invoker grpclib.UnaryInvoker,
		opts ...grpclib.CallOption,
	) error {
		const grpcType = "unary"
		c.requestsInflight.WithLabelValues(c.appName, grpcType, method).Inc()
		start := time.Now()

		err := invoker(ctx, method, req, reply, cc, opts...)

		code := status.Code(err).String()
		c.requestsInflight.WithLabelValues(c.appName, grpcType, method).Dec()
		c.handledTotal.WithLabelValues(c.appName, grpcType, method, code).Inc()
		c.handlingDuration.WithLabelValues(c.appName, grpcType, method, code).Observe(time.Since(start).Seconds())
		return err
	}
}

func (c *Client) streamClientInterceptor() grpclib.StreamClientInterceptor {
	return func(
		ctx context.Context,
		desc *grpclib.StreamDesc,
		cc *grpclib.ClientConn,
		method string,
		streamer grpclib.Streamer,
		opts ...grpclib.CallOption,
	) (grpclib.ClientStream, error) {
		grpcType := streamType(desc.ClientStreams, desc.ServerStreams)
		c.requestsInflight.WithLabelValues(c.appName, grpcType, method).Inc()
		start := time.Now()

		clientStream, err := streamer(ctx, desc, cc, method, opts...)
		if err != nil {
			code := status.Code(err).String()
			c.requestsInflight.WithLabelValues(c.appName, grpcType, method).Dec()
			c.handledTotal.WithLabelValues(c.appName, grpcType, method, code).Inc()
			c.handlingDuration.WithLabelValues(c.appName, grpcType, method, code).Observe(time.Since(start).Seconds())
			return nil, err
		}

		return &monitoredClientStream{
			ClientStream: clientStream,
			finish: func(streamErr error) {
				code := streamStatusCode(streamErr).String()
				c.requestsInflight.WithLabelValues(c.appName, grpcType, method).Dec()
				c.handledTotal.WithLabelValues(c.appName, grpcType, method, code).Inc()
				c.handlingDuration.WithLabelValues(c.appName, grpcType, method, code).Observe(time.Since(start).Seconds())
			},
		}, nil
	}
}

// streamType maps the gRPC stream flags to a metric label value: unary,
// client_stream, server_stream, or bidi_stream.
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

// streamStatusCode resolves the terminal status of a client stream. A stream
// that drains cleanly reports io.EOF, which maps to codes.OK.
func streamStatusCode(err error) codes.Code {
	if err == nil || errors.Is(err, io.EOF) {
		return codes.OK
	}
	return status.Code(err)
}

// monitoredClientStream wraps a grpc.ClientStream to record metrics exactly once
type monitoredClientStream struct {
	grpclib.ClientStream
	finish   func(error)
	finished sync.Once
}

func (s *monitoredClientStream) RecvMsg(m any) error {
	err := s.ClientStream.RecvMsg(m)
	if err != nil {
		s.finished.Do(func() { s.finish(err) })
	}
	//nolint:wrapcheck // transparent passthrough: callers rely on errors.Is(err, io.EOF) and status.FromError(err)
	return err
}

// Start establishes the gRPC connection and blocks until ctx is canceled.
// The connection is NOT closed by Start — call Stop for cleanup.
func (c *Client) Start(ctx context.Context) error {
	conn, err := grpclib.NewClient(c.target, c.dialOpts...)
	if err != nil {
		return fmt.Errorf("grpc client: %w", err)
	}
	c.mu.Lock()
	c.conn = conn
	c.mu.Unlock()
	c.logger.Info("gRPC client connected", slog.String("target", c.target))

	<-ctx.Done()
	return nil
}

// Stop implements service.Shutdown. It closes the connection; if ctx expires
// before Close returns, Stop returns ctx.Err() wrapped.
func (c *Client) Stop(ctx context.Context) error {
	c.mu.RLock()
	conn := c.conn
	c.mu.RUnlock()

	if conn == nil {
		return nil
	}

	done := make(chan error, 1)
	go func() { done <- conn.Close() }()

	select {
	case err := <-done:
		if err != nil {
			return fmt.Errorf("grpc client close: %w", err)
		}
		return nil
	case <-ctx.Done():
		return fmt.Errorf("grpc client close: %w", ctx.Err())
	}
}

// Conn returns the underlying *grpc.ClientConn. Returns nil until Start has been called.
func (c *Client) Conn() *grpclib.ClientConn {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.conn
}
