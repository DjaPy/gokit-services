package grpcclient

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	grpclib "google.golang.org/grpc"
)

// Client is a lifecycle-managed gRPC client. The connection is established in
// Start and closed by Stop. Conn() returns nil until Start is called.
type Client struct {
	target   string
	dialOpts []grpclib.DialOption
	mu       sync.RWMutex
	conn     *grpclib.ClientConn
	logger   *slog.Logger
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

// NewClient constructs a Client that will connect to target when Started.
func NewClient(target string, opts ...Option) *Client {
	c := &Client{
		target: target,
		logger: slog.Default(),
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
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
func (c *Client) Stop(ctx context.Context, _ error) error {
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
