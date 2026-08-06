package server_test

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	grpclib "google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"

	grpcsrv "github.com/DjaPy/gokit-services/pkg/grpc/server"
)

// startHealthServer boots a gRPC server exposing the standard health service on
// a random port, returning a dialed client connection. The server and the
// connection are torn down via t.Cleanup.
func startHealthServer(t *testing.T, opts ...grpcsrv.Option) *grpclib.ClientConn {
	t.Helper()
	startTimeout := 500 * time.Millisecond

	opts = append([]grpcsrv.Option{grpcsrv.WithPort(0), grpcsrv.WithHost("127.0.0.1")}, opts...)
	srv := grpcsrv.NewServer(opts...)
	healthpb.RegisterHealthServer(srv.GRPCServer(), health.NewServer())

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go srv.Start(ctx) //nolint:errcheck

	require.Eventually(t, func() bool {
		return srv.Addr() != "127.0.0.1:0"
	}, startTimeout, time.Millisecond)

	conn, err := grpclib.NewClient(srv.Addr(), grpclib.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() }) //nolint:errcheck

	return conn
}

func TestGrpcServer_UnaryMetrics(t *testing.T) {
	appName := "test-svc"
	method := "/grpc.health.v1.Health/Check"

	reg := prometheus.NewRegistry()
	conn := startHealthServer(t, grpcsrv.WithAppName(appName), grpcsrv.WithPrometheusRegisterer(reg))

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := healthpb.NewHealthClient(conn).Check(ctx, &healthpb.HealthCheckRequest{})
	require.NoError(t, err)

	okLabels := map[string]string{
		"grpc_service": appName,
		"grpc_type":    "unary",
		"grpc_method":  method,
		"grpc_code":    "OK",
	}
	assert.Equal(t, 1.0, counterValue(t, reg, "grpc_server_handled_total", okLabels))
	assert.Equal(t, uint64(1), histogramCount(t, reg, "grpc_server_handling_seconds", okLabels))
	// Inflight must return to zero once the RPC completes.
	assert.Equal(t, 0.0, gaugeValue(t, reg, "grpc_server_requests_inflight", map[string]string{
		"grpc_service": appName,
		"grpc_type":    "unary",
		"grpc_method":  method,
	}))
}

func TestGrpcServer_StreamMetrics(t *testing.T) {
	appName := "test-svc"
	method := "/grpc.health.v1.Health/Watch"

	reg := prometheus.NewRegistry()
	conn := startHealthServer(t, grpcsrv.WithAppName(appName), grpcsrv.WithPrometheusRegisterer(reg))

	ctx, cancel := context.WithCancel(context.Background())
	stream, err := healthpb.NewHealthClient(conn).Watch(ctx, &healthpb.HealthCheckRequest{})
	require.NoError(t, err)
	_, err = stream.Recv() // first status push confirms the stream is live
	require.NoError(t, err)
	cancel() // canceling ends the server-side handler

	streamLabels := map[string]string{
		"grpc_service": appName,
		"grpc_type":    "server_stream",
		"grpc_method":  method,
	}
	require.Eventually(t, func() bool {
		return counterValue(t, reg, "grpc_server_handled_total", withCode(streamLabels, "Canceled")) == 1.0
	}, time.Second, 5*time.Millisecond)
	assert.Equal(t, 0.0, gaugeValue(t, reg, "grpc_server_requests_inflight", streamLabels))
}

func withCode(labels map[string]string, code string) map[string]string {
	out := map[string]string{"grpc_code": code}
	for k, v := range labels {
		out[k] = v
	}
	return out
}

func counterValue(t *testing.T, reg *prometheus.Registry, name string, want map[string]string) float64 {
	t.Helper()
	m := findMetric(t, reg, name, want)
	if m == nil {
		return 0
	}
	return m.GetCounter().GetValue()
}

func gaugeValue(t *testing.T, reg *prometheus.Registry, name string, want map[string]string) float64 {
	t.Helper()
	m := findMetric(t, reg, name, want)
	if m == nil {
		return 0
	}
	return m.GetGauge().GetValue()
}

func histogramCount(t *testing.T, reg *prometheus.Registry, name string, want map[string]string) uint64 {
	t.Helper()
	m := findMetric(t, reg, name, want)
	if m == nil {
		return 0
	}
	return m.GetHistogram().GetSampleCount()
}

func findMetric(t *testing.T, reg *prometheus.Registry, name string, want map[string]string) *dto.Metric {
	t.Helper()
	mfs, err := reg.Gather()
	require.NoError(t, err)
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			if labelsMatch(m, want) {
				return m
			}
		}
	}
	return nil
}

func labelsMatch(m *dto.Metric, want map[string]string) bool {
	got := make(map[string]string, len(m.GetLabel()))
	for _, lp := range m.GetLabel() {
		got[lp.GetName()] = lp.GetValue()
	}
	for k, v := range want {
		if got[k] != v {
			return false
		}
	}
	return true
}
