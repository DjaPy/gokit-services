package retry_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/DjaPy/gokit-services/pkg/internal/retry"
)

var errConnect = errors.New("connect failed")

func TestDo_SucceedsFirstAttempt(t *testing.T) {
	var calls atomic.Int32
	ctx := context.Background()

	result, err := retry.Do(ctx, retry.DefaultConfig(), func(_ context.Context) (int, error) {
		calls.Add(1)
		return 42, nil
	})

	require.NoError(t, err)
	assert.Equal(t, 42, result)
	assert.Equal(t, int32(1), calls.Load())
}

func TestDo_SucceedsAfterRetries(t *testing.T) {
	const failuresBeforeSuccess = 3
	var calls atomic.Int32
	ctx := context.Background()
	cfg := retry.Config{MaxAttempts: 10, BackoffMin: time.Millisecond, BackoffMax: 5 * time.Millisecond}

	result, err := retry.Do(ctx, cfg, func(_ context.Context) (string, error) {
		n := calls.Add(1)
		if n <= failuresBeforeSuccess {
			return "", errConnect
		}
		return "ok", nil
	})

	require.NoError(t, err)
	assert.Equal(t, "ok", result)
	assert.Equal(t, int32(failuresBeforeSuccess+1), calls.Load())
}

func TestDo_ExhaustsMaxAttempts(t *testing.T) {
	const maxAttempts = 4
	var calls atomic.Int32
	ctx := context.Background()
	cfg := retry.Config{MaxAttempts: maxAttempts, BackoffMin: time.Millisecond, BackoffMax: 5 * time.Millisecond}

	_, err := retry.Do(ctx, cfg, func(_ context.Context) (int, error) {
		calls.Add(1)
		return 0, errConnect
	})

	require.Error(t, err)
	assert.ErrorIs(t, err, errConnect)
	assert.Equal(t, int32(maxAttempts), calls.Load())
}

func TestDo_RespectsContextCancellation(t *testing.T) {
	cancelAfter := 20 * time.Millisecond
	cfg := retry.Config{MaxAttempts: 1000, BackoffMin: 50 * time.Millisecond, BackoffMax: 50 * time.Millisecond}

	ctx, cancel := context.WithTimeout(context.Background(), cancelAfter)
	defer cancel()

	_, err := retry.Do(ctx, cfg, func(_ context.Context) (int, error) {
		return 0, errConnect
	})

	require.Error(t, err)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestDo_BackoffCappedAtMax(t *testing.T) {
	const maxAttempts = 6
	testTimeout := 2 * time.Second
	cfg := retry.Config{MaxAttempts: maxAttempts, BackoffMin: 10 * time.Millisecond, BackoffMax: 20 * time.Millisecond}
	ctx := context.Background()

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = retry.Do(ctx, cfg, func(_ context.Context) (int, error) {
			return 0, errConnect
		})
	}()

	require.Eventually(t, func() bool {
		select {
		case <-done:
			return true
		default:
			return false
		}
	}, testTimeout, 10*time.Millisecond, "backoff should stay capped, not grow unbounded")
}
