// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package drivers

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/golang/protobuf/ptypes"
	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/helper/testlog"
	"github.com/hashicorp/nomad/plugins/drivers/proto"
	"github.com/shoenig/test/must"
	"github.com/shoenig/test/wait"
	"google.golang.org/grpc"
)

// fakeDriverClient overrides the proto.DriverClient methods a test needs;
// calling any other method panics on the nil embedded interface.
type fakeDriverClient struct {
	proto.DriverClient

	waitTask          func(ctx context.Context) (*proto.WaitTaskResponse, error)
	createNetwork     func(req *proto.CreateNetworkRequest)
	execTaskStreaming proto.Driver_ExecTaskStreamingClient
}

func (f *fakeDriverClient) WaitTask(ctx context.Context, _ *proto.WaitTaskRequest, _ ...grpc.CallOption) (*proto.WaitTaskResponse, error) {
	return f.waitTask(ctx)
}

func (f *fakeDriverClient) CreateNetwork(_ context.Context, req *proto.CreateNetworkRequest, _ ...grpc.CallOption) (*proto.CreateNetworkResponse, error) {
	f.createNetwork(req)
	return &proto.CreateNetworkResponse{}, nil
}

func (f *fakeDriverClient) ExecTaskStreaming(context.Context, ...grpc.CallOption) (proto.Driver_ExecTaskStreamingClient, error) {
	return f.execTaskStreaming, nil
}

// errClientStream is a client stream whose Recv always fails.
type errClientStream struct {
	grpc.ClientStream
}

func (errClientStream) Recv() (*proto.FingerprintResponse, error) {
	return nil, errors.New("stream broken")
}

type errEventsStream struct {
	grpc.ClientStream
}

func (errEventsStream) Recv() (*proto.DriverTaskEvent, error) {
	return nil, errors.New("stream broken")
}

func testDriverClient(t *testing.T, c proto.DriverClient) *driverPluginClient {
	return &driverPluginClient{
		client:  c,
		logger:  testlog.HCLogger(t),
		doneCtx: context.Background(),
	}
}

// TestDriverClient_StreamErrorAfterCancel asserts that the stream handlers
// exit when the stream fails after the consumer cancelled and stopped
// reading, instead of blocking on the error send.
func TestDriverClient_StreamErrorAfterCancel(t *testing.T) {
	ci.Parallel(t)
	d := testDriverClient(t, nil)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	fpCh := make(chan *Fingerprint, 1)
	fpCh <- &Fingerprint{} // a value the consumer never read
	evCh := make(chan *TaskEvent, 1)
	evCh <- &TaskEvent{}

	done := make(chan struct{}, 2)
	go func() { d.handleFingerprint(ctx, fpCh, errClientStream{}); done <- struct{}{} }()
	go func() { d.handleTaskEvents(ctx, evCh, errEventsStream{}); done <- struct{}{} }()

	for i := 0; i < 2; i++ {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("stream handler blocked on the error send")
		}
	}
}

