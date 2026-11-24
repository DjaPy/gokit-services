package entrypoint_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/DjaPy/gokit-services/pkg/core/entrypoint"
)

type readinessService struct {
	trackingService
	startWithReady func(context.Context, func()) error
}

func (s *readinessService) StartWithReady(ctx context.Context, ready func()) error {
	defer s.exited.Store(true)
	return s.startWithReady(ctx, ready)
}

func TestPostStartWaitsForEveryReadyService(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	firstReady := make(chan struct{})
	allowSecond := make(chan struct{})
	secondWaiting := make(chan struct{})
	first := &readinessService{startWithReady: func(ctx context.Context, ready func()) error {
		ready()
		ready()
		close(firstReady)
		<-ctx.Done()
		return nil
	}}
	second := &readinessService{startWithReady: func(ctx context.Context, ready func()) error {
		close(secondWaiting)
		select {
		case <-allowSecond:
			ready()
		case <-ctx.Done():
			return nil
		}
		<-ctx.Done()
		return nil
	}}
	postStarted := make(chan struct{})
	ep := entrypoint.New(
		noSignals,
		entrypoint.WithServices(first, second),
		entrypoint.WithStartupReadiness(2*time.Second),
		entrypoint.WithPostStart(func(context.Context) error {
			close(postStarted)
			return nil
		}),
	)
	done := make(chan error, 1)
	go func() { done <- ep.Run(ctx) }()

	for _, entered := range []<-chan struct{}{firstReady, secondWaiting} {
		select {
		case <-entered:
		case <-ctx.Done():
			t.Fatal("services did not enter StartWithReady")
		}
	}
	select {
	case <-postStarted:
		t.Fatal("PostStart ran before the second service was ready")
	case <-time.After(25 * time.Millisecond):
	}
	close(allowSecond)
	select {
	case <-postStarted:
	case <-ctx.Done():
		t.Fatal("PostStart did not run after both services were ready")
	}

	ep.Shutdown()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-ctx.Done():
		t.Fatal("Run did not return after Shutdown")
	}
	assert.False(t, first.started.Load(), "the readiness path must use StartWithReady")
	assert.False(t, second.started.Load(), "the readiness path must use StartWithReady")
}

func TestStartupReadinessRejectsServicesWithoutContract(t *testing.T) {
	svc := &trackingService{}
	ep := entrypoint.New(
		noSignals,
		entrypoint.WithServices(svc),
		entrypoint.WithStartupReadiness(time.Second),
		entrypoint.WithPreStart(func(context.Context) error {
			t.Error("PreStart ran before readiness configuration was validated")
			return nil
		}),
	)
	assert.ErrorContains(t, ep.Run(t.Context()), "does not implement service.ReadyService")
	assert.False(t, svc.started.Load())
}

func TestStartupReadinessTimeoutRunsCleanup(t *testing.T) {
	svc := &readinessService{startWithReady: func(ctx context.Context, _ func()) error {
		<-ctx.Done()
		return nil
	}}
	postStopped := false
	ep := entrypoint.New(
		noSignals,
		entrypoint.WithServices(svc),
		entrypoint.WithStartupReadiness(25*time.Millisecond),
		entrypoint.WithShutdownTimeout(time.Second),
		entrypoint.WithPostStart(func(context.Context) error {
			t.Error("PostStart ran before the service was ready")
			return nil
		}),
		entrypoint.WithPostStop(func(context.Context) error {
			postStopped = true
			return nil
		}),
	)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	err := ep.Run(ctx)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assert.ErrorContains(t, err, "waiting for startup readiness")
	assert.True(t, svc.exited.Load())
	assert.True(t, svc.stopped.Load())
	assert.True(t, postStopped)
}

func TestServiceExitBeforeReadinessRunsCleanup(t *testing.T) {
	startErr := errors.New("service initialization failed")
	for _, tc := range []struct {
		name string
		err  error
	}{
		{name: "error", err: startErr},
		{name: "nil"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := &readinessService{startWithReady: func(context.Context, func()) error {
				return tc.err
			}}
			ep := entrypoint.New(
				noSignals,
				entrypoint.WithServices(svc),
				entrypoint.WithStartupReadiness(time.Second),
				entrypoint.WithPostStart(func(context.Context) error {
					t.Error("PostStart ran after the service exited without signaling readiness")
					return nil
				}),
			)

			err := ep.Run(t.Context())
			if tc.err == nil {
				assert.ErrorContains(t, err, "returned before signaling startup readiness")
			} else {
				assert.ErrorIs(t, err, tc.err)
			}
			assert.True(t, svc.exited.Load())
			assert.True(t, svc.stopped.Load())
		})
	}
}
