// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

//go:build unix

package command

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"syscall"
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/api"
)

// endedOperation is the API of an operation that has ended, whose result
// is never answered: hung tells when it is read, cancelAsked when the
// operation is asked to be cancelled.
func endedOperation(t *testing.T) (client *api.Client, hung, cancelAsked <-chan struct{}) {
	t.Helper()

	release := make(chan struct{})
	reads := make(chan struct{}, 1)
	cancels := make(chan struct{}, 1)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch {
		case r.Method == http.MethodPut && r.URL.Path == "/v1/rigger/operation/op1/cancel":
			_, _ = w.Write([]byte(`{"name":"op1","status":{"phase":"Succeeded"}}`))

			cancels <- struct{}{}
		case r.URL.Path == "/v1/rigger/operation/op1/log":
			_, _ = w.Write([]byte(`{"Lines":[],"Phase":"Succeeded","Finished":true}`))
		case r.URL.Path == "/v1/object/rigger.norca.io/Operation/op1":
			reads <- struct{}{}

			select {
			case <-r.Context().Done():
			case <-release:
			}
		default:
			http.NotFound(w, r)
		}
	}))

	// Close waits for the requests: one whose client never gave up is let
	// go first.
	t.Cleanup(func() {
		close(release)
		srv.Close()
	})

	cfg := api.DefaultConfig()
	cfg.Address = srv.URL

	client, err := api.NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}

	return client, reads, cancels
}

// The second interrupt leaves the command also while it reads the result
// of an operation that has ended: that request ends with the others.
//
// The test interrupts its own process, which is the only way to the second
// interrupt: it is not parallel, so that no other test listens for one, and
// it sends the signal only when the command listens for it (it does before
// it asks the servers to cancel the operation).
//
//nolint:paralleltest // it sends an interrupt to the process of the tests
func TestFollowOperation_DetachAtResult(t *testing.T) {
	client, hung, cancelAsked := endedOperation(t)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	done := make(chan error, 1)

	go func() {
		done <- followOperation(ctx, slog.New(slog.DiscardHandler), io.Discard, io.Discard, client, "shop", "op1")
	}()

	wait := func(what string, ch <-chan struct{}) {
		t.Helper()

		select {
		case <-ch:
		case err := <-done:
			t.Fatalf("followOperation() = %v before %s", err, what)
		case <-time.After(30 * time.Second):
			t.Fatalf("waited in vain for %s", what)
		}
	}

	wait("the read of the result", hung)
	cancel() // the first interrupt
	wait("the request to cancel the operation", cancelAsked)

	if err := syscall.Kill(syscall.Getpid(), syscall.SIGINT); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("followOperation() = %v, want the error of the cancelled context", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the command still reads the result after the second interrupt")
	}
}