// TestDriverClient_WaitTaskCancelled asserts that the WaitTask goroutine can
// deliver its result and exit when the caller stopped reading.
func TestDriverClient_WaitTaskCancelled(t *testing.T) {
	ci.Parallel(t)
	d := testDriverClient(t, &fakeDriverClient{
		waitTask: func(ctx context.Context) (*proto.WaitTaskResponse, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	ch, err := d.WaitTask(ctx, "task")
	must.NoError(t, err)
	cancel()

	// the result lands in the buffer without anybody reading
	must.Wait(t, wait.InitialSuccess(
		wait.BoolFunc(func() bool { return len(ch) == 1 }),
		wait.Timeout(5*time.Second),
		wait.Gap(10*time.Millisecond),
	))
	res := <-ch
	must.Error(t, res.Err)
	_, ok := <-ch
	must.False(t, ok)
}

// TestDriverClient_WaitTaskNoResult asserts that a WaitTask response without
// a result message, as the server sends when the driver closes its wait
// channel, does not panic.
func TestDriverClient_WaitTaskNoResult(t *testing.T) {
	ci.Parallel(t)
	d := testDriverClient(t, &fakeDriverClient{
		waitTask: func(context.Context) (*proto.WaitTaskResponse, error) {
			return &proto.WaitTaskResponse{Err: "channel closed"}, nil
		},
	})

	ch, err := d.WaitTask(context.Background(), "task")
	must.NoError(t, err)
	res := <-ch
	must.NotNil(t, res)
	must.EqError(t, res.Err, "channel closed")
	must.Eq(t, 0, res.ExitCode)
}

// TestDriverClient_CreateNetworkHostname asserts that the hostname of the
// request reaches the plugin.
func TestDriverClient_CreateNetworkHostname(t *testing.T) {
	ci.Parallel(t)
	var got *proto.CreateNetworkRequest
	d := testDriverClient(t, &fakeDriverClient{
		createNetwork: func(req *proto.CreateNetworkRequest) { got = req },
	})

	_, _, err := d.CreateNetwork("alloc", &NetworkCreateRequest{Hostname: "web-1"})
	must.NoError(t, err)
	must.Eq(t, "alloc", got.AllocId)
	must.Eq(t, "web-1", got.Hostname)

	_, _, err = d.CreateNetwork("alloc", nil)
	must.NoError(t, err)
	must.Eq(t, "", got.Hostname)
}

// fakeExecClientStream ends its output once CloseSend has been called.
type fakeExecClientStream struct {
	grpc.ClientStream
	closed chan struct{}
	once   sync.Once
}

func (f *fakeExecClientStream) Send(*proto.ExecTaskStreamingRequest) error { return nil }

func (f *fakeExecClientStream) CloseSend() error {
	f.once.Do(func() { close(f.closed) })
	return nil
}

func (f *fakeExecClientStream) Recv() (*proto.ExecTaskStreamingResponse, error) {
	select {
	case <-f.closed:
		return nil, io.EOF
	case <-time.After(5 * time.Second):
		return nil, errors.New("input end was not passed on")
	}
}

// eofExecStream is a caller stream with no input at all.
type eofExecStream struct{}

func (eofExecStream) Send(*ExecTaskStreamingResponseMsg) error    { return nil }
func (eofExecStream) Recv() (*ExecTaskStreamingRequestMsg, error) { return nil, io.EOF }

// TestDriverClient_ExecStreamingInputEOF asserts that the end of the
// caller's input is passed on to the plugin as a half-close.
func TestDriverClient_ExecStreamingInputEOF(t *testing.T) {
	ci.Parallel(t)
	stream := &fakeExecClientStream{closed: make(chan struct{})}
	d := testDriverClient(t, &fakeDriverClient{execTaskStreaming: stream})

	err := d.ExecTaskStreamingRaw(context.Background(), "task", []string{"cat"}, false, eofExecStream{})
	must.NoError(t, err)
}

// scriptedExecStream returns the given messages, then the given error.
type scriptedExecStream struct {
	msgs []*ExecTaskStreamingRequestMsg
	err  error
}

func (s *scriptedExecStream) Send(*ExecTaskStreamingResponseMsg) error { return nil }

func (s *scriptedExecStream) Recv() (*ExecTaskStreamingRequestMsg, error) {
	if len(s.msgs) == 0 {
		return nil, s.err
	}
	m := s.msgs[0]
	s.msgs = s.msgs[1:]
	return m, nil
}

// readStdin reads the command's stdin to its end and returns the error that
// ended it, failing the test if it stays open.
func readStdin(t *testing.T, opts *ExecOptions) error {
	errCh := make(chan error, 1)
	go func() {
		_, err := io.ReadAll(opts.Stdin)
		errCh <- err
	}()
	select {
	case err := <-errCh:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("stdin was not closed")
		return nil
	}
}

// TestStreamToExecOptions_InputEnd asserts that stdin is closed when the
// input stream ends, fails, or carries many unexpected messages.
func TestStreamToExecOptions_InputEnd(t *testing.T) {
	ci.Parallel(t)

	t.Run("eof", func(t *testing.T) {
		opts, _ := StreamToExecOptions(context.Background(), nil, false,
			&scriptedExecStream{err: io.EOF})
		must.NoError(t, readStdin(t, opts))
	})

	t.Run("error", func(t *testing.T) {
		opts, _ := StreamToExecOptions(context.Background(), nil, false,
			&scriptedExecStream{err: errors.New("broken")})
		must.EqError(t, readStdin(t, opts), "broken")
	})

	t.Run("unexpected messages", func(t *testing.T) {
		setup := &ExecTaskStreamingRequestMsg{Setup: &proto.ExecTaskStreamingRequest_Setup{}}
		msgs := []*ExecTaskStreamingRequestMsg{setup, setup, setup, setup, setup}
		opts, doneCh := StreamToExecOptions(context.Background(), nil, false,
			&scriptedExecStream{msgs: msgs, err: io.EOF})
		must.NoError(t, readStdin(t, opts))

		opts.Stdout.Close()
		opts.Stderr.Close()
		select {
		case err := <-doneCh:
			must.ErrorContains(t, err, "unexpected message type")
		case <-time.After(5 * time.Second):
			t.Fatal("exec streams did not finish")
		}
	})
}

// fakeEventsPlugin returns a task events channel that is never closed.
type fakeEventsPlugin struct {
	DriverPlugin
}

func (fakeEventsPlugin) TaskEvents(context.Context) (<-chan *TaskEvent, error) {
	return make(chan *TaskEvent), nil
}

type fakeEventsServer struct {
	grpc.ServerStream
	ctx context.Context
}

func (f fakeEventsServer) Context() context.Context          { return f.ctx }
func (f fakeEventsServer) Send(*proto.DriverTaskEvent) error { return nil }

// TestDriverServer_TaskEventsClientGone asserts that the TaskEvents handler
// returns when the client goes away even if the driver never closes its
// channel.
func TestDriverServer_TaskEventsClientGone(t *testing.T) {
	ci.Parallel(t)
	srv := &driverPluginServer{impl: fakeEventsPlugin{}}

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.TaskEvents(&proto.TaskEventsRequest{}, fakeEventsServer{ctx: ctx})
	}()
	cancel()

	select {
	case err := <-errCh:
		must.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("TaskEvents did not return after the client went away")
	}
}

// TestConvertersMissingMessages asserts that responses from an external
// plugin with absent (nil) message fields do not panic the client.
func TestConvertersMissingMessages(t *testing.T) {
	ci.Parallel(t)

	must.Eq(t, &ExitResult{}, exitResultFromProto(nil))

	_, err := taskStatusFromProto(nil)
	must.Error(t, err)

	_, err = TaskStatsFromProto(nil)
	must.Error(t, err)

	stats, err := TaskStatsFromProto(&proto.TaskStats{
		Timestamp:          ptypes.TimestampNow(),
		ResourceUsageByPid: map[string]*proto.TaskResourceUsage{"1": nil},
	})
	must.NoError(t, err)
	must.NotNil(t, stats.ResourceUsage.CpuStats)
	must.NotNil(t, stats.ResourceUsage.MemoryStats)
	must.NotNil(t, stats.Pids["1"].CpuStats)
}
