package healthserver

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/DjaPy/gokit-services/core/service"
	httpsrv "github.com/DjaPy/gokit-services/http/server"
)

// Server is an HTTP server exposing /healthz (liveness) and /readyz (readiness).
// All traffic is delegated to an internal httpsrv.Server.
type Server struct {
	probers []service.Prober
	logger  *slog.Logger
	inner   *httpsrv.Server
}

type config struct {
	port       int
	host       string
	appName    string
	registerer prometheus.Registerer
	logger     *slog.Logger
	probers    []service.Prober
}

// Option configures a Server.
type Option func(*config)

// WithProber adds a readiness prober.
func WithProber(p service.Prober) Option {
	return func(c *config) { c.probers = append(c.probers, p) }
}

// WithPort sets the listen port.
func WithPort(port int) Option {
	return func(c *config) { c.port = port }
}

// WithHost sets the listen host.
func WithHost(host string) Option {
	return func(c *config) { c.host = host }
}

// WithAppName sets the application name used in Prometheus labels.
func WithAppName(name string) Option {
	return func(c *config) { c.appName = name }
}

// WithPrometheusRegisterer sets a custom Prometheus registerer (use prometheus.NewRegistry() in tests).
func WithPrometheusRegisterer(reg prometheus.Registerer) Option {
	return func(c *config) { c.registerer = reg }
}

// WithLogger sets the logger.
func WithLogger(l *slog.Logger) Option {
	return func(c *config) { c.logger = l }
}

// New constructs a HealthServer with /healthz and /readyz endpoints.
func New(opts ...Option) *Server {
	cfg := &config{logger: slog.Default()}
	for _, opt := range opts {
		opt(cfg)
	}

	s := &Server{
		probers: cfg.probers,
		logger:  cfg.logger,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /readyz", s.handleReadyz)

	var httpOpts []httpsrv.Option
	if cfg.port != 0 {
		httpOpts = append(httpOpts, httpsrv.WithPort(cfg.port))
	}
	if cfg.host != "" {
		httpOpts = append(httpOpts, httpsrv.WithHost(cfg.host))
	}
	if cfg.appName != "" {
		httpOpts = append(httpOpts, httpsrv.WithAppName(cfg.appName))
	}
	if cfg.registerer != nil {
		httpOpts = append(httpOpts, httpsrv.WithPrometheusRegisterer(cfg.registerer))
	}
	if cfg.logger != nil {
		httpOpts = append(httpOpts, httpsrv.WithLogger(cfg.logger))
	}

	s.inner = httpsrv.NewServer(mux, httpOpts...)
	return s
}

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write([]byte(`{"status":"ok"}`)); err != nil {
		s.logger.Error("healthz: write error", slog.Any("error", err))
	}
}

func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	var (
		mu   sync.Mutex
		errs []string
		wg   sync.WaitGroup
	)
	for _, p := range s.probers {
		wg.Go(func() {
			if err := p.Probe(r.Context()); err != nil {
				mu.Lock()
				errs = append(errs, err.Error())
				mu.Unlock()
			}
		})
	}
	wg.Wait()

	w.Header().Set("Content-Type", "application/json")
	if len(errs) == 0 {
		w.WriteHeader(http.StatusOK)
		if _, err := w.Write([]byte(`{"status":"ready"}`)); err != nil {
			s.logger.Error("readyz: write error", slog.Any("error", err))
		}
		return
	}

	w.WriteHeader(http.StatusServiceUnavailable)
	body, err := json.Marshal(map[string]any{"status": "not ready", "errors": errs})
	if err != nil {
		s.logger.Error("readyz: marshal error", slog.Any("error", err))
		return
	}
	if _, err := w.Write(body); err != nil {
		s.logger.Error("readyz: write error", slog.Any("error", err))
	}
}

// Start implements service.Service.
func (s *Server) Start(ctx context.Context) error {
	if err := s.inner.Start(ctx); err != nil {
		return fmt.Errorf("healthserver start: %w", err)
	}
	return nil
}

// Stop implements service.Shutdown.
func (s *Server) Stop(ctx context.Context, cause error) error {
	if err := s.inner.Stop(ctx, cause); err != nil {
		return fmt.Errorf("healthserver stop: %w", err)
	}
	return nil
}
