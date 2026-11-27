// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Package otlptest is an in-process OTLP/gRPC receiver for tests: it
// implements the trace, metrics and logs collector services, records every
// request with its metadata, compression and client certificates, and
// serves in plaintext, TLS or mTLS.
package otlptest

import (
	"context"
	"crypto/tls"
	"net"
	"slices"
	"sync"
	"testing"
	"time"

	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	colmetricpb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	_ "google.golang.org/grpc/encoding/gzip" // the server accepts gzip
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/stats"
)

// Received is one export request with what came with it.
type Received[M any] struct {
	Request M

	// Metadata is the incoming gRPC metadata (the headers).
	Metadata metadata.MD

	// Compression is the grpc-encoding of the request, "" when none.
	Compression string

	// ClientCertificates is the number of certificates the client
	// presented (mTLS).
	ClientCertificates int
}

// Header returns the first value of a header, or "".
func (r Received[M]) Header(name string) string {
	if v := r.Metadata.Get(name); len(v) > 0 {
		return v[0]
	}
	return ""
}

// Options configure a Server.
type Options struct {
	// TLS serves TLS (or mTLS with ClientAuth set) instead of plaintext.
	TLS *tls.Config
}

// Server is a running OTLP/gRPC receiver.
type Server struct {
	// Addr is the host:port it listens on, on 127.0.0.1.
	Addr string

	grpc *grpc.Server

	mu      sync.Mutex
	traces  []Received[*coltracepb.ExportTraceServiceRequest]
	metrics []Received[*colmetricpb.ExportMetricsServiceRequest]
	logs    []Received[*collogspb.ExportLogsServiceRequest]
	changed chan struct{}

	// fail is returned by every export while set, which records nothing.
	fail error
	// failed counts the refused exports.
	failed int
}

// Start starts a receiver on a free port of 127.0.0.1; it stops at the end
// of the test.
func Start(t testing.TB, opts Options) *Server {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("otlptest: listen: %v", err)
	}
	s := &Server{Addr: lis.Addr().String(), changed: make(chan struct{})}
	serverOpts := []grpc.ServerOption{grpc.StatsHandler(compressionRecorder{})}
	if opts.TLS != nil {
		serverOpts = append(serverOpts, grpc.Creds(credentials.NewTLS(opts.TLS)))
	}
	s.grpc = grpc.NewServer(serverOpts...)
	coltracepb.RegisterTraceServiceServer(s.grpc, traceService{s: s})
	colmetricpb.RegisterMetricsServiceServer(s.grpc, metricsService{s: s})
	collogspb.RegisterLogsServiceServer(s.grpc, logsService{s: s})
	go func() { _ = s.grpc.Serve(lis) }()
	t.Cleanup(s.Stop)
	return s
}

// Stop stops the receiver; a later export fails as with a closed port.
func (s *Server) Stop() {
	s.grpc.Stop()
}

// Fail makes every export fail with err (a gRPC status error, e.g.
// status.Error(codes.Unavailable, "down")) until it is called with nil. The
// refused requests are not recorded.
func (s *Server) Fail(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fail = err
}

// Failed returns the number of exports refused by Fail.
func (s *Server) Failed() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.failed
}

// Traces returns the trace export requests received so far.
func (s *Server) Traces() []Received[*coltracepb.ExportTraceServiceRequest] {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.traces)
}

// Metrics returns the metrics export requests received so far.
func (s *Server) Metrics() []Received[*colmetricpb.ExportMetricsServiceRequest] {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.metrics)
}

// Logs returns the logs export requests received so far.
func (s *Server) Logs() []Received[*collogspb.ExportLogsServiceRequest] {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.logs)
}

// Wait blocks until cond holds or the timeout passes, and reports whether
// it holds. cond is checked after every received request.
func (s *Server) Wait(timeout time.Duration, cond func(*Server) bool) bool {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		s.mu.Lock()
		changed := s.changed
		s.mu.Unlock()
		if cond(s) {
			return true
		}
		select {
		case <-changed:
		case <-deadline.C:
			return cond(s)
		}
	}
}

// record stores a request and wakes the waiters, or returns the error set
// by Fail.
func record[M any](s *Server, ctx context.Context, list *[]Received[M], req M) error {
	r := Received[M]{Request: req}
	r.Metadata, _ = metadata.FromIncomingContext(ctx)
	if info, ok := ctx.Value(rpcInfoKey{}).(*rpcInfo); ok {
		r.Compression = info.compression
	}
	if p, ok := peer.FromContext(ctx); ok {
		if ti, ok := p.AuthInfo.(credentials.TLSInfo); ok {
			r.ClientCertificates = len(ti.State.PeerCertificates)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail != nil {
		s.failed++
		close(s.changed)
		s.changed = make(chan struct{})
		return s.fail
	}
	*list = append(*list, r)
	close(s.changed)
	s.changed = make(chan struct{})
	return nil
}

type traceService struct {
	coltracepb.UnimplementedTraceServiceServer
	s *Server
}

func (ts traceService) Export(ctx context.Context, req *coltracepb.ExportTraceServiceRequest) (*coltracepb.ExportTraceServiceResponse, error) {
	if err := record(ts.s, ctx, &ts.s.traces, req); err != nil {
		return nil, err
	}
	return &coltracepb.ExportTraceServiceResponse{}, nil
}

type metricsService struct {
	colmetricpb.UnimplementedMetricsServiceServer
	s *Server
}

func (ms metricsService) Export(ctx context.Context, req *colmetricpb.ExportMetricsServiceRequest) (*colmetricpb.ExportMetricsServiceResponse, error) {
	if err := record(ms.s, ctx, &ms.s.metrics, req); err != nil {
		return nil, err
	}
	return &colmetricpb.ExportMetricsServiceResponse{}, nil
}

type logsService struct {
	collogspb.UnimplementedLogsServiceServer
	s *Server
}

func (ls logsService) Export(ctx context.Context, req *collogspb.ExportLogsServiceRequest) (*collogspb.ExportLogsServiceResponse, error) {
	if err := record(ls.s, ctx, &ls.s.logs, req); err != nil {
		return nil, err
	}
	return &collogspb.ExportLogsServiceResponse{}, nil
}

// compressionRecorder takes the grpc-encoding of each request from the
// stats of the RPC, which the handler sees through its context.
type compressionRecorder struct{}

type rpcInfoKey struct{}

type rpcInfo struct {
	compression string
}

func (compressionRecorder) TagRPC(ctx context.Context, _ *stats.RPCTagInfo) context.Context {
	return context.WithValue(ctx, rpcInfoKey{}, &rpcInfo{})
}

func (compressionRecorder) HandleRPC(ctx context.Context, st stats.RPCStats) {
	if h, ok := st.(*stats.InHeader); ok {
		if info, ok := ctx.Value(rpcInfoKey{}).(*rpcInfo); ok {
			info.compression = h.Compression
		}
	}
}

func (compressionRecorder) TagConn(ctx context.Context, _ *stats.ConnTagInfo) context.Context {
	return ctx
}

func (compressionRecorder) HandleConn(context.Context, stats.ConnStats) {}

// ClosedAddr returns a 127.0.0.1 address that nothing listens on.
func ClosedAddr(t testing.TB) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("otlptest: listen: %v", err)
	}
	addr := lis.Addr().String()
	_ = lis.Close()
	return addr
}
