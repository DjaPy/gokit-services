package server

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
)

func TestStartWithReadyPropagatesServeFailure(t *testing.T) {
	srv := NewServer(WithHost("127.0.0.1"), WithPort(0), WithPrometheusRegisterer(prometheus.NewRegistry()))
	t.Cleanup(srv.GRPCServer().Stop)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	var closeErr error
	err := srv.StartWithReady(ctx, func() { closeErr = srv.listener.Close() })
	require.NoError(t, closeErr)
	require.ErrorIs(t, err, net.ErrClosed)
}
