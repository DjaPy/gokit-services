package httpserver

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"strconv"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/DjaPy/gokit-services/internal/prom"
)

// Problem represents an RFC 7807 Problem Details response.
// See https://datatracker.ietf.org/doc/html/rfc7807
type Problem struct {
	Type          string `json:"type,omitempty"`
	Title         string `json:"title,omitempty"`
	Status        int    `json:"status,omitempty"`
	Detail        any    `json:"detail,omitempty"`
	Instance      string `json:"instance,omitempty"`
	InvalidParams any    `json:"invalid-params,omitempty"`
}

const problemContentType = "application/problem+json"

// Server is an HTTP server implementing service.Service and service.Stopper.
type Server struct {
	host              string
	port              int
	readTimeout       time.Duration
	readHeaderTimeout time.Duration
	writeTimeout      time.Duration
	idleTimeout       time.Duration
	maxHeaderBytes    int
	appName           string
	logger            *slog.Logger
	handler           http.Handler
	registerer        prometheus.Registerer

	mu           sync.RWMutex
	server       *http.Server
	listener     net.Listener
	shutdownOnce sync.Once

	panicTotal        *prometheus.CounterVec
	requestDuration   *prometheus.HistogramVec
	requestsInflight  *prometheus.GaugeVec
	responseSizeBytes *prometheus.HistogramVec
}

// Option configures a Server.
type Option func(*Server)

// WithHost sets the bind host. Default: "0.0.0.0".
func WithHost(host string) Option {
	return func(s *Server) { s.host = host }
}

// WithPort sets the bind port. Default: 8080.
func WithPort(port int) Option {
	return func(s *Server) { s.port = port }
}

// WithReadTimeout sets the HTTP read timeout. Default: 5s.
func WithReadTimeout(d time.Duration) Option {
	return func(s *Server) { s.readTimeout = d }
}

// WithReadHeaderTimeout sets the HTTP read header timeout. Default: 5s.
func WithReadHeaderTimeout(d time.Duration) Option {
	return func(s *Server) { s.readHeaderTimeout = d }
}

// WithWriteTimeout sets the HTTP write timeout. Default: 10s.
func WithWriteTimeout(d time.Duration) Option {
	return func(s *Server) { s.writeTimeout = d }
}

// WithIdleTimeout sets the HTTP idle timeout. Default: 120s.
func WithIdleTimeout(d time.Duration) Option {
	return func(s *Server) { s.idleTimeout = d }
}

// WithMaxHeaderBytes sets the maximum size of request headers in bytes. Default: 0 (uses net/http default of 1MB).
func WithMaxHeaderBytes(n int) Option {
	return func(s *Server) { s.maxHeaderBytes = n }
}

// WithAppName sets the application name used in Prometheus metric labels.
func WithAppName(name string) Option {
	return func(s *Server) { s.appName = name }
}

// WithLogger sets the logger used by the server.
func WithLogger(l *slog.Logger) Option {
	return func(s *Server) { s.logger = l }
}

// WithPrometheusRegisterer sets a custom Prometheus registerer.
// Default: prometheus.DefaultRegisterer.
func WithPrometheusRegisterer(r prometheus.Registerer) Option {
	return func(s *Server) { s.registerer = r }
}

// NewServer creates an HTTP Server wrapping the given handler with Prometheus metrics
// middleware and panic recovery.
func NewServer(handler http.Handler, opts ...Option) *Server {
	s := &Server{
		host:              "0.0.0.0",
		port:              8080,
		readTimeout:       5 * time.Second,
		readHeaderTimeout: 5 * time.Second,
		writeTimeout:      10 * time.Second,
		idleTimeout:       120 * time.Second,
		logger:            slog.Default(),
		handler:           handler,
		registerer:        prometheus.DefaultRegisterer,
	}
	for _, opt := range opts {
		opt(s)
	}
	s.initMetrics()
	s.server = &http.Server{
		Addr:              net.JoinHostPort(s.host, strconv.Itoa(s.port)),
		Handler:           s.buildMiddleware(),
		ReadTimeout:       s.readTimeout,
		ReadHeaderTimeout: s.readHeaderTimeout,
		WriteTimeout:      s.writeTimeout,
		IdleTimeout:       s.idleTimeout,
		MaxHeaderBytes:    s.maxHeaderBytes,
	}
	return s
}

func (s *Server) initMetrics() {
	s.panicTotal = prom.RegisterOrReuse(s.registerer, prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "http_panic_recovery_total",
			Help: "Total number of recovered panics.",
		},
		[]string{"http_service", "http_method", "http_handler"},
	))
	s.requestDuration = prom.RegisterOrReuse(s.registerer, prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "http_request_duration_seconds",
			Help:    "The latency of the HTTP requests.",
			Buckets: []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10},
		},
		[]string{"http_service", "http_handler", "http_method", "http_code"},
	))
	s.requestsInflight = prom.RegisterOrReuse(s.registerer, prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "http_requests_inflight",
			Help: "The number of inflight requests being handled at the same time.",
		},
		[]string{"http_service", "http_handler"},
	))
	s.responseSizeBytes = prom.RegisterOrReuse(s.registerer, prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "http_response_size_bytes",
			Help:    "The size of the HTTP responses.",
			Buckets: []float64{100, 1_000, 10_000, 100_000, 1_000_000, 10_000_000, 100_000_000, 1_000_000_000},
		},
		[]string{"http_service", "http_handler", "http_method", "http_code"},
	))
}

