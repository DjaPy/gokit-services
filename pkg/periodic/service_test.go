package periodic_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/DjaPy/gokit-services/pkg/periodic"
)

func TestPeriodic_CallbackCalledMultipleTimes(t *testing.T) {
	const (
		interval         = 10 * time.Millisecond
		runFor           = 55 * time.Millisecond
		minExpectedCalls = int32(4)
	)
	var count atomic.Int32
	svc := periodic.New(interval, func(_ context.Context) error {
		count.Add(1)
		return nil
	})

	ctx, cancel := context.WithTimeout(context.Background(), runFor)
	defer cancel()

	require.NoError(t, svc.Start(ctx))
	assert.GreaterOrEqual(t, count.Load(), minExpectedCalls)
}

func TestPeriodic_NonOverlapping_SkipsTick(t *testing.T) {
	const (
		interval         = 10 * time.Millisecond
		fnDuration       = 30 * time.Millisecond
		runFor           = 60 * time.Millisecond
		maxExpectedCalls = int32(2)
	)
	var count atomic.Int32
	svc := periodic.New(interval, func(ctx context.Context) error {
		count.Add(1)
		select {
		case <-time.After(fnDuration):
		case <-ctx.Done():
		}
		return nil
	})

	ctx, cancel := context.WithTimeout(context.Background(), runFor)
	defer cancel()

	require.NoError(t, svc.Start(ctx))
	assert.LessOrEqual(t, count.Load(), maxExpectedCalls)
}

func TestPeriodic_ImmediateStart(t *testing.T) {
	const (
		interval         = 1 * time.Hour // long interval so only immediateStart fires
		runFor           = 50 * time.Millisecond
		minExpectedCalls = int32(1)
	)
	var count atomic.Int32
	svc := periodic.New(interval, func(_ context.Context) error {
		count.Add(1)
		return nil
	}, periodic.WithImmediateStart())

	ctx, cancel := context.WithTimeout(context.Background(), runFor)
	defer cancel()

	require.NoError(t, svc.Start(ctx))
	assert.GreaterOrEqual(t, count.Load(), minExpectedCalls)
}

func TestPeriodic_StopsOnCtxCancel(t *testing.T) {
	const (
		interval      = 10 * time.Millisecond
		runFor        = 35 * time.Millisecond
		waitAfterStop = 20 * time.Millisecond
	)
	var count atomic.Int32
	svc := periodic.New(interval, func(_ context.Context) error {
		count.Add(1)
		return nil
	})

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		defer close(done)
		svc.Start(ctx) //nolint:errcheck
	}()

	time.Sleep(runFor)
	cancel()
	<-done

	countAfterStop := count.Load()
	time.Sleep(waitAfterStop)
	assert.Equal(t, countAfterStop, count.Load(), "fn must not be called after ctx cancellation")
}

func TestPeriodic_Overlapping_AllowsConcurrentInvocations(t *testing.T) {
	var (
		interval      = 10 * time.Millisecond
		fnDuration    = 30 * time.Millisecond
		runFor        = 60 * time.Millisecond
		minConcurrent = int32(2)
	)
	var (
		current atomic.Int32
		peak    atomic.Int32
	)
	svc := periodic.New(interval, func(ctx context.Context) error {
		n := current.Add(1)
		for {
			old := peak.Load()
			if n <= old || peak.CompareAndSwap(old, n) {
				break
			}
		}
		select {
		case <-time.After(fnDuration):
		case <-ctx.Done():
		}
		current.Add(-1)
		return nil
	}, periodic.WithOverlapping())

	ctx, cancel := context.WithTimeout(context.Background(), runFor)
	defer cancel()

	require.NoError(t, svc.Start(ctx))
	assert.GreaterOrEqual(t, peak.Load(), minConcurrent, "overlapping mode must allow concurrent fn calls")
}

func TestPeriodic_ErrorInFn_DoesNotStop(t *testing.T) {
	const (
		interval         = 10 * time.Millisecond
		runFor           = 55 * time.Millisecond
		minExpectedCalls = int32(4)
	)
	var count atomic.Int32
	svc := periodic.New(interval, func(_ context.Context) error {
		count.Add(1)
		return errors.New("always fails")
	})

	ctx, cancel := context.WithTimeout(context.Background(), runFor)
	defer cancel()

	require.NoError(t, svc.Start(ctx))
	assert.GreaterOrEqual(t, count.Load(), minExpectedCalls)
}
