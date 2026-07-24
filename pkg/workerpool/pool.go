package workerpool

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
)

// Task is a function executed by a worker goroutine.
type Task func(ctx context.Context)

// Pool is a bounded goroutine pool. Submit blocks until space is available
// in the task queue or ctx is canceled.
//
// Precondition: Submit must not be called after the pool has stopped.
type Pool struct {
	size        int
	tasks       chan Task
	wg          sync.WaitGroup
	drainOnStop bool
	logger      *slog.Logger
}

// Option configures a Pool.
type Option func(*Pool)

// WithQueueSize sets the task queue buffer size. Default: size * 2.
func WithQueueSize(n int) Option {
	return func(p *Pool) { p.tasks = make(chan Task, n) }
}

// WithDrainOnStop causes Start (and Stop) to wait for all queued tasks to
// complete before returning. Without this option, Start returns as soon as
// ctx is canceled, leaving any buffered tasks unprocessed.
func WithDrainOnStop() Option {
	return func(p *Pool) { p.drainOnStop = true }
}

// WithLogger sets the logger used by the pool.
func WithLogger(l *slog.Logger) Option {
	return func(p *Pool) { p.logger = l }
}

// New creates a Pool with the given number of worker goroutines.
func New(size int, opts ...Option) *Pool {
	p := &Pool{
		size:   size,
		tasks:  make(chan Task, size*2),
		logger: slog.Default(),
	}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// Start launches worker goroutines and blocks until ctx is canceled.
// When ctx is done, the task channel is closed. If WithDrainOnStop is set,
// Start waits for workers to finish all remaining queued tasks.
func (p *Pool) Start(ctx context.Context) error {
	for range p.size {
		p.wg.Go(func() {
			for task := range p.tasks {
				task(ctx)
			}
		})
	}

	<-ctx.Done()
	close(p.tasks)

	if p.drainOnStop {
		p.wg.Wait()
	}
	return nil
}

// Stop implements service.Shutdown. If WithDrainOnStop is set, it waits for
// workers to finish within ctx's deadline.
func (p *Pool) Stop(ctx context.Context) error {
	if !p.drainOnStop {
		return nil
	}
	done := make(chan struct{})
	go func() {
		p.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("pool stop: %w", ctx.Err())
	}
}

// Submit enqueues a task. It blocks until space is available or ctx is canceled.
// Must not be called after the pool has stopped.
func (p *Pool) Submit(ctx context.Context, task Task) (err error) {
	defer func() {
		if recover() != nil {
			if ctx.Err() != nil {
				err = fmt.Errorf("submit: %w", ctx.Err())
			} else {
				err = errors.New("pool stopped")
			}
		}
	}()
	select {
	case p.tasks <- task:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("submit: %w", ctx.Err())
	}
}
