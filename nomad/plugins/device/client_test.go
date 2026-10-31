// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package device

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/plugins/device/proto"
	"google.golang.org/grpc"
)

type errFingerprintStream struct {
	grpc.ClientStream
}

func (errFingerprintStream) Recv() (*proto.FingerprintResponse, error) {
	return nil, errors.New("stream broken")
}

type errStatsStream struct {
	grpc.ClientStream
}

func (errStatsStream) Recv() (*proto.StatsResponse, error) {
	return nil, errors.New("stream broken")
}

// TestDeviceClient_StreamErrorAfterCancel asserts that the stream handlers
// exit when the stream fails after the consumer cancelled and stopped
// reading, instead of blocking on the error send.
func TestDeviceClient_StreamErrorAfterCancel(t *testing.T) {
	ci.Parallel(t)
	d := &devicePluginClient{doneCtx: context.Background()}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	fpCh := make(chan *FingerprintResponse, 1)
	fpCh <- &FingerprintResponse{} // a value the consumer never read
	statsCh := make(chan *StatsResponse, 1)
	statsCh <- &StatsResponse{}

	done := make(chan struct{}, 2)
	go func() { d.handleFingerprint(ctx, errFingerprintStream{}, fpCh); done <- struct{}{} }()
	go func() { d.handleStats(ctx, errStatsStream{}, statsCh); done <- struct{}{} }()

	for i := 0; i < 2; i++ {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("stream handler blocked on the error send")
		}
	}
}
