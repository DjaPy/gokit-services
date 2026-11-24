package entrypoint_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/DjaPy/gokit-services/pkg/core/entrypoint"
	"github.com/DjaPy/gokit-services/pkg/core/service"
)

// trackingService records Start/Stop calls and blocks in Start until ctx is canceled.
type trackingService struct {
	started  atomic.Bool
	exited   atomic.Bool
	stopped  atomic.Bool
	startErr error
	stopErr  error
}

func (s *trackingService) Start(ctx context.Context) error {
	s.started.Store(true)
	defer s.exited.Store(true)
	if s.startErr != nil {
		return s.startErr
	}
	<-ctx.Done()
	return nil
}

func (s *trackingService) Stop(_ context.Context) error {
	s.stopped.Store(true)
	return s.stopErr
}

// noSignals disables OS signal catching so tests are not affected by SIGINT/SIGTERM.
var noSignals = entrypoint.WithCatchSignals()

// cancelAfter returns a context that cancels itself after d.
func cancelAfter(d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), d)
}

func TestMultipleServices(t *testing.T) {
	svcCount := 5
	svcs := make([]*trackingService, svcCount)
	svcIfaces := make([]service.Service, svcCount)
	for i := range svcs {
		svcs[i] = &trackingService{}
		svcIfaces[i] = svcs[i]
	}

	timeout := 100 * time.Millisecond
	ctx, cancel := cancelAfter(timeout)
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

type exitingService struct {
	trackingService
	exit        <-chan struct{}
	exitedEarly atomic.Bool
}

func (s *exitingService) Start(ctx context.Context) error {
	return s.StartWithReady(ctx, func() {})
}

func (s *exitingService) StartWithReady(ctx context.Context, ready func()) error {
	defer s.exited.Store(true)
	ready()
	select {
	case <-s.exit:
		s.exitedEarly.Store(true)
	case <-ctx.Done():
	}
	return nil
}

func TestUnexpectedServiceExitPolicy(t *testing.T) {
	for _, tc := range []struct {
		name      string
		readiness bool
		enforce   bool
	}{
		{name: "default"},
		{name: "enabled", enforce: true},
		{name: "enabled after readiness", readiness: true, enforce: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			exit := make(chan struct{})
			svc := &exitingService{exit: exit}
			peer := &exitingService{}
			options := []entrypoint.Option{
				noSignals,
				entrypoint.WithServices(svc, peer),
				entrypoint.WithPostStart(func(context.Context) error {
					close(exit)
					return nil
				}),
			}
			if tc.readiness {
				options = append(options, entrypoint.WithStartupReadiness(time.Second))
			}
			if tc.enforce {
				options = append(options, entrypoint.WithFailOnUnexpectedExit())
			}

			timeout := 250 * time.Millisecond
			if tc.enforce {
				timeout = 5 * time.Second
			}
			ctx, cancel := context.WithTimeout(t.Context(), timeout)
			defer cancel()

			err := entrypoint.New(options...).Run(ctx)
			if tc.enforce {
				assert.ErrorContains(t, err, "exited unexpectedly")
			} else {
				assert.ErrorIs(t, err, context.DeadlineExceeded)
			}
			assert.True(t, svc.exitedEarly.Load())
			assert.True(t, peer.stopped.Load())
			assert.True(t, peer.exited.Load())
		})
	}
}

func TestExternalShutdown(t *testing.T) {
	svc := &trackingService{}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ep := entrypoint.New(entrypoint.WithServices(svc), noSignals)

	var runErr error
	var wg sync.WaitGroup
	wg.Go(func() {
		runErr = ep.Run(ctx)
	})

	waitBefore := 20 * time.Millisecond
	time.Sleep(waitBefore)
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

	waitBefore := 20 * time.Millisecond
	time.Sleep(waitBefore)
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
	wg.Go(func() {
		ep.Run(ctx) //nolint:errcheck
	})

	waitBefore := 20 * time.Millisecond
	time.Sleep(waitBefore)
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

	timeout := 50 * time.Millisecond
	ctx, cancel := cancelAfter(timeout)
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

const startupHookSignalPhase = "GOKIT_TEST_STARTUP_HOOK_SIGNAL_PHASE"

func TestStartupHooksHandleSIGTERM(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("sending SIGTERM to a process requires POSIX signals")
	}
	if phase := os.Getenv(startupHookSignalPhase); phase != "" {
		verifyStartupHookSIGTERM(t, phase)
		return
	}

	executable, err := os.Executable()
	require.NoError(t, err)
	for _, phase := range []string{"pre-start", "post-start", "post-start-result"} {
		t.Run(phase, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()

			cmd := exec.CommandContext(ctx, executable, "-test.run=^TestStartupHooksHandleSIGTERM$")
			cmd.Env = append(os.Environ(), startupHookSignalPhase+"="+phase)
			output, err := cmd.CombinedOutput()
			require.NoError(t, err, "%s", output)
		})
	}
}

