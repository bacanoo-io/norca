// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package csi

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/shoenig/test/must"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"

	"github.com/bacanoo-io/norca/norca/ci"
)

// A plugin that listens on its socket: the connection is ready when
// newGrpcConn returns, and an RPC goes through it.
func TestNewGrpcConn_Ready(t *testing.T) {
	ci.Parallel(t)
	if runtime.GOOS == "windows" {
		t.Skip("CSI plugins listen on unix sockets")
	}

	// short path: a unix socket path has a length limit
	dir, err := os.MkdirTemp("", "csi")
	must.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "csi.sock")

	l, err := net.Listen("unix", sock)
	must.NoError(t, err)
	srv := grpc.NewServer()
	healthpb.RegisterHealthServer(srv, health.NewServer())
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(srv.Stop)

	conn, err := newGrpcConn(sock, hclog.NewNullLogger())
	must.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := healthpb.NewHealthClient(conn).Check(ctx, &healthpb.HealthCheckRequest{})
	must.NoError(t, err)
	must.Eq(t, healthpb.HealthCheckResponse_SERVING, resp.Status)
}

// Nobody listens: newGrpcConn gives up after its second of waiting instead
// of returning a connection that fails later.
func TestNewGrpcConn_NoListener(t *testing.T) {
	ci.Parallel(t)
	if runtime.GOOS == "windows" {
		t.Skip("CSI plugins listen on unix sockets")
	}

	start := time.Now()
	conn, err := newGrpcConn(filepath.Join(t.TempDir(), "missing.sock"), hclog.NewNullLogger())
	must.Error(t, err)
	must.Nil(t, conn)
	must.ErrorContains(t, err, "context deadline exceeded")
	took := time.Since(start)
	must.Greater(t, 900*time.Millisecond, took)
	must.Less(t, 5*time.Second, took)
}
