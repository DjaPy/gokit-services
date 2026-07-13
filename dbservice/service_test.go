package dbservice_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/DjaPy/gokit-services/dbservice"
	"github.com/DjaPy/gokit-services/internal/retry"
)

func testDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN not set; skipping test requiring a real PostgreSQL instance")
	}
	return dsn
}

func TestService_StartConnectsAndProbeSucceeds(t *testing.T) {
	startTimeout := 5 * time.Second
	svc := dbservice.New(testDSN(t), dbservice.WithPrometheusRegisterer(prometheus.NewRegistry()))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go svc.Start(ctx) //nolint:errcheck // exercised via Probe below

	require.Eventually(t, func() bool {
		return svc.Pool() != nil
	}, startTimeout, 10*time.Millisecond)

	require.NoError(t, svc.Probe(context.Background()))
}

func TestService_ProbeBeforeStart_ReturnsError(t *testing.T) {
	svc := dbservice.New(testDSN(t), dbservice.WithPrometheusRegisterer(prometheus.NewRegistry()))

	err := svc.Probe(context.Background())

	require.Error(t, err)
	assert.Contains(t, err.Error(), "not connected")
}

func TestService_StartRetriesOnInitialFailure(t *testing.T) {
	const maxAttempts = 3
	unreachableDSN := "postgresql://user:pass@localhost:1/db"
	svc := dbservice.New(unreachableDSN,
		dbservice.WithPrometheusRegisterer(prometheus.NewRegistry()),
		dbservice.WithRetry(retry.Config{
			MaxAttempts: maxAttempts,
			BackoffMin:  10 * time.Millisecond,
			BackoffMax:  50 * time.Millisecond,
		}),
	)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := svc.Start(ctx)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "dbservice: connect")
}

func TestService_StopClosesPool(t *testing.T) {
	startTimeout := 5 * time.Second
	svc := dbservice.New(testDSN(t), dbservice.WithPrometheusRegisterer(prometheus.NewRegistry()))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go svc.Start(ctx) //nolint:errcheck

	require.Eventually(t, func() bool {
		return svc.Pool() != nil
	}, startTimeout, 10*time.Millisecond)

	require.NoError(t, svc.Stop(context.Background(), nil))

	err := svc.Probe(context.Background())
	assert.Error(t, err)
}

func TestService_StopIdempotent(t *testing.T) {
	startTimeout := 5 * time.Second
	svc := dbservice.New(testDSN(t), dbservice.WithPrometheusRegisterer(prometheus.NewRegistry()))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go svc.Start(ctx) //nolint:errcheck

	require.Eventually(t, func() bool {
		return svc.Pool() != nil
	}, startTimeout, 10*time.Millisecond)

	require.NoError(t, svc.Stop(context.Background(), nil))
	require.NoError(t, svc.Stop(context.Background(), nil))
	require.NoError(t, svc.Stop(context.Background(), nil))
}