func verifyStartupHookSIGTERM(t *testing.T, phase string) {
	t.Helper()
	proc, err := os.FindProcess(os.Getpid())
	require.NoError(t, err)
	hookCanceled := make(chan struct{})
	releaseHook := make(chan struct{})
	defer close(releaseHook)

	hook := func(ctx context.Context) error {
		if err := proc.Signal(syscall.SIGTERM); err != nil {
			return err
		}
		<-ctx.Done()
		close(hookCanceled)
		<-releaseHook
		return nil
	}

	svc := &trackingService{}
	options := []entrypoint.Option{
		entrypoint.WithCatchSignals(syscall.SIGTERM),
		entrypoint.WithServices(svc),
		entrypoint.WithShutdownTimeout(time.Second),
	}
	if phase == "pre-start" {
		options = append(options, entrypoint.WithPreStart(hook))
	} else {
		options = append(options, entrypoint.WithPostStart(hook))
	}
	ep := entrypoint.New(options...)
	if phase == "post-start-result" {
		result := ep.RunWithResult(t.Context())
		require.Equal(t, syscall.SIGTERM, result.Signal)
		require.NoError(t, result.Cause)
		require.NoError(t, result.ShutdownErr)
	} else {
		require.ErrorContains(t, ep.Run(t.Context()), "received signal: "+syscall.SIGTERM.String())
	}
	requireStartupHookCanceled(t, hookCanceled)

	launched := phase != "pre-start"
	assert.Equal(t, launched, svc.started.Load())
	assert.Equal(t, launched, svc.stopped.Load())
	assert.Equal(t, launched, svc.exited.Load())
}

func TestServiceErrorInterruptsPostStartHook(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	hookEntered := make(chan struct{})
	hookCanceled := make(chan struct{})
	releaseHook := make(chan struct{})
	defer close(releaseHook)
	serviceErr := errors.New("service failed while post-start was running")
	svc := &readinessService{startWithReady: func(ctx context.Context, ready func()) error {
		ready()
		select {
		case <-hookEntered:
			return serviceErr
		case <-ctx.Done():
			return nil
		}
	}}
	ep := entrypoint.New(
		noSignals,
		entrypoint.WithServices(svc),
		entrypoint.WithStartupReadiness(time.Second),
		entrypoint.WithPostStart(func(ctx context.Context) error {
			close(hookEntered)
			<-ctx.Done()
			close(hookCanceled)
			<-releaseHook
			return nil
		}),
		entrypoint.WithPostStart(func(context.Context) error {
			return errors.New("a later PostStart hook was called")
		}),
	)

	done := make(chan error, 1)
	go func() { done <- ep.Run(ctx) }()
	select {
	case err := <-done:
		require.ErrorIs(t, err, serviceErr)
	case <-ctx.Done():
		t.Fatal("service error did not interrupt PostStart")
	}
	requireStartupHookCanceled(t, hookCanceled)
	assert.True(t, svc.stopped.Load())
	assert.True(t, svc.exited.Load())
}

