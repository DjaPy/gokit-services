package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// metricHistogramCount returns the sample count for the first metric in the named histogram family.
// Uses type inference on dto.MetricFamily — no direct import of client_model needed.
func metricHistogramCount(t *testing.T, reg *prometheus.Registry, name string) uint64 {
	t.Helper()
	mfs, err := reg.Gather()
	require.NoError(t, err)
	for _, mf := range mfs {
		if mf.GetName() == name && len(mf.GetMetric()) > 0 {
			return mf.GetMetric()[0].GetHistogram().GetSampleCount()
		}
	}
	return 0
}

// metricGaugeValue returns the value of the first metric in the named gauge family.
func metricGaugeValue(t *testing.T, reg *prometheus.Registry, name string) float64 {
	t.Helper()
	mfs, err := reg.Gather()
	require.NoError(t, err)
	for _, mf := range mfs {
		if mf.GetName() == name && len(mf.GetMetric()) > 0 {
			return mf.GetMetric()[0].GetGauge().GetValue()
		}
	}
	return 0
}

// metricCounterValue returns the value of the first metric in the named counter family.
func metricCounterValue(t *testing.T, reg *prometheus.Registry, name string) float64 {
	t.Helper()
	mfs, err := reg.Gather()
	require.NoError(t, err)
	for _, mf := range mfs {
		if mf.GetName() == name && len(mf.GetMetric()) > 0 {
			return mf.GetMetric()[0].GetCounter().GetValue()
		}
	}
	return 0
}

// newTestServer creates a Server backed by an isolated Prometheus registry.
func newTestServer(handler http.Handler, opts ...Option) (*Server, *prometheus.Registry) {
	reg := prometheus.NewRegistry()
	opts = append([]Option{WithPrometheusRegisterer(reg)}, opts...)
	return NewServer(handler, opts...), reg
}

// TestNewServer_Defaults verifies the default field values after construction.
func TestNewServer_Defaults(t *testing.T) {
	srv, _ := newTestServer(http.NewServeMux())

	assert.Equal(t, "0.0.0.0", srv.host)
	assert.Equal(t, 8080, srv.port)
	assert.Equal(t, 5*time.Second, srv.readTimeout)
	assert.Equal(t, 10*time.Second, srv.writeTimeout)
	assert.Equal(t, 120*time.Second, srv.idleTimeout)
	assert.Equal(t, "", srv.appName)
	assert.NotNil(t, srv.logger)
}

func TestWithPort(t *testing.T) {
	srv, _ := newTestServer(http.NewServeMux(), WithPort(9090))
	assert.Equal(t, 9090, srv.port)
}

func TestWithHost(t *testing.T) {
	srv, _ := newTestServer(http.NewServeMux(), WithHost("127.0.0.1"))
	assert.Equal(t, "127.0.0.1", srv.host)
}

func TestWithReadTimeout(t *testing.T) {
	srv, _ := newTestServer(http.NewServeMux(), WithReadTimeout(3*time.Second))
	assert.Equal(t, 3*time.Second, srv.readTimeout)
}

func TestWithWriteTimeout(t *testing.T) {
	srv, _ := newTestServer(http.NewServeMux(), WithWriteTimeout(7*time.Second))
	assert.Equal(t, 7*time.Second, srv.writeTimeout)
}

func TestWithIdleTimeout(t *testing.T) {
	srv, _ := newTestServer(http.NewServeMux(), WithIdleTimeout(60*time.Second))
	assert.Equal(t, 60*time.Second, srv.idleTimeout)
}

func TestWithAppName(t *testing.T) {
	srv, _ := newTestServer(http.NewServeMux(), WithAppName("my-svc"))
	assert.Equal(t, "my-svc", srv.appName)
}

// TestWriteProblem verifies the Content-Type, status code, and JSON body.
func TestWriteProblem(t *testing.T) {
	p := &Problem{
		Title:    "Not Found",
		Status:   http.StatusNotFound,
		Instance: "/users/42",
	}

	rw := httptest.NewRecorder()
	WriteProblem(rw, p)

	assert.Equal(t, http.StatusNotFound, rw.Code)
	assert.Equal(t, problemContentType, rw.Header().Get("Content-Type"))

	var got Problem
	require.NoError(t, json.Unmarshal(rw.Body.Bytes(), &got))
	assert.Equal(t, "Not Found", got.Title)
	assert.Equal(t, http.StatusNotFound, got.Status)
	assert.Equal(t, "/users/42", got.Instance)
}

// TestWriteProblem_OmitsEmptyFields ensures omitempty fields are absent from JSON.
func TestWriteProblem_OmitsEmptyFields(t *testing.T) {
	rw := httptest.NewRecorder()
	WriteProblem(rw, &Problem{Status: http.StatusOK, Title: "OK"})

	body := rw.Body.Bytes()
	assert.NotContains(t, string(body), `"type"`)
	assert.NotContains(t, string(body), `"instance"`)
	assert.NotContains(t, string(body), `"detail"`)
}

