// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package drivers

import (
	"context"
	"io"
	"sync/atomic"
	"testing"

	"github.com/bacanoo-io/norca/norca/plugins/drivers/proto"
	"github.com/shoenig/test/must"
	"google.golang.org/grpc"
)

// execClient answers ExecTaskStreaming with a stream that records CloseSend.
// Without a TTY the stream ends once it was half-closed, with one once the
// caller's input ended.
type execClient struct {
	proto.DriverClient
	stream *execStreamClient
}

func (c *execClient) ExecTaskStreaming(context.Context, ...grpc.CallOption) (proto.Driver_ExecTaskStreamingClient, error) {
	return c.stream, nil
}

type execStreamClient struct {
	grpc.ClientStream
	tty        bool
	closedSend atomic.Bool
	halfClosed chan struct{}
	inputDone  chan struct{}
}

func (s *execStreamClient) Send(*proto.ExecTaskStreamingRequest) error { return nil }
func (s *execStreamClient) CloseSend() error {
	if !s.closedSend.Swap(true) {
		close(s.halfClosed)
	}
	return nil
}
func (s *execStreamClient) Recv() (*proto.ExecTaskStreamingResponse, error) {
	if s.tty {
		<-s.inputDone
	} else {
		<-s.halfClosed
	}
	return nil, io.EOF
}

// callerInput is the caller's side: its input ends at once.
type callerInput struct{ done chan struct{} }

func (c *callerInput) Send(*ExecTaskStreamingResponseMsg) error { return nil }
func (c *callerInput) Recv() (*ExecTaskStreamingRequestMsg, error) {
	close(c.done)
	return nil, io.EOF
}

// The end of the caller's input half-closes the stream of a session without a
// TTY, which ends the command's stdin; a TTY session keeps it open (review
// finding PLG-1).
func TestExecTaskStreamingRaw_HalfCloseOnlyWithoutTTY(t *testing.T) {
	for _, tty := range []bool{false, true} {
		stream := &execStreamClient{tty: tty, halfClosed: make(chan struct{}), inputDone: make(chan struct{})}
		d := &driverPluginClient{client: &execClient{stream: stream}, doneCtx: context.Background()}
		must.NoError(t, d.ExecTaskStreamingRaw(context.Background(), "task", []string{"sh"}, tty,
			&callerInput{done: stream.inputDone}))
		must.Eq(t, !tty, stream.closedSend.Load(), must.Sprintf("tty %v", tty))
	}
}