func requireStartupHookCanceled(t *testing.T, cancellation <-chan struct{}) {
	t.Helper()
	select {
	case <-cancellation:
	case <-time.After(time.Second):
		t.Fatal("startup hook context was not canceled")
	}
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

func TestPostStartErrorRunsShutdown(t *testing.T) {
	postStartErr := errors.New("post-start failed")
	svc := &trackingService{}
	var hooks []string

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	err := entrypoint.New(
		entrypoint.WithServices(svc),
		noSignals,
		entrypoint.WithPostStart(func(context.Context) error { return postStartErr }),
		entrypoint.WithPreStop(func(context.Context) error {
			hooks = append(hooks, "pre-stop")
			return nil
		}),
		entrypoint.WithPostStop(func(context.Context) error {
			hooks = append(hooks, "post-stop")
			assert.True(t, svc.stopped.Load(), "Stop must complete before PostStop")
			assert.True(t, svc.exited.Load(), "Start must return before PostStop")
			return nil
		}),
	).Run(ctx)

	assert.ErrorIs(t, err, postStartErr)
	assert.Equal(t, []string{"pre-stop", "post-stop"}, hooks)
}

func TestRunWithResultReportsCauseAndCleanupErrors(t *testing.T) {
	startErr := errors.New("post-start failed")
	preStopErr := errors.New("pre-stop failed")
	firstStopErr := errors.New("first stop failed")
	secondStopErr := errors.New("second stop failed")
	postStopErr := errors.New("post-stop failed")

	for _, grouped := range []bool{false, true} {
		name := "default"
		if grouped {
			name = "grouped"
		}
		t.Run(name, func(t *testing.T) {
			first := &trackingService{stopErr: firstStopErr}
			second := &trackingService{stopErr: secondStopErr}
			options := []entrypoint.Option{
				noSignals,
				entrypoint.WithPostStart(func(context.Context) error { return startErr }),
				entrypoint.WithPreStop(func(context.Context) error { return preStopErr }),
				entrypoint.WithPostStop(func(context.Context) error { return postStopErr }),
			}
			if grouped {
				options = append(options, entrypoint.WithShutdownGroups([]service.Service{first, second}))
			} else {
				options = append(options, entrypoint.WithServices(first, second))
			}

			result := entrypoint.New(options...).RunWithResult(t.Context())
			require.Nil(t, result.Signal)
			require.ErrorIs(t, result.Cause, startErr)
			for _, err := range []error{preStopErr, firstStopErr, secondStopErr, postStopErr} {
				assert.ErrorIs(t, result.ShutdownErr, err)
			}
		})
	}
}

func TestRunIgnoresCleanupFailureOnProgrammaticShutdown(t *testing.T) {
	svc := &trackingService{stopErr: errors.New("stop failed")}
	var ep *entrypoint.Entrypoint
	ep = entrypoint.New(
		noSignals,
		entrypoint.WithServices(svc),
		entrypoint.WithPostStart(func(context.Context) error {
			ep.Shutdown()
			return nil
		}),
	)

	require.NoError(t, ep.Run(t.Context()))
	assert.True(t, svc.stopped.Load())
}

type groupedService struct {
	lifetime   chan context.Context
	waitToExit <-chan struct{}
	stop       func(context.Context) error
}

func (s *groupedService) Start(ctx context.Context) error {
	return s.StartWithReady(ctx, func() {})
}

func (s *groupedService) StartWithReady(ctx context.Context, ready func()) error {
	s.lifetime <- ctx
	ready()
	<-ctx.Done()
	if s.waitToExit != nil {
		<-s.waitToExit
	}
	return nil
}

func (s *groupedService) Stop(ctx context.Context) error {
	return s.stop(ctx)
}

func TestShutdownGroupsKeepDependenciesRunning(t *testing.T) {
	testCtx, cancelTest := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancelTest()
	appCtx, cancelApp := context.WithCancel(testCtx)
	defer cancelApp()

	preStopEntered := make(chan struct{})
	preStopGate := make(chan struct{})
	releasePreStop := sync.OnceFunc(func() { close(preStopGate) })
	defer releasePreStop()
	apiExitGate := make(chan struct{})
	releaseAPI := sync.OnceFunc(func() { close(apiExitGate) })
	defer releaseAPI()
	apiStopGate := make(chan struct{})
	releaseAPIStop := sync.OnceFunc(func() { close(apiStopGate) })
	defer releaseAPIStop()

	apiStopStarted := make(chan struct{})
	apiStopFinished := make(chan struct{})
	workerStopStarted := make(chan struct{})
	dbStopStarted := make(chan struct{})

	api := &groupedService{
		lifetime:   make(chan context.Context, 1),
		waitToExit: apiExitGate,
		stop: func(context.Context) error {
			close(apiStopStarted)
			<-apiStopGate
			close(apiStopFinished)
			return nil
		},
	}
	worker := &groupedService{lifetime: make(chan context.Context, 1), stop: func(context.Context) error {
		close(workerStopStarted)
		return nil
	}}
	db := &groupedService{lifetime: make(chan context.Context, 1), stop: func(context.Context) error {
		close(dbStopStarted)
		return nil
	}}

	started := make(chan struct{})
	ep := entrypoint.New(
		noSignals,
		entrypoint.WithShutdownGroups([]service.Service{api, worker}, []service.Service{db}),
		entrypoint.WithStartupReadiness(time.Second),
		entrypoint.WithShutdownTimeout(3*time.Second),
		entrypoint.WithPostStart(func(context.Context) error {
			close(started)
			return nil
		}),
		entrypoint.WithPreStop(func(context.Context) error {
			close(preStopEntered)
			<-preStopGate
			return nil
		}),
	)
	done := make(chan error, 1)
	go func() { done <- ep.Run(appCtx) }()

	awaitGroupEvent(t, testCtx, started)
	apiCtx, workerCtx, dbCtx := <-api.lifetime, <-worker.lifetime, <-db.lifetime
	cancelApp()
	awaitGroupEvent(t, testCtx, preStopEntered)
	require.NoError(t, apiCtx.Err())
	require.NoError(t, workerCtx.Err())
	require.NoError(t, dbCtx.Err())

	releasePreStop()
	awaitGroupEvent(t, testCtx, apiStopStarted)
	awaitGroupEvent(t, testCtx, workerStopStarted)
	require.ErrorIs(t, apiCtx.Err(), context.Canceled)
	require.ErrorIs(t, workerCtx.Err(), context.Canceled)
	require.NoError(t, dbCtx.Err())

	releaseAPIStop()
	awaitGroupEvent(t, testCtx, apiStopFinished)
	select {
	case <-dbCtx.Done():
		t.Fatal("database was canceled before the API Start returned")
	case <-time.After(25 * time.Millisecond):
	}
	releaseAPI()
	awaitGroupEvent(t, testCtx, dbStopStarted)
	require.ErrorIs(t, dbCtx.Err(), context.Canceled)

	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-testCtx.Done():
		t.Fatal("Run did not return after the groups stopped")
	}
}