// TestMiddleware_RecordsRequestDuration verifies the duration histogram is observed.
func TestMiddleware_RecordsRequestDuration(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	srv, reg := newTestServer(handler)

	req := httptest.NewRequest(http.MethodGet, "/ping", nil)
	rw := httptest.NewRecorder()
	srv.buildMiddleware().ServeHTTP(rw, req)

	assert.Equal(t, uint64(1), metricHistogramCount(t, reg, "http_request_duration_seconds"))
}

// TestMiddleware_RecordsResponseSize verifies the response size histogram is observed.
func TestMiddleware_RecordsResponseSize(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, "hello")
	})
	srv, reg := newTestServer(handler)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rw := httptest.NewRecorder()
	srv.buildMiddleware().ServeHTTP(rw, req)

	assert.Equal(t, uint64(1), metricHistogramCount(t, reg, "http_response_size_bytes"))
}

// TestMiddleware_InflightBackToZero verifies inflight gauge returns to 0 after the request.
func TestMiddleware_InflightBackToZero(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	srv, reg := newTestServer(handler)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rw := httptest.NewRecorder()
	srv.buildMiddleware().ServeHTTP(rw, req)

	assert.Equal(t, float64(0), metricGaugeValue(t, reg, "http_requests_inflight"))
}

// TestMiddleware_PanicRecovery verifies that a panicking handler returns a 500 Problem response.
func TestMiddleware_PanicRecovery(t *testing.T) {
	handler := http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		panic("something went wrong")
	})
	srv, reg := newTestServer(handler, WithAppName("test-svc"))

	req := httptest.NewRequest(http.MethodGet, "/boom", nil)
	rw := httptest.NewRecorder()
	srv.buildMiddleware().ServeHTTP(rw, req)

	assert.Equal(t, http.StatusInternalServerError, rw.Code)
	assert.Equal(t, problemContentType, rw.Header().Get("Content-Type"))

	var p Problem
	require.NoError(t, json.Unmarshal(rw.Body.Bytes(), &p))
	assert.Equal(t, http.StatusInternalServerError, p.Status)
	assert.Equal(t, "/boom", p.Instance)

	assert.Equal(t, float64(1), metricCounterValue(t, reg, "http_panic_recovery_total"))
}

// TestMiddleware_PanicAfterWrite does not overwrite a response that was already started.
func TestMiddleware_PanicAfterWrite(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		panic("panic after headers sent")
	})
	srv, _ := newTestServer(handler)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rw := httptest.NewRecorder()
	srv.buildMiddleware().ServeHTTP(rw, req)

	assert.Equal(t, http.StatusAccepted, rw.Code)
}

// TestServer_StartStop verifies the full lifecycle: bind, serve, graceful stop.
func TestServer_StartStop(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	srv, _ := newTestServer(mux, WithHost("127.0.0.1"), WithPort(0))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		require.NoError(t, srv.Start(ctx))
	}()

	require.Eventually(t, func() bool {
		return srv.Addr() != "127.0.0.1:0"
	}, time.Second, 5*time.Millisecond)

	resp, err := http.Get("http://" + srv.Addr() + "/health") //nolint:noctx
	require.NoError(t, err)
	_ = resp.Body.Close()
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	stopCtx, stopCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer stopCancel()
	require.NoError(t, srv.Stop(stopCtx, nil))

	wg.Wait()
}

// TestServer_ContextCancellationStops verifies that cancelling the ctx stops the server.
func TestServer_ContextCancellationStops(t *testing.T) {
	srv, _ := newTestServer(http.NewServeMux(), WithHost("127.0.0.1"), WithPort(0))

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- srv.Start(ctx) }()

	require.Eventually(t, func() bool {
		return srv.Addr() != "127.0.0.1:0"
	}, time.Second, 5*time.Millisecond)

	cancel()

	select {
	case err := <-done:
		assert.NoError(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("server did not stop after ctx cancellation")
	}
}

// TestServer_Addr_BeforeStart returns the configured address before Start is called.
func TestServer_Addr_BeforeStart(t *testing.T) {
	srv, _ := newTestServer(http.NewServeMux(), WithHost("127.0.0.1"), WithPort(9999))
	assert.Equal(t, "127.0.0.1:9999", srv.Addr())
}

// TestServer_Addr_AfterStart returns the actual bound address (useful with port 0).
func TestServer_Addr_AfterStart(t *testing.T) {
	srv, _ := newTestServer(http.NewServeMux(), WithHost("127.0.0.1"), WithPort(0))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go srv.Start(ctx) //nolint:errcheck

	require.Eventually(t, func() bool {
		return srv.Addr() != "127.0.0.1:0"
	}, time.Second, 5*time.Millisecond)

	addr := srv.Addr()
	assert.NotEqual(t, "127.0.0.1:0", addr, "Addr() should return actual bound port, not 0")
}
