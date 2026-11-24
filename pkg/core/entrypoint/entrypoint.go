package entrypoint

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"slices"
	"sync"
	"sync/atomic"
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
	services                []service.Service
	shutdownGroups          [][]service.Service
	shutdownTimeout         time.Duration
	startupReadinessTimeout time.Duration
	failOnUnexpectedExit    bool
	logger                  *slog.Logger
	catchSignals            []os.Signal

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

func WithShutdownGroups(groups ...[]service.Service) Option {
	return func(e *Entrypoint) {
		for _, group := range groups {
			if len(group) > 0 {
				e.shutdownGroups = append(e.shutdownGroups, slices.Clone(group))
			}
		}
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

func WithStartupReadiness(timeout time.Duration) Option {
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	return func(e *Entrypoint) {
		e.startupReadinessTimeout = timeout
	}
}

func WithFailOnUnexpectedExit() Option {
	return func(e *Entrypoint) {
		e.failOnUnexpectedExit = true
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

type RunResult struct {
	Signal      os.Signal
	Cause       error
	ShutdownErr error
}

// Run starts all registered services and blocks until a shutdown is triggered.
//
// Shutdown is triggered by one of:
//   - SIGINT or SIGTERM received from the OS
//   - parent ctx being canceled
//   - a service returning an error from Start
//   - a PostStart hook returning an error
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
	result := e.RunWithResult(ctx)
	if result.Signal != nil {
		return fmt.Errorf("received signal: %v", result.Signal)
	}
	return result.Cause
}

func (e *Entrypoint) RunWithResult(ctx context.Context) RunResult {
	groups, serviceCount := e.serviceGroups()
	startup, err := e.prepareStartupReadiness(groups, serviceCount)
	if err != nil {
		return RunResult{Cause: err}
	}

	sigCh := make(chan os.Signal, 1)
	if len(e.catchSignals) > 0 {
		signal.Notify(sigCh, e.catchSignals...)
		defer signal.Stop(sigCh)
	}

	hookCtx, cancelHooks := context.WithCancel(ctx)
	defer cancelHooks()
	if completed, result := e.runStartupHooks(hookCtx, sigCh, nil, "pre-start", e.onPreStart); !completed {
		return result
	}

	svcErrs := make(chan error, serviceCount)
	serviceCtx := ctx
	if len(e.shutdownGroups) > 0 {
		serviceCtx = context.WithoutCancel(ctx)
	}
	for _, group := range groups {
		e.startGroup(serviceCtx, group, startup, svcErrs)
	}

	result := e.waitForShutdown(hookCtx, sigCh, svcErrs, startup)

	cancelHooks()
	result.ShutdownErr = e.stopGroups(ctx, groups, serviceCount)
	return result
}

type runningGroup struct {
	services []service.Service
	startup  []startupService
	running  sync.WaitGroup
	cancel   context.CancelFunc
}

func (e *Entrypoint) serviceGroups() (groups []*runningGroup, serviceCount int) {
	groups = make([]*runningGroup, 0, len(e.shutdownGroups)+1)
	serviceCount = len(e.services)
	for _, services := range e.shutdownGroups {
		groups = append(groups, &runningGroup{services: services})
		serviceCount += len(services)
	}
	if len(e.services) > 0 || len(groups) == 0 {
		groups = append(groups, &runningGroup{services: e.services})
	}
	return groups, serviceCount
}

func (e *Entrypoint) startGroup(
	ctx context.Context,
	group *runningGroup,
	startup *startupReadiness,
	svcErrs chan<- error,
) {
	svcCtx, cancel := context.WithCancel(ctx)
	group.cancel = cancel
	for index, svc := range group.services {
		group.running.Go(func() {
			var startErr error
			if startup == nil {
				startErr = svc.Start(svcCtx)
			} else {
				startErr = group.startup[index].start(svcCtx, startup.ready)
			}
			if svcCtx.Err() != nil {
				return
			}
			if startErr == nil && e.failOnUnexpectedExit && e.shutdownCtx.Err() == nil {
				startErr = fmt.Errorf("service %T exited unexpectedly", svc)
			}
			if startErr != nil {
				svcErrs <- startErr
			}
		})
	}
}

func (e *Entrypoint) stopGroups(ctx context.Context, groups []*runningGroup, serviceCount int) error {
	grouped := len(e.shutdownGroups) > 0
	cleanupCtx := context.WithoutCancel(ctx)
	var shutdownErrs []error
	if grouped {
		shutdownErrs = append(shutdownErrs, e.runBoundedShutdownHooks(cleanupCtx, "pre-stop", e.onPreStop))
	} else {
		groups[0].cancel()
	}

	stopCtx, cancel := context.WithTimeout(cleanupCtx, e.shutdownTimeout)
	defer cancel()
	if !grouped {
		if err := runHooks(stopCtx, e.onPreStop); err != nil {
			e.logger.Error("pre-stop hook error", "error", err)
			shutdownErrs = append(shutdownErrs, fmt.Errorf("pre-stop hook: %w", err))
		}
	}

	stopErrs := make(chan error, serviceCount)
	for _, group := range groups {
		group.cancel()
		select {
		case <-e.stopGroup(stopCtx, group, stopErrs):
		case <-stopCtx.Done():
		}
	}
	if err := stopCtx.Err(); err != nil {
		e.logger.Warn("shutdown timeout exceeded, some services may not have stopped cleanly",
			"timeout", e.shutdownTimeout)
		shutdownErrs = append(shutdownErrs, fmt.Errorf("stopping services: %w", err))
	}

	if grouped {
		shutdownErrs = append(shutdownErrs, e.runBoundedShutdownHooks(cleanupCtx, "post-stop", e.onPostStop))
	} else if err := runHooks(context.Background(), e.onPostStop); err != nil {
		e.logger.Error("post-stop hook error", "error", err)
		shutdownErrs = append(shutdownErrs, fmt.Errorf("post-stop hook: %w", err))
	}

	for {
		select {
		case err := <-stopErrs:
			shutdownErrs = append(shutdownErrs, err)
		default:
			return errors.Join(shutdownErrs...)
		}
	}
}

func (e *Entrypoint) stopGroup(ctx context.Context, group *runningGroup, stopErrs chan<- error) <-chan struct{} {
	var stopping sync.WaitGroup
	for index, svc := range group.services {
		stopper, ok := svc.(service.Shutdown)
		if !ok {
			continue
		}
		stopping.Go(func() {
			if group.startup != nil {
				select {
				case <-group.startup[index].initialized:
				case <-ctx.Done():
				}
			}
			if err := stopper.Stop(ctx); err != nil {
				stopErrs <- fmt.Errorf("stop service %T: %w", svc, err)
				e.logger.Error("service stop error", "error", err)
			}
		})
	}

	done := make(chan struct{})
	go func() {
		group.running.Wait()
		stopping.Wait()
		close(done)
	}()
	return done
}

func (e *Entrypoint) runBoundedShutdownHooks(ctx context.Context, phase string, hooks []hookFn) error {
	if len(hooks) == 0 {
		return nil
	}
	hookCtx, cancel := context.WithTimeout(ctx, e.shutdownTimeout)
	defer cancel()

	for _, hook := range hooks {
		err := hookCtx.Err()
		if err == nil {
			select {
			case err = <-startHook(hookCtx, hook):
				if deadlineErr := hookCtx.Err(); deadlineErr != nil && !errors.Is(err, deadlineErr) {
					err = errors.Join(err, deadlineErr)
				}
			case <-hookCtx.Done():
				err = hookCtx.Err()
			}
		}
		if err != nil {
			e.logger.Error(phase+" hook error", "error", err)
			return fmt.Errorf("%s hook: %w", phase, err)
		}
	}
	return nil
}

func (e *Entrypoint) waitForShutdown(
	ctx context.Context,
	sigCh <-chan os.Signal,
	svcErrs <-chan error,
	startup *startupReadiness,
) RunResult {
	if startup != nil {
		ready, result := e.waitForReadiness(ctx, sigCh, svcErrs, startup)
		if !ready {
			return result
		}
	}
	if completed, result := e.runStartupHooks(ctx, sigCh, svcErrs, "post-start", e.onPostStart); !completed {
		return result
	}

	_, result := e.waitForServiceEvent(ctx, sigCh, svcErrs, startupWait{})
	return result
}

func (e *Entrypoint) runStartupHooks(
	ctx context.Context,
	sigCh <-chan os.Signal,
	svcErrs <-chan error,
	phase string,
	hooks []hookFn,
) (bool, RunResult) {
	for _, hook := range hooks {
		wait := startupWait{hookDone: startHook(ctx, hook)}
		completed, result := e.waitForServiceEvent(ctx, sigCh, svcErrs, wait)
		if !completed {
			return false, result
		}
		if result.Cause != nil {
			return false, RunResult{Cause: fmt.Errorf("%s: %w", phase, result.Cause)}
		}
	}
	return true, RunResult{}
}

func startHook(ctx context.Context, hook hookFn) <-chan error {
	done := make(chan error, 1)
	go func() { done <- hook(ctx) }()
	return done
}

type startupService struct {
	service     service.ReadyService
	initialized chan struct{}
}

type startupReadiness struct {
	ready        chan struct{}
	serviceCount int
}

func (e *Entrypoint) prepareStartupReadiness(groups []*runningGroup, serviceCount int) (*startupReadiness, error) {
	if e.startupReadinessTimeout == 0 {
		return nil, nil
	}

	for _, group := range groups {
		group.startup = make([]startupService, 0, len(group.services))
		for _, svc := range group.services {
			readyService, ok := svc.(service.ReadyService)
			if !ok {
				return nil, fmt.Errorf("startup readiness: service %T does not implement service.ReadyService", svc)
			}
			group.startup = append(group.startup, startupService{
				service:     readyService,
				initialized: make(chan struct{}),
			})
		}
	}

	return &startupReadiness{ready: make(chan struct{}, serviceCount), serviceCount: serviceCount}, nil
}

func (s startupService) start(ctx context.Context, ready chan<- struct{}) error {
	finishInitialization := sync.OnceFunc(func() { close(s.initialized) })
	defer finishInitialization()

	var reported atomic.Bool
	err := s.service.StartWithReady(ctx, func() {
		if reported.CompareAndSwap(false, true) {
			finishInitialization()
			ready <- struct{}{}
		}
	})
	if err != nil {
		return fmt.Errorf("start service %T: %w", s.service, err)
	}
	if !reported.Load() {
		return fmt.Errorf("service %T returned before signaling startup readiness", s.service)
	}
	return nil
}

func (e *Entrypoint) waitForReadiness(
	ctx context.Context,
	sigCh <-chan os.Signal,
	svcErrs <-chan error,
	startup *startupReadiness,
) (bool, RunResult) {
	timer := time.NewTimer(e.startupReadinessTimeout)
	defer timer.Stop()

	wait := startupWait{ready: startup.ready, deadline: timer.C}
	for range startup.serviceCount {
		reported, result := e.waitForServiceEvent(ctx, sigCh, svcErrs, wait)
		if !reported {
			return false, result
		}
	}
	return true, RunResult{}
}

type startupWait struct {
	ready    <-chan struct{}
	deadline <-chan time.Time
	hookDone <-chan error
}

func (e *Entrypoint) waitForServiceEvent(
	ctx context.Context,
	sigCh <-chan os.Signal,
	svcErrs <-chan error,
	wait startupWait,
) (bool, RunResult) {
	var result RunResult
	select {
	case err := <-wait.hookDone:
		return true, RunResult{Cause: err}
	case <-wait.ready:
		return true, RunResult{}
	case <-wait.deadline:
		result.Cause = fmt.Errorf("waiting for startup readiness: %w", context.DeadlineExceeded)
	case <-ctx.Done():
		result.Cause = ctx.Err()
	case sig := <-sigCh:
		e.logger.Warn("received signal, shutting down", "signal", sig)
		result.Signal = sig
	case err := <-svcErrs:
		e.logger.Error("service error, shutting down", "error", err)
		result.Cause = err
	case <-e.shutdownCtx.Done():
	}
	return false, result
}
