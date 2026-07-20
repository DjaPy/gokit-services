// Package redisservice provides a managed Redis client implementing
// service.Service, service.Shutdown, and service.Prober. Unlike
// http/server and grpc/server, Start retries with exponential backoff instead of
// failing fast, since Redis unavailability at process startup is typically
// a transient condition.
package redisservice

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"

	"github.com/DjaPy/gokit-services/internal/prom"
	"github.com/DjaPy/gokit-services/internal/retry"
)

// Service is a lifecycle-managed Redis client.
type Service struct {
	dsn             string
	poolSize        int
	appName         string
	retry           retry.Config
	metricsInterval time.Duration
	logger          *slog.Logger
	registerer      prometheus.Registerer

	mu           sync.RWMutex
	client       *redis.Client
	shutdownOnce sync.Once

	poolConns     *prometheus.GaugeVec
	poolHits      *prometheus.GaugeVec
	poolMisses    *prometheus.GaugeVec
	probeDuration *prometheus.HistogramVec
}

// Option configures a Service.
type Option func(*Service)

// WithPoolSize sets the connection pool size. Zero keeps the go-redis default.
func WithPoolSize(n int) Option {
	return func(s *Service) { s.poolSize = n }
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

// New creates a managed Redis client for the given DSN. The connection is
// established in Start, not New.
func New(dsn string, opts ...Option) *Service {
	s := &Service{
		dsn:             dsn,
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
			Name: "redis_pool_conns",
			Help: "Number of connections in the pool by state.",
		},
		[]string{"redis_service", "state"},
	))
	s.poolHits = prom.RegisterOrReuse(s.registerer, prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "redis_pool_hits",
			Help: "Cumulative number of free connections found in the pool (polled snapshot, not a true counter).",
		},
		[]string{"redis_service"},
	))
	s.poolMisses = prom.RegisterOrReuse(s.registerer, prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "redis_pool_misses",
			Help: "Cumulative number of times a free connection was not found (polled snapshot, not a true counter).",
		},
		[]string{"redis_service"},
	))
	s.probeDuration = prom.RegisterOrReuse(s.registerer, prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "redis_probe_duration_seconds",
			Help:    "The latency of readiness probe pings.",
			Buckets: []float64{.001, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5},
		},
		[]string{"redis_service"},
	))
}

// connectOnce makes a single connection attempt. redis.NewClient is lazy, so
// Ping is what actually detects an unreachable server; without it, retry.Do
// would never see a failure and the retry policy would be a no-op.
func connectOnce(ctx context.Context, opts *redis.Options) (*redis.Client, error) {
	c := redis.NewClient(opts)

	errPing := c.Ping(ctx).Err()
	if errPing == nil {
		return c, nil
	}

	if errClose := c.Close(); errClose != nil {
		return nil, fmt.Errorf("redisservice: ping: %w; close: %w", errPing, errClose)
	}
	return nil, fmt.Errorf("redisservice: ping: %w", errPing)
}

// Start establishes the Redis client, retrying with exponential backoff on
// failure, then blocks until ctx is canceled.
func (s *Service) Start(ctx context.Context) error {
	redisOpts, errParse := redis.ParseURL(s.dsn)
	if errParse != nil {
		return fmt.Errorf("redisservice: parse dsn: %w", errParse)
	}
	if s.poolSize > 0 {
		redisOpts.PoolSize = s.poolSize
	}

	client, errConnect := retry.Do(ctx, s.retry, func(attemptCtx context.Context) (*redis.Client, error) {
		return connectOnce(attemptCtx, redisOpts)
	})
	if errConnect != nil {
		return fmt.Errorf("redisservice: connect: %w", errConnect)
	}

	s.initMetrics()

	s.mu.Lock()
	s.client = client
	s.mu.Unlock()
	s.logger.Info("redisservice: connected")

	ticker := time.NewTicker(s.metricsInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			stats := client.PoolStats()
			s.poolConns.WithLabelValues(s.appName, "total").Set(float64(stats.TotalConns))
			s.poolConns.WithLabelValues(s.appName, "idle").Set(float64(stats.IdleConns))
			s.poolHits.WithLabelValues(s.appName).Set(float64(stats.Hits))
			s.poolMisses.WithLabelValues(s.appName).Set(float64(stats.Misses))
		}
	}
}

// Stop implements service.Shutdown. It closes the client. Repeated calls are safe.
func (s *Service) Stop(_ context.Context, _ error) error {
	s.mu.RLock()
	client := s.client
	s.mu.RUnlock()

	if client == nil {
		return nil
	}

	var errClose error
	s.shutdownOnce.Do(func() {
		if err := client.Close(); err != nil {
			errClose = fmt.Errorf("redisservice: stop: %w", err)
		}
	})
	return errClose
}

// Probe implements service.Prober. It pings Redis to verify connectivity.
// Returns an error if Start has not yet connected.
func (s *Service) Probe(ctx context.Context) error {
	s.mu.RLock()
	client := s.client
	s.mu.RUnlock()

	if client == nil {
		return errors.New("redisservice: not connected")
	}

	start := time.Now()
	err := client.Ping(ctx).Err()
	s.probeDuration.WithLabelValues(s.appName).Observe(time.Since(start).Seconds())
	if err != nil {
		return fmt.Errorf("redisservice: probe: %w", err)
	}
	return nil
}

// Client returns the underlying *redis.Client. Returns nil until Start has
// successfully connected.
func (s *Service) Client() *redis.Client {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.client
}
