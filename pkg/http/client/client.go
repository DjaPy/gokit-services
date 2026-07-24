package client

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"time"
)

type lengther interface {
	Len() int
}

// RoundTripperMiddleware wraps an http.RoundTripper to add cross-cutting behavior
// such as logging, tracing, or header injection.
type RoundTripperMiddleware func(http.RoundTripper) http.RoundTripper

// RoundTripFunc is a function that implements http.RoundTripper.
// Useful for creating inline middleware without defining a new type.
type RoundTripFunc func(*http.Request) (*http.Response, error)

func (f RoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Client is a base HTTP client with a fixed base URL and timeout.
// Safe for concurrent use.
type Client struct {
	baseURL     *url.URL
	http        *http.Client
	middlewares []RoundTripperMiddleware
	logger      *slog.Logger

	// transport config — used to build http.Transport when no custom transport is set
	maxIdleConns        int
	maxIdleConnsPerHost int
	idleConnTimeout     time.Duration
	tlsHandshakeTimeout time.Duration
}

// Option configures a Client at construction time.
type Option func(*Client)

// WithTimeout sets the total request timeout. Default: 30s.
func WithTimeout(d time.Duration) Option {
	return func(c *Client) { c.http.Timeout = d }
}

// WithTransport replaces the transport entirely with the given RoundTripper.
// When set, transport config options (WithMaxIdleConns etc.) have no effect.
func WithTransport(t http.RoundTripper) Option {
	return func(c *Client) { c.http.Transport = t }
}

// WithMiddleware adds middleware to the client.
// First middleware is outermost — wrapping the final transport.
func WithMiddleware(middlewares ...RoundTripperMiddleware) Option {
	return func(c *Client) { c.middlewares = append(c.middlewares, middlewares...) }
}

// WithMaxIdleConns sets the maximum number of idle connections across all hosts. Default: 100.
func WithMaxIdleConns(n int) Option {
	return func(c *Client) { c.maxIdleConns = n }
}

// WithMaxIdleConnsPerHost sets the maximum idle connections per host. Default: 10.
func WithMaxIdleConnsPerHost(n int) Option {
	return func(c *Client) { c.maxIdleConnsPerHost = n }
}

// WithIdleConnTimeout sets the idle connection timeout. Default: 90s.
func WithIdleConnTimeout(d time.Duration) Option {
	return func(c *Client) { c.idleConnTimeout = d }
}

// WithTLSHandshakeTimeout sets the TLS handshake timeout. Default: 10s.
func WithTLSHandshakeTimeout(d time.Duration) Option {
	return func(c *Client) { c.tlsHandshakeTimeout = d }
}

// WithLogger sets the logger. Default: slog.Default().
func WithLogger(l *slog.Logger) Option {
	return func(c *Client) { c.logger = l }
}

// New creates a new Client with the given base URL.
// All transport parameters have defaults and can be overridden via options.
func New(baseURL string, opts ...Option) (*Client, error) {
	base, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("parsing base URL: %w", err)
	}

	c := &Client{
		baseURL:             base,
		http:                &http.Client{Timeout: 30 * time.Second},
		logger:              slog.Default(),
		maxIdleConns:        100,
		maxIdleConnsPerHost: 10,
		idleConnTimeout:     90 * time.Second,
		tlsHandshakeTimeout: 10 * time.Second,
	}

	for _, opt := range opts {
		opt(c)
	}

	// Build transport from config only when no custom transport was provided via WithTransport.
	if c.http.Transport == nil {
		c.http.Transport = &http.Transport{
			MaxIdleConns:        c.maxIdleConns,
			MaxIdleConnsPerHost: c.maxIdleConnsPerHost,
			IdleConnTimeout:     c.idleConnTimeout,
			TLSHandshakeTimeout: c.tlsHandshakeTimeout,
		}
	}

	if len(c.middlewares) > 0 {
		transport := c.http.Transport
		for i := len(c.middlewares) - 1; i >= 0; i-- {
			transport = c.middlewares[i](transport)
		}
		c.http.Transport = transport
	}

	return c, nil
}

// URL builds a full URL by joining the base URL with the given path segments.
func (c *Client) URL(p string) string {
	return c.baseURL.JoinPath(p).String()
}

// RequestOption configures an outgoing HTTP request.
type RequestOption func(*http.Request)

// WithHeader adds a header to the request.
func WithHeader(key, value string) RequestOption {
	return func(r *http.Request) { r.Header.Set(key, value) }
}

// WithBody sets the request body and content type.
func WithBody(body io.Reader, contentType string) RequestOption {
	return func(r *http.Request) {
		rc, ok := body.(io.ReadCloser)
		if !ok {
			rc = io.NopCloser(body)
		}
		r.Body = rc
		if l, ok := body.(lengther); ok {
			r.ContentLength = int64(l.Len())
		}
		if contentType != "" {
			r.Header.Set("Content-Type", contentType)
		}
	}
}

// Do sends an HTTP request and decodes the JSON response body into T.
// Returns an error if the status code is not 2xx.
func Do[T any](ctx context.Context, c *Client, method, p string, opts ...RequestOption) (T, error) {
	var zero T

	req, err := http.NewRequestWithContext(ctx, method, c.URL(p), http.NoBody)
	if err != nil {
		return zero, fmt.Errorf("creating request: %w", err)
	}

	for _, opt := range opts {
		opt(req)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return zero, fmt.Errorf("sending request: %w", err)
	}
	defer func() {
		if errCl := resp.Body.Close(); errCl != nil {
			c.logger.Error("closing response body", slog.String("error", errCl.Error()))
		}
	}()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return zero, fmt.Errorf("reading response body: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return zero, fmt.Errorf("unexpected status %d %s: %s",
			resp.StatusCode, http.StatusText(resp.StatusCode), body)
	}

	if len(body) == 0 {
		return zero, nil
	}

	if err := json.Unmarshal(body, &zero); err != nil {
		return zero, fmt.Errorf("decoding response: %w", err)
	}

	return zero, nil
}
