package periodic

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// Service runs a callback on a fixed interval.
// By default, if the previous invocation is still running when the next tick
// fires, the tick is skipped (non-overlapping). Use WithOverlapping to allow
// concurrent invocations.
type Service struct {
	interval       time.Duration
	fn             func(context.Context) error
	overlapping    bool
	immediateStart bool
	logger         *slog.Logger
}

// Option configures a Service.
type Option func(*Service)

// WithOverlapping allows concurrent fn invocations: each tick launches fn in
// a new goroutine regardless of whether the previous one is still running.
func WithOverlapping() Option {
	return func(s *Service) { s.overlapping = true }
}

// WithImmediateStart calls fn once before the first tick fires.
func WithImmediateStart() Option {
	return func(s *Service) { s.immediateStart = true }
}

// WithLogger sets the logger used by the service.
func WithLogger(l *slog.Logger) Option {
	return func(s *Service) { s.logger = l }
}

// New creates a Service that calls fn every interval.
func New(interval time.Duration, fn func(context.Context) error, opts ...Option) *Service {
	s := &Service{
		interval: interval,
		fn:       fn,
		logger:   slog.Default(),
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Start implements service.Service. It blocks until ctx is canceled.
func (s *Service) Start(ctx context.Context) error {
	if s.immediateStart {
		if err := s.fn(ctx); err != nil {
			s.logger.Error("periodic fn error", slog.Any("error", err))
		}
	}

	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()

	if s.overlapping {
		return s.runOverlapping(ctx, ticker)
	}
	return s.runNonOverlapping(ctx, ticker)
}

// runNonOverlapping runs fn on each tick, skipping if fn is still running.
// Waits for any in-flight invocation before returning.
func (s *Service) runNonOverlapping(ctx context.Context, ticker *time.Ticker) error {
	var busy atomic.Bool
	var wg sync.WaitGroup
	defer wg.Wait()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if !busy.CompareAndSwap(false, true) {
				s.logger.Warn("periodic: skipping tick — previous invocation still running")
				continue
			}
			wg.Go(func() {
				defer busy.Store(false)
				if err := s.fn(ctx); err != nil {
					s.logger.Error("periodic fn error", slog.Any("error", err))
				}
			})
		}
	}
}

// runOverlapping launches fn in a new goroutine on every tick.
// Waits for all in-flight invocations before returning.
func (s *Service) runOverlapping(ctx context.Context, ticker *time.Ticker) error {
	var wg sync.WaitGroup
	defer wg.Wait()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			wg.Go(func() {
				if err := s.fn(ctx); err != nil {
					s.logger.Error("periodic fn error", slog.Any("error", err))
				}
			})
		}
	}
}
