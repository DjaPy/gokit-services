package client_test

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

	grpccli "github.com/DjaPy/gokit-services/pkg/grpc/client"
	grpcsrv "github.com/DjaPy/gokit-services/pkg/grpc/server"
)

// startInstrumentedClient boots a health server and a managed client wired to a
// fresh registry, returning the client's connection. Everything is torn down via
// t.Cleanup.
func startInstrumentedClient(t *testing.T, reg prometheus.Registerer, appName string) *grpclib.ClientConn {
	t.Helper()
	timeout := 500 * time.Millisecond

	srv := grpcsrv.NewServer(
		grpcsrv.WithPort(0), grpcsrv.WithHost("127.0.0.1"),
		grpcsrv.WithPrometheusRegisterer(prometheus.NewRegistry()), // isolate server metrics
	)
	healthpb.RegisterHealthServer(srv.GRPCServer(), health.NewServer())
	srvCtx, srvCancel := context.WithCancel(context.Background())
	t.Cleanup(srvCancel)
	go srv.Start(srvCtx) //nolint:errcheck
	require.Eventually(t, func() bool {
		return srv.Addr() != "127.0.0.1:0"
	}, timeout, time.Millisecond)

	c := grpccli.NewClient(srv.Addr(),
		grpccli.WithAppName(appName),
		grpccli.WithPrometheusRegisterer(reg),
		grpccli.WithDialOptions(grpclib.WithTransportCredentials(insecure.NewCredentials())),
	)
	cliCtx, cliCancel := context.WithCancel(context.Background())
	t.Cleanup(cliCancel)
	go c.Start(cliCtx) //nolint:errcheck
	require.Eventually(t, func() bool {
		return c.Conn() != nil
	}, timeout, time.Millisecond)

	return c.Conn()
}

func TestGrpcClient_UnaryMetrics(t *testing.T) {
	appName := "test-cli"
	method := "/grpc.health.v1.Health/Check"

	reg := prometheus.NewRegistry()
	conn := startInstrumentedClient(t, reg, appName)

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
	assert.Equal(t, 1.0, counterValue(t, reg, "grpc_client_handled_total", okLabels))
	assert.Equal(t, uint64(1), histogramCount(t, reg, "grpc_client_handling_seconds", okLabels))
	assert.Equal(t, 0.0, gaugeValue(t, reg, "grpc_client_requests_inflight", map[string]string{
		"grpc_service": appName,
		"grpc_type":    "unary",
		"grpc_method":  method,
	}))
}

func TestGrpcClient_StreamMetrics(t *testing.T) {
	appName := "test-cli"
	method := "/grpc.health.v1.Health/Watch"

	reg := prometheus.NewRegistry()
	conn := startInstrumentedClient(t, reg, appName)

	ctx, cancel := context.WithCancel(context.Background())
	stream, err := healthpb.NewHealthClient(conn).Watch(ctx, &healthpb.HealthCheckRequest{})
	require.NoError(t, err)
	_, err = stream.Recv() // first push confirms the stream is live
	require.NoError(t, err)

	cancel()
	_, err = stream.Recv() // terminal Recv drives the stream metric to completion
	require.Error(t, err)

	streamLabels := map[string]string{
		"grpc_service": appName,
		"grpc_type":    "server_stream",
		"grpc_method":  method,
		"grpc_code":    "Canceled",
	}
	assert.Equal(t, 1.0, counterValue(t, reg, "grpc_client_handled_total", streamLabels))
	assert.Equal(t, 0.0, gaugeValue(t, reg, "grpc_client_requests_inflight", map[string]string{
		"grpc_service": appName,
		"grpc_type":    "server_stream",
		"grpc_method":  method,
	}))
}

// --- metric gathering helpers ---

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
