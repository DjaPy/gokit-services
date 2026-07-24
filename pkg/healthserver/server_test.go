package healthserver_test

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/DjaPy/gokit-services/pkg/healthserver"
)

// proberFunc is a test helper that implements service.Prober via a plain function.
type proberFunc func(ctx context.Context) error

func (f proberFunc) Probe(ctx context.Context) error { return f(ctx) }

// startServer starts a HealthServer on a free port and returns the base URL.
// The server is stopped when the test ends via t.Cleanup.
func startServer(t *testing.T, extra ...healthserver.Option) string {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()

	opts := append([]healthserver.Option{
		healthserver.WithPrometheusRegisterer(prometheus.NewRegistry()),
		healthserver.WithHost("127.0.0.1"),
		healthserver.WithPort(port),
	}, extra...)

	srv := healthserver.New(opts...)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	go srv.Start(ctx) //nolint:errcheck

	baseURL := "http://127.0.0.1:" + strconv.Itoa(port)
	waitFor := 200 * time.Millisecond
	require.Eventually(t, func() bool {
		resp, err := http.Get(baseURL + "/healthz") //nolint:noctx
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	}, waitFor, time.Millisecond)

	return baseURL
}

func TestHealthServer_Healthz_Always200(t *testing.T) {
	baseURL := startServer(t)

	resp, err := http.Get(baseURL + "/healthz") //nolint:noctx
	require.NoError(t, err)
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	expectedStatus := http.StatusOK
	expectedBody := `{"status":"ok"}`
	assert.Equal(t, expectedStatus, resp.StatusCode)
	assert.JSONEq(t, expectedBody, string(body))
}

func TestHealthServer_Readyz_NoProbers_200(t *testing.T) {
	baseURL := startServer(t)

	resp, err := http.Get(baseURL + "/readyz") //nolint:noctx
	require.NoError(t, err)
	defer resp.Body.Close()

	expectedStatus := http.StatusOK
	assert.Equal(t, expectedStatus, resp.StatusCode)
}

func TestHealthServer_Readyz_AllProbersPass_200(t *testing.T) {
	passing := proberFunc(func(_ context.Context) error { return nil })
	baseURL := startServer(t,
		healthserver.WithProber(passing),
		healthserver.WithProber(passing),
	)

	resp, err := http.Get(baseURL + "/readyz") //nolint:noctx
	require.NoError(t, err)
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	expectedStatus := http.StatusOK
	expectedBody := `{"status":"ready"}`
	assert.Equal(t, expectedStatus, resp.StatusCode)
	assert.JSONEq(t, expectedBody, string(body))
}

func TestHealthServer_Readyz_SomeProberFails_503(t *testing.T) {
	passing := proberFunc(func(_ context.Context) error { return nil })
	failing := proberFunc(func(_ context.Context) error { return errors.New("db down") })
	baseURL := startServer(t,
		healthserver.WithProber(passing),
		healthserver.WithProber(failing),
	)

	resp, err := http.Get(baseURL + "/readyz") //nolint:noctx
	require.NoError(t, err)
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	expectedStatus := http.StatusServiceUnavailable
	expectedErrMsg := "db down"
	assert.Equal(t, expectedStatus, resp.StatusCode)
	assert.Contains(t, string(body), expectedErrMsg)
}

func TestHealthServer_Stop(t *testing.T) {
	startWaitFor := 200 * time.Millisecond
	stopTimeout := 500 * time.Millisecond

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()

	srv := healthserver.New(
		healthserver.WithPrometheusRegisterer(prometheus.NewRegistry()),
		healthserver.WithHost("127.0.0.1"),
		healthserver.WithPort(port),
	)

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- srv.Start(ctx) }()

	baseURL := "http://127.0.0.1:" + strconv.Itoa(port)
	require.Eventually(t, func() bool {
		resp, err := http.Get(baseURL + "/healthz") //nolint:noctx
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	}, startWaitFor, time.Millisecond)

	cancel()

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(stopTimeout):
		t.Fatal("Start did not return after context cancel")
	}
}
