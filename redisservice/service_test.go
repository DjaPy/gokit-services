package redisservice_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/DjaPy/gokit-services/internal/retry"
	"github.com/DjaPy/gokit-services/redisservice"
)

func testDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("TEST_REDIS_DSN")
	if dsn == "" {
		t.Skip("TEST_REDIS_DSN not set; skipping test requiring a real Redis instance")
	}
	return dsn
}

func TestService_StartConnectsAndProbeSucceeds(t *testing.T) {
	startTimeout := 5 * time.Second
	pollInterval := 10 * time.Millisecond

	svc := redisservice.New(testDSN(t), redisservice.WithPrometheusRegisterer(prometheus.NewRegistry()))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go svc.Start(ctx) //nolint:errcheck // exercised via Probe below

	require.Eventually(t, func() bool {
		return svc.Client() != nil
	}, startTimeout, pollInterval)

	require.NoError(t, svc.Probe(context.Background()))
}

func TestService_ProbeBeforeStart_ReturnsError(t *testing.T) {
	svc := redisservice.New(testDSN(t), redisservice.WithPrometheusRegisterer(prometheus.NewRegistry()))

	err := svc.Probe(context.Background())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "not connected")
}

func TestService_StartRetriesOnInitialFailure(t *testing.T) {
	maxAttempts := 3
	startTimeout := 5 * time.Second
	unreachableDSN := "redis://localhost:1"

	svc := redisservice.New(unreachableDSN,
		redisservice.WithPrometheusRegisterer(prometheus.NewRegistry()),
		redisservice.WithRetry(retry.Config{
			MaxAttempts: maxAttempts,
			BackoffMin:  10 * time.Millisecond,
			BackoffMax:  50 * time.Millisecond,
		}),
	)

	ctx, cancel := context.WithTimeout(context.Background(), startTimeout)
	defer cancel()

	err := svc.Start(ctx)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "redisservice: connect")
}

func TestService_StopClosesClient(t *testing.T) {
	startTimeout := 5 * time.Second
	pollInterval := 10 * time.Millisecond

	svc := redisservice.New(testDSN(t), redisservice.WithPrometheusRegisterer(prometheus.NewRegistry()))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go svc.Start(ctx) //nolint:errcheck

	require.Eventually(t, func() bool {
		return svc.Client() != nil
	}, startTimeout, pollInterval)

	require.NoError(t, svc.Stop(context.Background(), nil))

	err := svc.Client().Ping(context.Background()).Err()
	assert.Error(t, err)
}

func TestService_StopIdempotent(t *testing.T) {
	startTimeout := 5 * time.Second
	pollInterval := 10 * time.Millisecond

	svc := redisservice.New(testDSN(t), redisservice.WithPrometheusRegisterer(prometheus.NewRegistry()))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go svc.Start(ctx) //nolint:errcheck

	require.Eventually(t, func() bool {
		return svc.Client() != nil
	}, startTimeout, pollInterval)

	require.NoError(t, svc.Stop(context.Background(), nil))
	require.NoError(t, svc.Stop(context.Background(), nil))
	require.NoError(t, svc.Stop(context.Background(), nil))
}