// responseWriter wraps http.ResponseWriter to capture the status code and response size.
type responseWriter struct {
	http.ResponseWriter
	statusCode  int
	size        int
	wroteHeader bool
}

func (rw *responseWriter) WriteHeader(code int) {
	if !rw.wroteHeader {
		rw.statusCode = code
		rw.wroteHeader = true
	}
	rw.ResponseWriter.WriteHeader(code)
}

func (rw *responseWriter) Write(b []byte) (int, error) {
	if !rw.wroteHeader {
		rw.wroteHeader = true
	}
	n, err := rw.ResponseWriter.Write(b)
	rw.size += n
	if err != nil {
		return n, fmt.Errorf("responseWriter write: %w", err)
	}
	return n, nil
}

func (rw *responseWriter) Flush() {
	if f, ok := rw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (rw *responseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := rw.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("responseWriter: underlying ResponseWriter does not implement http.Hijacker")
	}
	conn, rw2, err := h.Hijack()
	if err != nil {
		return nil, nil, fmt.Errorf("responseWriter hijack: %w", err)
	}
	return conn, rw2, nil
}

// buildMiddleware wraps the user handler with metrics collection and panic recovery.
func (s *Server) buildMiddleware() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inflightPattern := r.Pattern

		rw := &responseWriter{ResponseWriter: w, statusCode: http.StatusOK}
		s.requestsInflight.WithLabelValues(s.appName, inflightPattern).Inc()
		start := time.Now()

		defer func() {
			pattern := r.Pattern

			if rec := recover(); rec != nil {
				s.logger.Error("panic recovered",
					slog.Any("panic", rec),
					slog.String("stack", string(debug.Stack())),
				)
				s.panicTotal.WithLabelValues(s.appName, r.Method, pattern).Inc()
				if !rw.wroteHeader {
					if err := WriteProblem(rw, &Problem{
						Title:    http.StatusText(http.StatusInternalServerError),
						Status:   http.StatusInternalServerError,
						Instance: r.URL.Path,
					}); err != nil {
						s.logger.Error("writing panic problem response", slog.Any("error", err))
					}
				}
			}

			dur := time.Since(start).Seconds()
			code := strconv.Itoa(rw.statusCode)
			s.requestDuration.WithLabelValues(s.appName, pattern, r.Method, code).Observe(dur)
			s.responseSizeBytes.WithLabelValues(s.appName, pattern, r.Method, code).Observe(float64(rw.size))
			s.requestsInflight.WithLabelValues(s.appName, inflightPattern).Dec()
		}()

		s.handler.ServeHTTP(rw, r)
	})
}

// WriteProblem writes an RFC 7807 Problem Details response.
func WriteProblem(w http.ResponseWriter, p *Problem) error {
	w.Header().Set("Content-Type", problemContentType)
	w.WriteHeader(p.Status)
	if err := json.NewEncoder(w).Encode(p); err != nil {
		return fmt.Errorf("write problem: %w", err)
	}
	return nil
}

// Addr returns the address the server is listening on.
// Before Start is called, it returns the configured host:port.
// After Start, it returns the actual bound address (useful when port is 0).
func (s *Server) Addr() string {
	s.mu.RLock()
	ln := s.listener
	s.mu.RUnlock()
	if ln != nil {
		return ln.Addr().String()
	}
	return s.server.Addr
}

// Start implements service.Service. It binds and serves until the server is shut down.
// When ctx is canceled, the server is shut down gracefully.
func (s *Server) Start(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.server.Addr)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", s.server.Addr, err)
	}
	s.mu.Lock()
	s.listener = ln
	s.mu.Unlock()

	s.logger.Info("HTTP server started", slog.String("addr", s.Addr()))

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		s.shutdownOnce.Do(func() {
			if err := s.server.Shutdown(shutdownCtx); err != nil {
				s.logger.Error("shutdown on context cancel", slog.String("error", err.Error()))
			}
		})
	}()

	if err := s.server.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serving: %w", err)
	}
	return nil
}

// Stop implements service.Stopper. It gracefully shuts down the server within the given ctx deadline.
func (s *Server) Stop(ctx context.Context, _ error) error {
	s.logger.Info("HTTP server stopping")
	var err error
	s.shutdownOnce.Do(func() {
		if shutdownErr := s.server.Shutdown(ctx); shutdownErr != nil {
			err = fmt.Errorf("shutdown: %w", shutdownErr)
		}
	})
	return err
}
