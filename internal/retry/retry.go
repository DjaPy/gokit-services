// Package retry provides a generic exponential-backoff retry helper used by
// managed infrastructure clients (dbservice, kafkaproducer, kafkaconsumer,
// redisservice) that connect to external dependencies at Start — unlike
// httpserver/grpcserver, those are expected to retry transient startup
// failures rather than fail fast.
package retry

import (
	"context"
	"fmt"
	"time"
)

// Config configures the retry loop.
type Config struct {
	MaxAttempts int
	BackoffMin  time.Duration
	BackoffMax  time.Duration
}

// DefaultConfig returns a reasonable default retry policy.
func DefaultConfig() Config {
	return Config{
		MaxAttempts: 10,
		BackoffMin:  500 * time.Millisecond,
		BackoffMax:  30 * time.Second,
	}
}

const backoffMultiplier = 1.5

// Do calls connect until it succeeds, ctx is canceled, or cfg.MaxAttempts is
// exhausted. Between attempts it waits with exponential backoff starting at
// cfg.BackoffMin and capped at cfg.BackoffMax. MaxAttempts <= 0 is treated
// as 1 (a single attempt, no retries).
func Do[T any](ctx context.Context, cfg Config, connect func(context.Context) (T, error)) (T, error) {
	maxAttempts := cfg.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = 1
	}

	backoff := cfg.BackoffMin
	var lastErr error
	var zero T

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		result, err := connect(ctx)
		if err == nil {
			return result, nil
		}
		lastErr = err

		if attempt == maxAttempts {
			break
		}

		select {
		case <-ctx.Done():
			return zero, fmt.Errorf("retry: context canceled after %d attempts: %w", attempt, ctx.Err())
		case <-time.After(backoff):
		}

		backoff = min(time.Duration(float64(backoff)*backoffMultiplier), cfg.BackoffMax)
	}

	return zero, fmt.Errorf("retry: failed after %d attempts: %w", maxAttempts, lastErr)
}
