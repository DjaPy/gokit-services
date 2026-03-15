package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
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
	host         string
	port         int
	readTimeout  time.Duration
	writeTimeout time.Duration
	idleTimeout  time.Duration
	appName      string
	logger       *slog.Logger
	handler      http.Handler
	registerer   prometheus.Registerer

	server   *http.Server
	listener net.Listener

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

// WithWriteTimeout sets the HTTP write timeout. Default: 10s.
func WithWriteTimeout(d time.Duration) Option {
	return func(s *Server) { s.writeTimeout = d }
}

// WithIdleTimeout sets the HTTP idle timeout. Default: 120s.
func WithIdleTimeout(d time.Duration) Option {
	return func(s *Server) { s.idleTimeout = d }
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
//
// Typical usage with a code-generated handler:
//
//	mux := http.NewServeMux()
//	generated.HandlerFromMux(myImpl, mux)
//	srv := httpserver.NewServer(mux, httpserver.WithPort(8080), httpserver.WithAppName("my-svc"))
func NewServer(handler http.Handler, opts ...Option) *Server {
	s := &Server{
		host:         "0.0.0.0",
		port:         8080,
		readTimeout:  5 * time.Second,
		writeTimeout: 10 * time.Second,
		idleTimeout:  120 * time.Second,
		logger:       slog.Default(),
		handler:      handler,
		registerer:   prometheus.DefaultRegisterer,
	}
	for _, opt := range opts {
		opt(s)
	}
	s.initMetrics()
	s.server = &http.Server{
		Addr:         net.JoinHostPort(s.host, strconv.Itoa(s.port)),
		Handler:      s.buildMiddleware(),
		ReadTimeout:  s.readTimeout,
		WriteTimeout: s.writeTimeout,
		IdleTimeout:  s.idleTimeout,
	}
	return s
}

func (s *Server) initMetrics() {
	s.panicTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "http_panic_recovery_total",
			Help: "Total number of recovered panics.",
		},
		[]string{"http_service", "http_method", "http_handler"},
	)
	s.requestDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "http_request_duration_seconds",
			Help:    "The latency of the HTTP requests.",
			Buckets: []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10},
		},
		[]string{"http_service", "http_handler", "http_method", "http_code"},
	)
	s.requestsInflight = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "http_requests_inflight",
			Help: "The number of inflight requests being handled at the same time.",
		},
		[]string{"http_service", "http_handler"},
	)
	s.responseSizeBytes = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "http_response_size_bytes",
			Help:    "The size of the HTTP responses.",
			Buckets: []float64{100, 1_000, 10_000, 100_000, 1_000_000, 10_000_000, 100_000_000, 1_000_000_000},
		},
		[]string{"http_service", "http_handler", "http_method", "http_code"},
	)
	s.registerer.MustRegister(
		s.panicTotal,
		s.requestDuration,
		s.requestsInflight,
		s.responseSizeBytes,
	)
}

// responseWriter wraps http.ResponseWriter to capture the status code and response size.
type responseWriter struct {
	http.ResponseWriter
	statusCode int
	size       int
}

func (rw *responseWriter) WriteHeader(code int) {
	rw.statusCode = code
	rw.ResponseWriter.WriteHeader(code)
}

func (rw *responseWriter) Write(b []byte) (int, error) {
	n, err := rw.ResponseWriter.Write(b)
	rw.size += n
	if err != nil {
		return 0, fmt.Errorf("error writing response: %w", err)
	}
	return n, nil
}

// buildMiddleware wraps the user handler with metrics collection and panic recovery.
func (s *Server) buildMiddleware() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pattern := r.Pattern

		rw := &responseWriter{ResponseWriter: w, statusCode: http.StatusOK}
		s.requestsInflight.WithLabelValues(s.appName, pattern).Inc()
		start := time.Now()

		defer func() {
			if rec := recover(); rec != nil {
				s.logger.Error("panic recovered",
					slog.Any("panic", rec),
					slog.String("stack", string(debug.Stack())),
				)
				s.panicTotal.WithLabelValues(s.appName, r.Method, pattern).Inc()
				if rw.statusCode == http.StatusOK {
					WriteProblem(rw, &Problem{
						Title:    http.StatusText(http.StatusInternalServerError),
						Status:   http.StatusInternalServerError,
						Instance: r.URL.Path,
					})
				}
			}

			dur := time.Since(start).Seconds()
			code := strconv.Itoa(rw.statusCode)
			s.requestDuration.WithLabelValues(s.appName, pattern, r.Method, code).Observe(dur)
			s.responseSizeBytes.WithLabelValues(s.appName, pattern, r.Method, code).Observe(float64(rw.size))
			s.requestsInflight.WithLabelValues(s.appName, pattern).Dec()
		}()

		s.handler.ServeHTTP(rw, r)
	})
}

// WriteProblem writes an RFC 7807 Problem Details response.
func WriteProblem(w http.ResponseWriter, p *Problem) {
	w.Header().Set("Content-Type", problemContentType)
	w.WriteHeader(p.Status)
	_ = json.NewEncoder(w).Encode(p)
}

// Addr returns the address the server is listening on.
// Before Start is called, it returns the configured host:port.
// After Start, it returns the actual bound address (useful when port is 0).
func (s *Server) Addr() string {
	if s.listener != nil {
		return s.listener.Addr().String()
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
	s.listener = ln

	s.logger.Info("HTTP server started", slog.String("addr", s.Addr()))

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.server.Shutdown(shutdownCtx)
	}()

	if err := s.server.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serving: %w", err)
	}
	return nil
}

// Stop implements service.Stopper. It gracefully shuts down the server within the given ctx deadline.
func (s *Server) Stop(ctx context.Context, _ error) error {
	s.logger.Info("HTTP server stopping")
	if err := s.server.Shutdown(ctx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	return nil
}
