// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/helper/pointer"
	"github.com/bacanoo-io/norca/norca/helper/uuid"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/gorilla/websocket"
	"github.com/hashicorp/go-msgpack/codec"
	"github.com/shoenig/test/must"
)

// TestWsSyncWriter_ConcurrentWrites mirrors the alloc exec stream, where the
// output streaming goroutine and the error path write to the same websocket.
// Without serialization gorilla/websocket panics with "concurrent write to
// websocket connection" and the race detector reports a data race.
func TestWsSyncWriter_ConcurrentWrites(t *testing.T) {
	ci.Parallel(t)

	const writers, messages = 8, 200

	upgrader := websocket.Upgrader{}
	srvDone := make(chan error, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			srvDone <- err
			return
		}
		defer ws.Close()

		wsw := &wsSyncWriter{ws: ws}
		var wg sync.WaitGroup
		errCh := make(chan error, writers)
		for i := 0; i < writers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for j := 0; j < messages; j++ {
					if err := wsw.WriteMessage(websocket.TextMessage, []byte("payload")); err != nil {
						errCh <- err
						return
					}
				}
			}()
		}
		wg.Wait()
		close(errCh)
		for err := range errCh {
			srvDone <- err
			return
		}
		srvDone <- wsw.WriteMessage(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
	}))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	must.NoError(t, err)
	defer conn.Close()

	received := 0
	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			must.True(t, websocket.IsCloseError(err, websocket.CloseNormalClosure),
				must.Sprintf("unexpected read error: %v", err))
			break
		}
		must.Eq(t, "payload", string(msg))
		received++
	}

	must.NoError(t, <-srvDone)
	must.Eq(t, writers*messages, received)
}

// TestForwardExecInput_ClosedPipe: when the RPC rejects an exec (e.g. an
// unknown allocation) and closes the stream, forwarding the next stdin frame
// fails on the closed pipe. That must not be reported, or it can arrive
// before the error of the RPC and replace it ("msgpack encode error: io:
// read/write on closed pipe" instead of "Unknown allocation").
func TestForwardExecInput_ClosedPipe(t *testing.T) {
	ci.Parallel(t)

	upgrader := websocket.Upgrader{}
	errCh := make(chan HTTPCodedError, 1)
	done := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer ws.Close()
		pr, pw := io.Pipe()
		pr.Close() // the stream has ended
		forwardExecInput(codec.NewEncoder(pw, structs.MsgpackHandle), ws, errCh)
		close(done)
	}))
	defer srv.Close()

	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	must.NoError(t, err)
	defer conn.Close()
	must.NoError(t, conn.WriteJSON(map[string]any{"stdin": map[string]any{"data": "aGk="}}))

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("forwardExecInput did not return")
	}
	select {
	case err := <-errCh:
		t.Fatalf("closed pipe reported as an error: %v", err)
	default:
	}
	must.True(t, isClosedPipe(fmt.Errorf("msgpack encode error: %s", io.ErrClosedPipe)))
	must.False(t, isClosedPipe(nil))
	must.False(t, isClosedPipe(io.EOF))
}

// TestAllocExec_HandshakeErrorClosesConn: the exec handler closes the
// connection it hijacked when the handshake message of the client is bad.
// It only sent the close frame and left the connection open: the client
// could hold it, and since a hijacked connection counts in
// http_max_conns_per_client until it is closed, every such request would
// take a slot of its client for good.
func TestAllocExec_HandshakeErrorClosesConn(t *testing.T) {
	ci.Parallel(t)

	const limit = 3

	s := makeHTTPServer(t, func(c *Config) {
		c.Limits.HTTPMaxConnsPerClient = pointer.Of(limit)
		c.LogLevel = "ERROR"
	})
	defer s.Shutdown()

	wsURL := "ws://" + s.Server.Addr + "/v1/client/allocation/" + uuid.Generate() +
		"/exec?task=web&ws_handshake=true&command=" + url.QueryEscape(`["true"]`)

	// more requests than the client has connections
	for i := 0; i < 2*limit; i++ {
		conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
		must.NoError(t, err, must.Sprintf("request %d", i))
		defer conn.Close()
		must.NoError(t, conn.WriteJSON(map[string]any{"version": 99}))

		_, _, err = conn.ReadMessage()
		must.True(t, websocket.IsCloseError(err, websocket.ClosePolicyViolation),
			must.Sprintf("request %d: unexpected error: %v", i, err))

		// the server closes the connection after the close frame
		raw := conn.NetConn()
		must.NoError(t, raw.SetReadDeadline(time.Now().Add(10*time.Second)))
		_, err = raw.Read(make([]byte, 1))
		must.ErrorIs(t, err, io.EOF, must.Sprintf("request %d", i))
	}
}
