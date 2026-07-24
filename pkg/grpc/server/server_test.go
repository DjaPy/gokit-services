package server_test

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	grpclib "google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	grpcsrv "github.com/DjaPy/gokit-services/pkg/grpc/server"
)

func TestGrpcServer_StartStop(t *testing.T) {
	startTimeout := 200 * time.Millisecond
	stopTimeout := 500 * time.Millisecond

	srv := grpcsrv.NewServer(grpcsrv.WithPort(0), grpcsrv.WithHost("127.0.0.1"))

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- srv.Start(ctx) }()

	require.Eventually(t, func() bool {
		addr := srv.Addr()
		if addr == "127.0.0.1:0" {
			return false
		}
		conn, err := grpclib.NewClient(addr, grpclib.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			return false
		}
		conn.Close() //nolint:errcheck
		return true
	}, startTimeout, time.Millisecond)

	cancel()

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(stopTimeout):
		t.Fatal("Start did not return after context cancel")
	}
}

func TestGrpcServer_Addr_BeforeStart(t *testing.T) {
	host := "127.0.0.1"
	port := 19090
	expectedAddr := "127.0.0.1:19090"

	srv := grpcsrv.NewServer(grpcsrv.WithHost(host), grpcsrv.WithPort(port))
	assert.Equal(t, expectedAddr, srv.Addr())
}

func TestGrpcServer_Addr_AfterStart(t *testing.T) {
	startTimeout := 200 * time.Millisecond

	srv := grpcsrv.NewServer(grpcsrv.WithPort(0), grpcsrv.WithHost("127.0.0.1"))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- srv.Start(ctx) }()

	var actualAddr string
	require.Eventually(t, func() bool {
		addr := srv.Addr()
		if addr == "127.0.0.1:0" {
			return false
		}
		_, port, err := net.SplitHostPort(addr)
		if err != nil || port == "0" {
			return false
		}
		actualAddr = addr
		return true
	}, startTimeout, time.Millisecond)

	assert.NotEqual(t, "127.0.0.1:0", actualAddr)
	cancel()
	<-done
}

func TestGrpcServer_Stop_GracefulShutdown(t *testing.T) {
	startTimeout := 200 * time.Millisecond
	stopTimeout := 500 * time.Millisecond

	srv := grpcsrv.NewServer(grpcsrv.WithPort(0), grpcsrv.WithHost("127.0.0.1"))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- srv.Start(ctx) }()

	require.Eventually(t, func() bool {
		return srv.Addr() != "127.0.0.1:0"
	}, startTimeout, time.Millisecond)

	stopCtx, stopCancel := context.WithTimeout(context.Background(), stopTimeout)
	defer stopCancel()
	require.NoError(t, srv.Stop(stopCtx))

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(stopTimeout):
		t.Fatal("Start did not return after Stop")
	}
}

func TestGrpcServer_Stop_ForcefulOnCtxExpiry(t *testing.T) {
	startTimeout := 200 * time.Millisecond

	srv := grpcsrv.NewServer(grpcsrv.WithPort(0), grpcsrv.WithHost("127.0.0.1"))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- srv.Start(ctx) }()

	require.Eventually(t, func() bool {
		return srv.Addr() != "127.0.0.1:0"
	}, startTimeout, time.Millisecond)

	// already-expired ctx forces fallback to srv.Stop()
	expiredCtx, expiredCancel := context.WithCancel(context.Background())
	expiredCancel()
	err := srv.Stop(expiredCtx)
	assert.ErrorIs(t, err, context.Canceled)

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(startTimeout):
		t.Fatal("Start did not return after forced Stop")
	}
}

// TestGrpcServer_Stop_ExpiredCtxReportsCtxError pins the ctx.Err() fast-path in
// Stop: when the deadline is already blown, Stop must skip the graceful attempt
// and report the ctx's own error — context.Canceled or context.DeadlineExceeded
// — and never nil.

func TestGrpcServer_Stop_ExpiredCtxReportsCtxError(t *testing.T) {
	tests := map[string]struct {
		ctx     func() (context.Context, context.CancelFunc)
		wantErr error
	}{
		"already canceled": {
			ctx: func() (context.Context, context.CancelFunc) {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return ctx, cancel
			},
			wantErr: context.Canceled,
		},
		"deadline exceeded": {
			ctx: func() (context.Context, context.CancelFunc) {
				return context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
			},
			wantErr: context.DeadlineExceeded,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			srv := grpcsrv.NewServer(grpcsrv.WithPort(0), grpcsrv.WithHost("127.0.0.1"))
			ctx, cancel := tc.ctx()
			defer cancel()

			err := srv.Stop(ctx)
			require.Error(t, err)
			assert.ErrorIs(t, err, tc.wantErr)
		})
	}
}

func TestGrpcServer_ContextCancelStops(t *testing.T) {
	startTimeout := 200 * time.Millisecond
	stopTimeout := 500 * time.Millisecond

	srv := grpcsrv.NewServer(grpcsrv.WithPort(0), grpcsrv.WithHost("127.0.0.1"))

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- srv.Start(ctx) }()

	require.Eventually(t, func() bool {
		return srv.Addr() != "127.0.0.1:0"
	}, startTimeout, time.Millisecond)

	cancel()

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(stopTimeout):
		t.Fatal("Start did not return after context cancel")
	}
}
