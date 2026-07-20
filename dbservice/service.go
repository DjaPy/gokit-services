// Package dbservice provides a managed PostgreSQL connection pool
// implementing service.Service, service.Shutdown, and service.Prober.
// Unlike http/server and grpc/server, Start retries with exponential backoff
// instead of failing fast, since database unavailability at process
// startup is typically a transient condition.
package dbservice

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/DjaPy/gokit-services/internal/prom"
	"github.com/DjaPy/gokit-services/internal/retry"
)

// Service is a lifecycle-managed PostgreSQL connection pool.
type Service struct {
	dsn             string
	maxConns        int32
	minConns        int32
	appName         string
	retry           retry.Config
	metricsInterval time.Duration
	logger          *slog.Logger
	registerer      prometheus.Registerer

	mu           sync.RWMutex
	pool         *pgxpool.Pool
	shutdownOnce sync.Once

	poolConns      *prometheus.GaugeVec
	poolMaxConns   *prometheus.GaugeVec
	poolAcquireCnt *prometheus.GaugeVec
	probeDuration  *prometheus.HistogramVec
}

// Option configures a Service.
type Option func(*Service)

func WithMaxConns(n int32) Option {
	return func(s *Service) { s.maxConns = n }
}

func WithMinConns(n int32) Option {
	return func(s *Service) { s.minConns = n }
}

func WithRetry(cfg retry.Config) Option {
	return func(s *Service) { s.retry = cfg }
}

func WithMetricsInterval(d time.Duration) Option {
	return func(s *Service) { s.metricsInterval = d }
}

func WithAppName(name string) Option {
	return func(s *Service) { s.appName = name }
}

func WithLogger(l *slog.Logger) Option {
	return func(s *Service) { s.logger = l }
}

func WithPrometheusRegisterer(r prometheus.Registerer) Option {
	return func(s *Service) { s.registerer = r }
}

// New creates a managed PostgreSQL connection pool for the given DSN.
// The connection is established in Start, not New.
func New(dsn string, opts ...Option) *Service {
	s := &Service{
		dsn:             dsn,
		maxConns:        10,
		minConns:        2,
		retry:           retry.DefaultConfig(),
		metricsInterval: 15 * time.Second,
		logger:          slog.Default(),
		registerer:      prometheus.DefaultRegisterer,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

func (s *Service) initMetrics() {
	s.poolConns = prom.RegisterOrReuse(s.registerer, prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "db_pool_conns",
			Help: "Number of connections in the pool by state.",
		},
		[]string{"db_service", "state"},
	))
	s.poolMaxConns = prom.RegisterOrReuse(s.registerer, prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "db_pool_max_conns",
			Help: "Maximum size of the connection pool.",
		},
		[]string{"db_service"},
	))
	s.poolAcquireCnt = prom.RegisterOrReuse(s.registerer, prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "db_pool_acquire_count",
			Help: "Cumulative number of successful connection acquires (polled snapshot, not a true counter).",
		},
		[]string{"db_service"},
	))
	s.probeDuration = prom.RegisterOrReuse(s.registerer, prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "db_probe_duration_seconds",
			Help:    "The latency of readiness probe pings.",
			Buckets: []float64{.001, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5},
		},
		[]string{"db_service"},
	))
}

// connectOnce makes a single connection attempt. pgxpool.NewWithConfig never
// dials the database itself — it only validates cfg and returns immediately
// — so Ping is what actually detects an unreachable database; without it,
// retry.Do would never see a failure and the retry policy would be a no-op.
func connectOnce(ctx context.Context, cfg *pgxpool.Config) (*pgxpool.Pool, error) {
	p, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("dbservice: create pool: %w", err)
	}
	if err := p.Ping(ctx); err != nil {
		p.Close()
		return nil, fmt.Errorf("dbservice: ping: %w", err)
	}
	return p, nil
}

// Start establishes the connection pool, retrying with exponential backoff
// on failure, then blocks until ctx is canceled. Unlike http/server and grpc/server,
// it does not fail fast — database unavailability at startup is treated as
// a transient condition bounded by the retry policy and ctx.
func (s *Service) Start(ctx context.Context) error {
	cfg, errConfig := pgxpool.ParseConfig(s.dsn)
	if errConfig != nil {
		return fmt.Errorf("dbservice: parse dsn: %w", errConfig)
	}
	cfg.MaxConns = s.maxConns
	cfg.MinConns = s.minConns

	pool, errPool := retry.Do(ctx, s.retry, func(attemptCtx context.Context) (*pgxpool.Pool, error) {
		return connectOnce(attemptCtx, cfg)
	})
	if errPool != nil {
		return fmt.Errorf("dbservice: connect: %w", errPool)
	}

	s.initMetrics()

	s.mu.Lock()
	s.pool = pool
	s.mu.Unlock()
	s.logger.Info("dbservice: connected")

	ticker := time.NewTicker(s.metricsInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			stat := pool.Stat()
			s.poolConns.WithLabelValues(s.appName, "total").Set(float64(stat.TotalConns()))
			s.poolConns.WithLabelValues(s.appName, "idle").Set(float64(stat.IdleConns()))
			s.poolConns.WithLabelValues(s.appName, "acquired").Set(float64(stat.AcquiredConns()))
			s.poolConns.WithLabelValues(s.appName, "constructing").Set(float64(stat.ConstructingConns()))
			s.poolMaxConns.WithLabelValues(s.appName).Set(float64(stat.MaxConns()))
			s.poolAcquireCnt.WithLabelValues(s.appName).Set(float64(stat.AcquireCount()))
		}
	}
}

// Stop implements service.Shutdown. It closes the connection pool, bounded
// by ctx. pgxpool.Pool.Close has no context-aware variant, so on ctx
// expiry Stop returns ctx.Err() while Close keeps running in the
// background until the pool actually drains.
func (s *Service) Stop(ctx context.Context, _ error) error {
	s.mu.RLock()
	pool := s.pool
	s.mu.RUnlock()

	if pool == nil {
		return nil
	}

	done := make(chan struct{})
	go func() {
		s.shutdownOnce.Do(pool.Close)
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("dbservice: stop: %w", ctx.Err())
	}
}

// Probe implements service.Prober. It pings the database to verify
// connectivity. Returns an error if Start has not yet established a pool.
func (s *Service) Probe(ctx context.Context) error {
	s.mu.RLock()
	pool := s.pool
	s.mu.RUnlock()

	if pool == nil {
		return errors.New("dbservice: not connected")
	}

	start := time.Now()
	err := pool.Ping(ctx)
	s.probeDuration.WithLabelValues(s.appName).Observe(time.Since(start).Seconds())
	if err != nil {
		return fmt.Errorf("dbservice: probe: %w", err)
	}
	return nil
}

// Pool returns the underlying *pgxpool.Pool. Returns nil until Start has
// successfully connected.
func (s *Service) Pool() *pgxpool.Pool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.pool
}
