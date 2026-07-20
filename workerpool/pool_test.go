package workerpool_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/DjaPy/gokit-services/core/service"
	"github.com/DjaPy/gokit-services/workerpool"
)

func TestPool_ImplementsServiceAndShutdown(t *testing.T) {
	pool := workerpool.New(1)
	var _ service.Service = pool
	var _ service.Shutdown = pool
}

func TestPool_AllTasksExecuted(t *testing.T) {
	const (
		workerCount = 5
		taskCount   = 100
		waitFor     = 2 * time.Second
	)
	var executed atomic.Int32

	pool := workerpool.New(workerCount)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- pool.Start(ctx) }()

	submitCtx := context.Background()
	for range taskCount {
		require.NoError(t, pool.Submit(submitCtx, func(_ context.Context) {
			executed.Add(1)
		}))
	}

	require.Eventually(t, func() bool {
		return executed.Load() == int32(taskCount)
	}, waitFor, time.Millisecond)

	cancel()
	require.NoError(t, <-done)
	assert.Equal(t, int32(taskCount), executed.Load())
}

func TestPool_Submit_ReturnsCancelledOnDoneCtx(t *testing.T) {
	const workerCount = 1

	pool := workerpool.New(workerCount, workerpool.WithQueueSize(1))

	poolCtx, poolCancel := context.WithCancel(context.Background())
	defer poolCancel()
	go pool.Start(poolCtx) //nolint:errcheck

	// fill the queue with a blocking task so Submit has nowhere to put new tasks
	blockRelease := make(chan struct{})
	_ = pool.Submit(context.Background(), func(_ context.Context) {
		<-blockRelease
	})
	_ = pool.Submit(context.Background(), func(_ context.Context) {
		<-blockRelease
	})

	cancelledCtx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled

	err := pool.Submit(cancelledCtx, func(_ context.Context) {})
	assert.ErrorIs(t, err, context.Canceled)

	close(blockRelease)
}

func TestPool_DrainOnStop_WaitsForTasks(t *testing.T) {
	const (
		workerCount = 2
		taskCount   = 4
		taskDelay   = 20 * time.Millisecond
		drainBudget = 500 * time.Millisecond
	)
	var executed atomic.Int32

	pool := workerpool.New(workerCount, workerpool.WithDrainOnStop())
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- pool.Start(ctx) }()

	for range taskCount {
		require.NoError(t, pool.Submit(context.Background(), func(_ context.Context) {
			time.Sleep(taskDelay)
			executed.Add(1)
		}))
	}

	cancel()

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(drainBudget):
		t.Fatal("Start did not return within drain budget")
	}

	assert.Equal(t, int32(taskCount), executed.Load(), "all tasks must complete before Start returns")
}

func TestPool_NoDrainOnStop_ReturnsImmediately(t *testing.T) {
	const (
		workerCount   = 1
		taskDelay     = 200 * time.Millisecond
		maxReturnTime = 50 * time.Millisecond
	)

	pool := workerpool.New(workerCount) // no WithDrainOnStop
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- pool.Start(ctx) }()

	blockRelease := make(chan struct{})
	_ = pool.Submit(context.Background(), func(_ context.Context) {
		select {
		case <-blockRelease:
		case <-time.After(taskDelay):
		}
	})

	cancel()

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(maxReturnTime):
		t.Fatal("Start should return quickly without draining")
	}
	close(blockRelease)
}

func TestPool_SubmitAfterCtxCancel_ReturnsError(t *testing.T) {
	pool := workerpool.New(1)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- pool.Start(ctx) }()

	cancel()
	require.NoError(t, <-done)

	err := pool.Submit(ctx, func(_ context.Context) {})
	assert.Error(t, err, "Submit with cancelled ctx must return an error")
}