func TestShutdownGroupsGiveEachPhaseAFreshDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	releaseBlocked := make(chan struct{})
	defer close(releaseBlocked)

	preStopExpired := make(chan struct{})
	stopExpired := make(chan struct{})
	postStopExpired := make(chan struct{})
	laterStopErr := make(chan error, 1)
	stopEntryErr := make(chan error, 1)
	postStopEntryErr := make(chan error, 1)

	first := &groupedService{lifetime: make(chan context.Context, 1), stop: func(ctx context.Context) error {
		stopEntryErr <- ctx.Err()
		<-ctx.Done()
		close(stopExpired)
		<-releaseBlocked
		return nil
	}}
	last := &groupedService{lifetime: make(chan context.Context, 1), stop: func(ctx context.Context) error {
		laterStopErr <- ctx.Err()
		return nil
	}}

	started := make(chan struct{})
	ep := entrypoint.New(
		noSignals,
		entrypoint.WithShutdownGroups([]service.Service{first}),
		entrypoint.WithServices(last),
		entrypoint.WithShutdownTimeout(150*time.Millisecond),
		entrypoint.WithPostStart(func(context.Context) error {
			close(started)
			return nil
		}),
		entrypoint.WithPreStop(func(ctx context.Context) error {
			<-ctx.Done()
			close(preStopExpired)
			<-releaseBlocked
			return nil
		}),
		entrypoint.WithPostStop(func(ctx context.Context) error {
			postStopEntryErr <- ctx.Err()
			<-ctx.Done()
			close(postStopExpired)
			<-releaseBlocked
			return nil
		}),
	)
	done := make(chan entrypoint.RunResult, 1)
	go func() { done <- ep.RunWithResult(ctx) }()

	awaitGroupEvent(t, ctx, started)
	ep.Shutdown()
	awaitGroupEvent(t, ctx, preStopExpired)
	awaitGroupEvent(t, ctx, stopExpired)
	awaitGroupEvent(t, ctx, postStopExpired)
	require.NoError(t, <-stopEntryErr)
	require.NoError(t, <-postStopEntryErr)
	select {
	case err := <-laterStopErr:
		require.ErrorIs(t, err, context.DeadlineExceeded)
	case <-ctx.Done():
		t.Fatal("the final group was skipped after the shutdown deadline")
	}
	select {
	case result := <-done:
		require.Nil(t, result.Signal)
		require.NoError(t, result.Cause)
		require.ErrorIs(t, result.ShutdownErr, context.DeadlineExceeded)
		assert.ErrorContains(t, result.ShutdownErr, "pre-stop hook: context deadline exceeded")
		assert.ErrorContains(t, result.ShutdownErr, "stopping services: context deadline exceeded")
		assert.ErrorContains(t, result.ShutdownErr, "post-stop hook: context deadline exceeded")
	case <-ctx.Done():
		t.Fatal("RunWithResult did not return after a hook exceeded its deadline")
	}
}

func awaitGroupEvent(t *testing.T, ctx context.Context, event <-chan struct{}) {
	t.Helper()
	select {
	case <-event:
	case <-ctx.Done():
		t.Fatal("shutdown group did not reach the expected event")
	}
}

func TestShutdownTimeout(t *testing.T) {
	stopDelay := 500 * time.Millisecond
	shutdownTimeout := 100 * time.Millisecond
	maxRunTime := 400 * time.Millisecond

	slowSvc := &slowStopService{delay: stopDelay}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ep := entrypoint.New(
		entrypoint.WithServices(slowSvc),
		noSignals,
		entrypoint.WithShutdownTimeout(shutdownTimeout),
	)

	waitBefore := 20 * time.Millisecond
	go func() {
		time.Sleep(waitBefore)
		ep.Shutdown()
	}()

	start := time.Now()
	ep.Run(ctx) //nolint:errcheck

	assert.Less(t, time.Since(start), maxRunTime,
		"Run should return after shutdown timeout, not wait for slow Stop")
}

func TestNoServices(t *testing.T) {
	timeout := 50 * time.Millisecond
	ctx, cancel := cancelAfter(timeout)
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

func (s *slowStopService) Stop(_ context.Context) error {
	time.Sleep(s.delay)
	return nil
}
