package entrypoint_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/DjaPy/gokit-services/entrypoint"
	"github.com/DjaPy/gokit-services/service"
)

// trackingService records Start/Stop calls and blocks in Start until ctx is canceled.
type trackingService struct {
	started  atomic.Bool
	stopped  atomic.Bool
	startErr error
	stopErr  error
}

func (s *trackingService) Start(ctx context.Context) error {
	s.started.Store(true)
	if s.startErr != nil {
		return s.startErr
	}
	<-ctx.Done()
	return nil
}

func (s *trackingService) Stop(_ context.Context, _ error) error {
	s.stopped.Store(true)
	return s.stopErr
}

var _ service.Service = (*trackingService)(nil)
var _ service.Stopper = (*trackingService)(nil)

// noSignals disables OS signal catching so tests are not affected by SIGINT/SIGTERM.
var noSignals = entrypoint.WithCatchSignals()

// cancelAfter returns a context that cancels itself after d.
func cancelAfter(d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), d)
}

func TestMultipleServices(t *testing.T) {
	const count = 5
	svcs := make([]*trackingService, count)
	svcIfaces := make([]service.Service, count)
	for i := range svcs {
		svcs[i] = &trackingService{}
		svcIfaces[i] = svcs[i]
	}

	ctx, cancel := cancelAfter(100 * time.Millisecond)
	defer cancel()

	entrypoint.New(entrypoint.WithServices(svcIfaces...), noSignals).Run(ctx) //nolint:errcheck

	for i, svc := range svcs {
		assert.True(t, svc.started.Load(), "service %d: Start not called", i)
		assert.True(t, svc.stopped.Load(), "service %d: Stop not called", i)
	}
}

func TestServiceStartErrorTriggersShutdown(t *testing.T) {
	startErr := errors.New("failed to start")
	failingSvc := &trackingService{startErr: startErr}
	healthySvc := &trackingService{}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := entrypoint.New(
		entrypoint.WithServices(failingSvc, healthySvc),
		noSignals,
	).Run(ctx)

	assert.ErrorIs(t, err, startErr)
	assert.True(t, failingSvc.started.Load())
	assert.True(t, healthySvc.stopped.Load(), "healthy service must be stopped after sibling error")
}

func TestExternalShutdown(t *testing.T) {
	svc := &trackingService{}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ep := entrypoint.New(entrypoint.WithServices(svc), noSignals)

	var runErr error
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		runErr = ep.Run(ctx)
	}()

	time.Sleep(20 * time.Millisecond)
	ep.Shutdown()
	wg.Wait()

	assert.NoError(t, runErr)
	assert.True(t, svc.started.Load())
	assert.True(t, svc.stopped.Load())
}

func TestShutdownIdempotent(t *testing.T) {
	ep := entrypoint.New(noSignals)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	go ep.Run(ctx) //nolint:errcheck

	time.Sleep(20 * time.Millisecond)
	// Multiple Shutdown calls must not panic or block.
	ep.Shutdown()
	ep.Shutdown()
	ep.Shutdown()
}

func TestContextCancellationTriggersShutdown(t *testing.T) {
	svc := &trackingService{}
	ctx, cancel := context.WithCancel(context.Background())

	ep := entrypoint.New(entrypoint.WithServices(svc), noSignals)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		ep.Run(ctx) //nolint:errcheck
	}()

	time.Sleep(20 * time.Millisecond)
	cancel()
	wg.Wait()

	assert.True(t, svc.started.Load())
	assert.True(t, svc.stopped.Load())
}

func TestHooksCalledInOrder(t *testing.T) {
	var mu sync.Mutex
	var order []string

	hook := func(name string) func(context.Context) error {
		return func(_ context.Context) error {
			mu.Lock()
			defer mu.Unlock()
			order = append(order, name)
			return nil
		}
	}

	ctx, cancel := cancelAfter(50 * time.Millisecond)
	defer cancel()

	entrypoint.New(
		noSignals,
		entrypoint.WithPreStart(hook("pre-start")),
		entrypoint.WithPostStart(hook("post-start")),
		entrypoint.WithPreStop(hook("pre-stop")),
		entrypoint.WithPostStop(hook("post-stop")),
	).Run(ctx) //nolint:errcheck

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, []string{"pre-start", "post-start", "pre-stop", "post-stop"}, order)
}

func TestPreStartErrorAbortsRun(t *testing.T) {
	hookErr := errors.New("pre-start failed")
	svc := &trackingService{}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := entrypoint.New(
		entrypoint.WithServices(svc),
		noSignals,
		entrypoint.WithPreStart(func(_ context.Context) error { return hookErr }),
	).Run(ctx)

	assert.ErrorIs(t, err, hookErr)
	assert.False(t, svc.started.Load(), "services must not start if pre-start hook fails")
}

func TestShutdownTimeout(t *testing.T) {
	// Service that ignores ctx cancellation and sleeps in Stop.
	slowSvc := &slowStopService{delay: 500 * time.Millisecond}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ep := entrypoint.New(
		entrypoint.WithServices(slowSvc),
		noSignals,
		entrypoint.WithShutdownTimeout(100*time.Millisecond),
	)

	go func() {
		time.Sleep(20 * time.Millisecond)
		ep.Shutdown()
	}()

	start := time.Now()
	ep.Run(ctx) //nolint:errcheck

	assert.Less(t, time.Since(start), 400*time.Millisecond,
		"Run should return after shutdown timeout, not wait for slow Stop")
}

func TestNoServices(t *testing.T) {
	ctx, cancel := cancelAfter(50 * time.Millisecond)
	defer cancel()

	// No services — Run blocks until ctx expires.
	err := entrypoint.New(noSignals).Run(ctx)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
}

// slowStopService ignores ctx and sleeps in Stop to test shutdown timeout.
type slowStopService struct {
	delay time.Duration
}

func (s *slowStopService) Start(ctx context.Context) error {
	<-ctx.Done()
	return nil
}

func (s *slowStopService) Stop(_ context.Context, _ error) error {
	time.Sleep(s.delay)
	return nil
}
