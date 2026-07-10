package grpcclient_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	grpclib "google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/DjaPy/gokit-services/grpcclient"
	"github.com/DjaPy/gokit-services/grpcserver"
)

func TestGrpcClient_ConnAvailableAfterStart(t *testing.T) {
	startTimeout := 200 * time.Millisecond
	stopTimeout := 500 * time.Millisecond

	srv := grpcserver.NewServer(grpcserver.WithPort(0), grpcserver.WithHost("127.0.0.1"))
	srvCtx, srvCancel := context.WithCancel(context.Background())
	defer srvCancel()
	go srv.Start(srvCtx) //nolint:errcheck

	require.Eventually(t, func() bool {
		return srv.Addr() != "127.0.0.1:0"
	}, startTimeout, time.Millisecond)

	c := grpcclient.NewClient(srv.Addr(),
		grpcclient.WithDialOptions(grpclib.WithTransportCredentials(insecure.NewCredentials())),
	)

	assert.Nil(t, c.Conn(), "Conn() must be nil before Start")

	clientCtx, clientCancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- c.Start(clientCtx) }()

	require.Eventually(t, func() bool {
		return c.Conn() != nil
	}, startTimeout, time.Millisecond)

	clientCancel()

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(stopTimeout):
		t.Fatal("Start did not return after context cancel")
	}
}

func TestGrpcClient_StopClosesConn(t *testing.T) {
	startTimeout := 200 * time.Millisecond
	stopTimeout := 500 * time.Millisecond

	srv := grpcserver.NewServer(grpcserver.WithPort(0), grpcserver.WithHost("127.0.0.1"))
	srvCtx, srvCancel := context.WithCancel(context.Background())
	defer srvCancel()
	go srv.Start(srvCtx) //nolint:errcheck

	require.Eventually(t, func() bool {
		return srv.Addr() != "127.0.0.1:0"
	}, startTimeout, time.Millisecond)

	c := grpcclient.NewClient(srv.Addr(),
		grpcclient.WithDialOptions(grpclib.WithTransportCredentials(insecure.NewCredentials())),
	)

	clientCtx, clientCancel := context.WithCancel(context.Background())
	defer clientCancel()

	done := make(chan error, 1)
	go func() { done <- c.Start(clientCtx) }()

	require.Eventually(t, func() bool {
		return c.Conn() != nil
	}, startTimeout, time.Millisecond)

	stopCtx, stopCancel := context.WithTimeout(context.Background(), stopTimeout)
	defer stopCancel()
	require.NoError(t, c.Stop(stopCtx, nil))

	assert.Equal(t, connectivity.Shutdown, c.Conn().GetState())
}
