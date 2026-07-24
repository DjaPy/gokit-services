package entrypoint

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/DjaPy/gokit-services/pkg/core/service"
)

type hookFn func(ctx context.Context) error

const defaultShutdownTimeout = 60 * time.Second

func runHooks(ctx context.Context, hooks []hookFn) error {
	for _, h := range hooks {
		if err := h(ctx); err != nil {
			return err
		}
	}
	return nil
}

// Entrypoint manages the lifecycle of a set of services.
type Entrypoint struct {
	services        []service.Service
	shutdownTimeout time.Duration
	logger          *slog.Logger
	catchSignals    []os.Signal

	onPreStart  []hookFn
	onPostStart []hookFn
	onPreStop   []hookFn
	onPostStop  []hookFn

	shutdownCancel context.CancelCauseFunc
	shutdownCtx    context.Context
}

// Option configures an Entrypoint.
type Option func(*Entrypoint)

// WithServices registers services with the entrypoint.
func WithServices(svcs ...service.Service) Option {
	return func(e *Entrypoint) {
		e.services = append(e.services, svcs...)
	}
}

// WithShutdownTimeout sets the graceful shutdown timeout.
// Default: 60s. Values ≤ 0 are ignored.
func WithShutdownTimeout(d time.Duration) Option {
	return func(e *Entrypoint) {
		if d > 0 {
			e.shutdownTimeout = d
		}
	}
}

// WithLogger sets the logger used by the entrypoint.
func WithLogger(l *slog.Logger) Option {
	return func(e *Entrypoint) {
		e.logger = l
	}
}

// WithCatchSignals overrides the OS signals that trigger shutdown.
// Pass no signals to disable OS signal handling.
func WithCatchSignals(sigs ...os.Signal) Option {
	return func(e *Entrypoint) {
		e.catchSignals = sigs
	}
}

// WithPreStart adds a hook called before services start.
func WithPreStart(fn hookFn) Option {
	return func(e *Entrypoint) {
		e.onPreStart = append(e.onPreStart, fn)
	}
}

// WithPostStart adds a hook called after all services have started.
func WithPostStart(fn hookFn) Option {
	return func(e *Entrypoint) {
		e.onPostStart = append(e.onPostStart, fn)
	}
}

// WithPreStop adds a hook called before services are stopped.
func WithPreStop(fn hookFn) Option {
	return func(e *Entrypoint) {
		e.onPreStop = append(e.onPreStop, fn)
	}
}

// WithPostStop adds a hook called after all services have stopped.
func WithPostStop(fn hookFn) Option {
	return func(e *Entrypoint) {
		e.onPostStop = append(e.onPostStop, fn)
	}
}

// New creates a new Entrypoint with the given options.
// By default, it catches SIGINT and SIGTERM and uses slog.Default() for logging.
func New(opts ...Option) *Entrypoint {
	shutdownCtx, shutdownCancel := context.WithCancelCause(context.Background())

	e := &Entrypoint{
		shutdownTimeout: defaultShutdownTimeout,
		logger:          slog.Default(),
		catchSignals:    []os.Signal{syscall.SIGINT, syscall.SIGTERM},
		shutdownCtx:     shutdownCtx,
		shutdownCancel:  shutdownCancel,
	}

	for _, opt := range opts {
		opt(e)
	}

	return e
}

// AddService registers additional services before Run is called.
func (e *Entrypoint) AddService(svcs ...service.Service) {
	e.services = append(e.services, svcs...)
}

// Shutdown initiates a graceful shutdown from outside Run.
// Safe to call multiple times — subsequent calls are no-ops.
func (e *Entrypoint) Shutdown() {
	e.shutdownCancel(nil)
}

// Run starts all registered services and blocks until a shutdown is triggered.
//
// Shutdown is triggered by one of:
//   - SIGINT or SIGTERM received from the OS
//   - parent ctx being canceled
//   - a service returning an error from Start
//   - an explicit call to Shutdown
//
// Lifecycle order:
//  1. PreStart hooks
//  2. Start all services (concurrently)
//  3. PostStart hooks
//  4. blocks until shutdown is triggered
//  5. PreStop hooks
//  6. Stop all services (concurrently, bounded by shutdown timeout)
//  7. PostStop hooks
func (e *Entrypoint) Run(ctx context.Context) error {
	sigCh := make(chan os.Signal, 1)
	if len(e.catchSignals) > 0 {
		signal.Notify(sigCh, e.catchSignals...)
		defer signal.Stop(sigCh)
	}

	if err := runHooks(ctx, e.onPreStart); err != nil {
		return fmt.Errorf("pre-start: %w", err)
	}

	svcCtx, svcCancel := context.WithCancel(ctx)

	var wg sync.WaitGroup
	svcErrs := make(chan error, len(e.services))

	for _, svc := range e.services {
		wg.Go(func() {
			if err := svc.Start(svcCtx); err != nil && svcCtx.Err() == nil {
				svcErrs <- err
			}
		})
	}

	if err := runHooks(ctx, e.onPostStart); err != nil {
		svcCancel()
		return fmt.Errorf("post-start: %w", err)
	}

	var shutdownCause error
	select {
	case <-ctx.Done():
		shutdownCause = ctx.Err()
	case sig := <-sigCh:
		e.logger.Warn("received signal, shutting down", "signal", sig)
		shutdownCause = fmt.Errorf("received signal: %v", sig)
	case err := <-svcErrs:
		e.logger.Error("service error, shutting down", "error", err)
		shutdownCause = err
	case <-e.shutdownCtx.Done():
	}

	svcCancel()

	stopCtx, stopCancel := context.WithTimeout(context.WithoutCancel(ctx), e.shutdownTimeout)
	defer stopCancel()

	if err := runHooks(stopCtx, e.onPreStop); err != nil {
		e.logger.Error("pre-stop hook error", "error", err)
	}

	var stopWg sync.WaitGroup
	for _, svc := range e.services {
		stopper, ok := svc.(service.Shutdown)
		if !ok {
			continue
		}
		stopWg.Go(func() {
			if err := stopper.Stop(stopCtx); err != nil {
				e.logger.Error("service stop error", "error", err)
			}
		})
	}

	allDone := make(chan struct{})
	go func() {
		wg.Wait()
		stopWg.Wait()
		close(allDone)
	}()

	select {
	case <-allDone:
	case <-stopCtx.Done():
		e.logger.Warn("shutdown timeout exceeded, some services may not have stopped cleanly",
			"timeout", e.shutdownTimeout)
	}

	if err := runHooks(context.Background(), e.onPostStop); err != nil {
		e.logger.Error("post-stop hook error", "error", err)
	}

	return shutdownCause
}
