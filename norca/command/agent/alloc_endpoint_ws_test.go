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

// TestAllocExec_HandshakeTimeout: a client that asks for the websocket
// handshake and never sends its message is not waited for longer than
// wsHandshakeTimeout: the exec handler tells it so in a close frame and
// closes the connection. The read had no time limit, so such a client, of
// which no ACL token was checked yet, held the connection and its slot of
// http_max_conns_per_client as long as it liked.
//
// The test waits for the real limit (5s); TestReadWsHandshakeWithin covers
// the rest with a short one.
func TestAllocExec_HandshakeTimeout(t *testing.T) {
	ci.Parallel(t)

	s := makeHTTPServer(t, func(c *Config) {
		c.LogLevel = "ERROR"
	})
	defer s.Shutdown()

	wsURL := "ws://" + s.Server.Addr + "/v1/client/allocation/" + uuid.Generate() +
		"/exec?task=web&ws_handshake=true&command=" + url.QueryEscape(`["true"]`)

	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	must.NoError(t, err)
	defer conn.Close()

	// nothing is sent; the server ends the wait
	must.NoError(t, conn.SetReadDeadline(time.Now().Add(wsHandshakeTimeout+15*time.Second)))
	_, _, err = conn.ReadMessage()
	must.True(t, websocket.IsCloseError(err, websocket.ClosePolicyViolation),
		must.Sprintf("unexpected error: %v", err))
	must.StrContains(t, err.Error(), "no handshake message within")

	// the server closes the connection after the close frame
	raw := conn.NetConn()
	must.NoError(t, raw.SetReadDeadline(time.Now().Add(10*time.Second)))
	_, err = raw.Read(make([]byte, 1))
	must.ErrorIs(t, err, io.EOF)
}

// TestReadWsHandshakeWithin: the time limit of the handshake message ends a
// silent client, and it is over once the handshake is read: a session that
// is idle for longer than the limit afterwards is not cut.
func TestReadWsHandshakeWithin(t *testing.T) {
	ci.Parallel(t)

	const limit = 2 * time.Second

	// the handler does what allocExec does around the handshake, then
	// answers the first message of the session with the token it got
	upgrader := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer ws.Close()

		var q structs.QueryOptions
		if err := readWsHandshakeWithin(ws, limit, r, &q); err != nil {
			ws.WriteMessage(websocket.CloseMessage,
				websocket.FormatCloseMessage(toWsCode(400), err.Error()))
			return
		}
		_, msg, err := ws.ReadMessage()
		if err != nil {
			ws.WriteMessage(websocket.CloseMessage,
				websocket.FormatCloseMessage(websocket.CloseInternalServerErr, "session read failed"))
			return
		}
		ws.WriteMessage(websocket.TextMessage, []byte(q.AuthToken+" "+string(msg)))
	}))
	t.Cleanup(srv.Close)

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")

	t.Run("no handshake message", func(t *testing.T) {
		ci.Parallel(t)

		conn, _, err := websocket.DefaultDialer.Dial(wsURL+"/?ws_handshake=true", nil)
		must.NoError(t, err)
		defer conn.Close()

		start := time.Now()
		must.NoError(t, conn.SetReadDeadline(start.Add(limit+15*time.Second)))
		_, _, err = conn.ReadMessage()
		must.True(t, websocket.IsCloseError(err, websocket.ClosePolicyViolation),
			must.Sprintf("unexpected error: %v", err))
		must.StrContains(t, err.Error(), "no handshake message within 2s")
		// not before the limit either (the dial returned after the
		// server set the deadline, or a moment before)
		must.Greater(t, limit/2, time.Since(start))
	})

	t.Run("idle after the handshake", func(t *testing.T) {
		ci.Parallel(t)

		conn, _, err := websocket.DefaultDialer.Dial(wsURL+"/?ws_handshake=true", nil)
		must.NoError(t, err)
		defer conn.Close()

		must.NoError(t, conn.WriteJSON(map[string]any{"version": 1, "auth_token": "secret"}))
		time.Sleep(2 * limit)
		must.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte("late")))

		must.NoError(t, conn.SetReadDeadline(time.Now().Add(15*time.Second)))
		_, msg, err := conn.ReadMessage()
		must.NoError(t, err)
		must.Eq(t, "secret late", string(msg))
	})

	t.Run("no handshake asked", func(t *testing.T) {
		ci.Parallel(t)

		conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
		must.NoError(t, err)
		defer conn.Close()

		time.Sleep(2 * limit)
		must.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte("late")))

		must.NoError(t, conn.SetReadDeadline(time.Now().Add(15*time.Second)))
		_, msg, err := conn.ReadMessage()
		must.NoError(t, err)
		must.Eq(t, " late", string(msg))
	})
}
