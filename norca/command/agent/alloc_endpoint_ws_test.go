// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/helper/pointer"
	"github.com/bacanoo-io/norca/norca/helper/uuid"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/bacanoo-io/norca/norca/testutil"
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

// wsTestCloseReasonMax is the longest reason a websocket close frame holds:
// a control frame has at most 125 bytes of payload (RFC 6455, 5.5), and the
// close code takes two of them.
const wsTestCloseReasonMax = 123

// mustWsCloseFrame reads until the connection ends and wants it to end with
// a close frame of the server. Without one the read fails with the close
// error 1006, which no frame carries, and that fails the test.
func mustWsCloseFrame(t *testing.T, conn *websocket.Conn) *websocket.CloseError {
	t.Helper()

	must.NoError(t, conn.SetReadDeadline(time.Now().Add(20*time.Second)))
	for {
		_, _, err := conn.ReadMessage()
		if err == nil {
			continue
		}
		var closeErr *websocket.CloseError
		must.True(t, errors.As(err, &closeErr), must.Sprintf("no close frame: %v", err))
		must.NotEq(t, websocket.CloseAbnormalClosure, closeErr.Code, must.Sprintf("no close frame: %v", err))
		return closeErr
	}
}

// mustWsCutReason: reason is full, which does not fit into a close frame, cut
// to fit: a beginning of full, as much of it as fits, and "..." after it.
func mustWsCutReason(t *testing.T, full, reason string) {
	t.Helper()

	const ellipsis = "..."
	must.Greater(t, wsTestCloseReasonMax, len(full), must.Sprint("the full text fits, nothing to cut"))
	must.LessEq(t, wsTestCloseReasonMax, len(reason), must.Sprintf("reason: %q", reason))
	must.True(t, utf8.ValidString(reason), must.Sprintf("reason: %q", reason))
	must.StrHasSuffix(t, ellipsis, reason)
	kept := strings.TrimSuffix(reason, ellipsis)
	must.StrHasPrefix(t, kept, full)
	// no more is cut than the frame asks for: the rest of the room is less
	// than a character (at most 4 bytes)
	must.Greater(t, wsTestCloseReasonMax-utf8.UTFMax, len(reason), must.Sprintf("reason: %q", reason))
}

// TestWsCloseReason: a text that fits into a close frame is its reason as it
// is; a longer one is cut to the 123 bytes a close frame has for it, between
// two characters, and ends in "..."; the reason is valid UTF-8 whatever the
// text is.
func TestWsCloseReason(t *testing.T) {
	ci.Parallel(t)

	must.Eq(t, wsTestCloseReasonMax, wsCloseReasonMaxBytes)

	cases := []struct {
		name string
		text string
		want string
	}{
		{name: "empty", text: "", want: ""},
		{name: "short", text: "task not found", want: "task not found"},
		{name: "ASCII of 123 bytes", text: strings.Repeat("a", 123), want: strings.Repeat("a", 123)},
		{name: "ASCII of 124 bytes", text: strings.Repeat("a", 124), want: strings.Repeat("a", 120) + "..."},
		{name: "ASCII of 5000 bytes", text: strings.Repeat("a", 5000), want: strings.Repeat("a", 120) + "..."},
		{
			// 41 characters of three bytes
			name: "three-byte characters of 123 bytes",
			text: strings.Repeat("€", 41),
			want: strings.Repeat("€", 41),
		},
		{
			// the cut at byte 120 is between two characters
			name: "three-byte characters, cut at a boundary",
			text: strings.Repeat("€", 42),
			want: strings.Repeat("€", 40) + "...",
		},
		{
			// byte 120 is the last byte of a character: it goes whole
			name: "three-byte characters, cut before the last byte",
			text: "a" + strings.Repeat("€", 41),
			want: "a" + strings.Repeat("€", 39) + "...",
		},
		{
			// byte 120 is the second byte of a character
			name: "three-byte characters, cut inside a character",
			text: "aa" + strings.Repeat("€", 41),
			want: "aa" + strings.Repeat("€", 39) + "...",
		},
		{
			name: "two-byte characters, cut inside a character",
			text: "a" + strings.Repeat("é", 100),
			want: "a" + strings.Repeat("é", 59) + "...",
		},
		{
			name: "four-byte characters, cut inside a character",
			text: "aaa" + strings.Repeat("😀", 31),
			want: "aaa" + strings.Repeat("😀", 29) + "...",
		},
		{
			name: "long text with a character at the end of the room",
			text: strings.Repeat("a", 117) + "€" + strings.Repeat("a", 10),
			want: strings.Repeat("a", 117) + "€" + "...",
		},
		{name: "invalid byte", text: "bad \xff byte", want: "bad � byte"},
		{name: "run of invalid bytes", text: "bad \xff\xfe\xfd bytes", want: "bad � bytes"},
		{name: "half of a character at the end", text: "half \xe2\x82", want: "half �"},
		{
			// 122 bytes, 124 with the invalid byte replaced
			name: "invalid byte makes the text too long",
			text: strings.Repeat("a", 121) + "\xff",
			want: strings.Repeat("a", 120) + "...",
		},
		{
			name: "long text of invalid bytes",
			text: strings.Repeat("\xffa", 100),
			want: strings.Repeat("�a", 30) + "...",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := wsCloseReason(tc.text)
			must.Eq(t, tc.want, got)
			must.LessEq(t, wsTestCloseReasonMax, len(got))
			must.True(t, utf8.ValidString(got))
			// the websocket takes it as the payload of a control frame
			must.LessEq(t, 125, len(websocket.FormatCloseMessage(websocket.ClosePolicyViolation, got)))
		})
	}
}

// TestAllocExec_HandshakeErrorLongCloseReason: a handshake error whose text
// does not fit into a close frame still reaches the client as the close
// frame "policy violation", with the beginning of the text as its reason.
// The text went out whole, the websocket refused to write a control frame of
// that size, and the client saw the connection end without a close frame
// (1006). The client sets the length here: the text quotes its ws_handshake
// value.
func TestAllocExec_HandshakeErrorLongCloseReason(t *testing.T) {
	ci.Parallel(t)

	s := makeHTTPServer(t, func(c *Config) {
		c.LogLevel = "ERROR"
	})
	defer s.Shutdown()

	cases := []struct {
		name  string
		value string // of ws_handshake
	}{
		{name: "ASCII value", value: strings.Repeat("a", 200)},
		{name: "value of two-byte characters", value: strings.Repeat("é", 200)},
		{name: "value of three-byte characters", value: strings.Repeat("€", 200)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, parseErr := strconv.ParseBool(tc.value)
			must.Error(t, parseErr)
			full := fmt.Sprintf("ws_handshake value is not a boolean: %v", parseErr)

			wsURL := "ws://" + s.Server.Addr + "/v1/client/allocation/" + uuid.Generate() +
				"/exec?task=web&ws_handshake=" + url.QueryEscape(tc.value) +
				"&command=" + url.QueryEscape(`["true"]`)
			conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
			must.NoError(t, err)
			defer conn.Close()

			closeErr := mustWsCloseFrame(t, conn)
			must.Eq(t, websocket.ClosePolicyViolation, closeErr.Code)
			mustWsCutReason(t, full, closeErr.Text)
			mustWsConnClosed(t, conn)
		})
	}
}

// TestAllocExec_SessionErrorLongCloseReason: the error that ends an exec
// session reaches the client in a close frame when its text does not fit
// into one: the beginning of the text is the reason. The text is the error
// of the exec RPC, here of an allocation nobody knows, which quotes the
// allocation ID of the request. It went out whole, was refused by the
// websocket, and the client saw 1006 instead of the error of its session.
func TestAllocExec_SessionErrorLongCloseReason(t *testing.T) {
	ci.Parallel(t)

	httpTest(t, func(c *Config) {
		c.LogLevel = "ERROR"
	}, func(s *TestAgent) {
		// an allocation whose task has a long name and does not start
		// while the test runs: the exec RPC refuses it with an error that
		// quotes the name
		const oldTask = "web"
		task := strings.Repeat("t", 200)
		alloc := mockFSAlloc(s.client.NodeID(), map[string]interface{}{
			"start_block_for": "5m",
			"run_for":         "1s",
		})
		alloc.Job.TaskGroups[0].Tasks[0].Name = task
		alloc.TaskResources[task] = alloc.TaskResources[oldTask]
		delete(alloc.TaskResources, oldTask)
		alloc.AllocatedResources.Tasks[task] = alloc.AllocatedResources.Tasks[oldTask]
		delete(alloc.AllocatedResources.Tasks, oldTask)
		addAllocToClient(s, alloc, noWaitClientAlloc)

		testutil.WaitForResult(func() (bool, error) {
			state, err := s.client.GetAllocState(alloc.ID)
			if err != nil {
				return false, err
			}
			if state.TaskStates[task] == nil {
				return false, fmt.Errorf("the client has no state of the task yet")
			}
			return true, nil
		}, func(err error) {
			t.Fatal(err)
		})

		full := fmt.Sprintf("task %q not started yet.", task)

		wsURL := "ws://" + s.Server.Addr + "/v1/client/allocation/" + alloc.ID +
			"/exec?task=" + task + "&command=" + url.QueryEscape(`["true"]`)
		conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
		must.NoError(t, err)
		defer conn.Close()

		closeErr := mustWsCloseFrame(t, conn)
		must.Eq(t, websocket.ClosePolicyViolation, closeErr.Code)
		mustWsCutReason(t, full, closeErr.Text)
		mustWsConnClosed(t, conn)
	})
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
				websocket.FormatCloseMessage(toWsCode(400), wsCloseReason(err.Error())))
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

// The parts of a handshake message around its token.
const (
	wsTestHandshakeHead = `{"version":1,"auth_token":"`
	wsTestHandshakeTail = `"}`
)

// wsHandshakeOfSize is a handshake message of exactly size bytes; its token
// is as long as that needs.
func wsHandshakeOfSize(t *testing.T, size int) []byte {
	t.Helper()
	pad := size - len(wsTestHandshakeHead) - len(wsTestHandshakeTail)
	must.Positive(t, pad)
	return []byte(wsTestHandshakeHead + strings.Repeat("a", pad) + wsTestHandshakeTail)
}

// wsDialOneFrame dials with a write buffer that takes a message of size
// bytes whole, so the client sends it in one frame. The default dialer sends
// a larger message in frames of 4096 bytes.
func wsDialOneFrame(t *testing.T, wsURL string, size int) *websocket.Conn {
	t.Helper()
	dialer := websocket.Dialer{WriteBufferSize: size + 1024}
	conn, _, err := dialer.Dial(wsURL, nil)
	must.NoError(t, err)
	return conn
}

// mustWsTooBig: the server refuses what the client sent with the close frame
// "message too big" (1009). The server closes the connection with a part of
// the message unread, which resets it: a client that loses the close frame
// that way sees the reset. Anything else fails the test: a data message, a
// close frame with another code, or no answer.
func mustWsTooBig(t *testing.T, conn *websocket.Conn) {
	t.Helper()

	must.NoError(t, conn.SetReadDeadline(time.Now().Add(20*time.Second)))
	_, msg, err := conn.ReadMessage()
	must.Error(t, err, must.Sprintf("the server answered with a message: %.100q", msg))
	if websocket.IsCloseError(err, websocket.CloseMessageTooBig) {
		return
	}
	var closeErr *websocket.CloseError
	must.False(t, errors.As(err, &closeErr), must.Sprintf("unexpected close frame: %.300v", err))
	var netErr net.Error
	must.False(t, errors.As(err, &netErr) && netErr.Timeout(), must.Sprintf("no answer: %v", err))
	t.Logf("no close frame read, the connection ended with: %v", err)
}

// mustWsConnClosed: the server closed the connection (after its close
// frame, which the caller has read).
func mustWsConnClosed(t *testing.T, conn *websocket.Conn) {
	t.Helper()

	raw := conn.NetConn()
	must.NoError(t, raw.SetReadDeadline(time.Now().Add(10*time.Second)))
	_, err := raw.Read(make([]byte, 1))
	must.Error(t, err)
	var netErr net.Error
	must.False(t, errors.As(err, &netErr) && netErr.Timeout(), must.Sprintf("connection left open: %v", err))
}

// TestAllocExec_HandshakeTooBig: the exec handler does not read a handshake
// message of megabytes. The message carries the ACL token, so it is read
// before any token is checked; it was read whatever its size, and its token
// went on into the exec RPC.
func TestAllocExec_HandshakeTooBig(t *testing.T) {
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

	// the server ends the connection while the message is on its way, so
	// the write may fail
	must.NoError(t, conn.SetWriteDeadline(time.Now().Add(20*time.Second)))
	_ = conn.WriteMessage(websocket.TextMessage, wsHandshakeOfSize(t, 4<<20))

	mustWsTooBig(t, conn)
	mustWsConnClosed(t, conn)
}

// TestAllocExec_HandshakeAtLimitBadToken: a handshake message as large as
// the limit allows is read, and its bad token is refused the way a short bad
// token is.
func TestAllocExec_HandshakeAtLimitBadToken(t *testing.T) {
	ci.Parallel(t)

	httpACLTest(t, func(c *Config) {
		c.LogLevel = "ERROR"
	}, func(s *TestAgent) {
		wsURL := "ws://" + s.Server.Addr + "/v1/client/allocation/" + uuid.Generate() +
			"/exec?task=web&ws_handshake=true&command=" + url.QueryEscape(`["true"]`)

		answer := func(handshake []byte) error {
			conn := wsDialOneFrame(t, wsURL, len(handshake))
			defer conn.Close()
			must.NoError(t, conn.WriteMessage(websocket.TextMessage, handshake))
			must.NoError(t, conn.SetReadDeadline(time.Now().Add(20*time.Second)))
			_, msg, err := conn.ReadMessage()
			must.Error(t, err, must.Sprintf("the server answered with a message: %.100q", msg))
			return err
		}

		short := answer(wsHandshakeOfSize(t, 128))
		must.True(t, websocket.IsCloseError(short, websocket.CloseInternalServerErr),
			must.Sprintf("unexpected error: %v", short))
		must.StrContains(t, short.Error(), structs.ErrPermissionDenied.Error())

		atLimit := answer(wsHandshakeOfSize(t, wsHandshakeMaxBytes))
		must.Eq(t, short.Error(), atLimit.Error())
	})
}

// TestReadWsHandshakeWithin_SizeLimit: the handshake message may be
// wsHandshakeMaxBytes long. A longer one is refused with the close frame
// "message too big" as soon as its frames add up to more, and no session
// starts. The limit is over once the handshake is read, and a request
// without a handshake never has it: the messages of the session may be
// longer.
func TestReadWsHandshakeWithin_SizeLimit(t *testing.T) {
	ci.Parallel(t)

	const big = 4 << 20

	type handshakeResult struct {
		err      error // of readWsHandshakeWithin
		closeErr error // of the close frame the handler writes for err
	}

	// The handler does what allocExec does around the handshake, then
	// answers the first message of the session with the length of the token
	// it got and of that message.
	serve := func(t *testing.T) (string, <-chan handshakeResult) {
		results := make(chan handshakeResult, 1)
		upgrader := websocket.Upgrader{ReadBufferSize: 2048, WriteBufferSize: 2048}
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ws, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				return
			}
			defer ws.Close()

			var q structs.QueryOptions
			if err := readWsHandshakeWithin(ws, wsHandshakeTimeout, r, &q); err != nil {
				closeErr := ws.WriteMessage(websocket.CloseMessage,
					websocket.FormatCloseMessage(toWsCode(400), wsCloseReason(err.Error())))
				results <- handshakeResult{err: err, closeErr: closeErr}
				return
			}
			results <- handshakeResult{}
			_, msg, err := ws.ReadMessage()
			if err != nil {
				ws.WriteMessage(websocket.CloseMessage,
					websocket.FormatCloseMessage(websocket.CloseInternalServerErr, "session read failed"))
				return
			}
			ws.WriteMessage(websocket.TextMessage, []byte(fmt.Sprintf("%d %d", len(q.AuthToken), len(msg))))
		}))
		t.Cleanup(srv.Close)
		return "ws" + strings.TrimPrefix(srv.URL, "http"), results
	}

	tokenLen := func(handshakeSize int) int {
		return handshakeSize - len(wsTestHandshakeHead) - len(wsTestHandshakeTail)
	}

	cases := []struct {
		name      string
		handshake int  // size of the handshake message; 0: none is asked for
		oneFrame  bool // the client sends each message in one frame
		session   int  // size of the first message of the session
		tooBig    bool // the handshake message is refused
	}{
		{name: "handshake at the limit, one frame", handshake: wsHandshakeMaxBytes, oneFrame: true, session: 5},
		{name: "handshake at the limit, in frames", handshake: wsHandshakeMaxBytes, session: 5},
		{name: "handshake one byte over the limit, one frame", handshake: wsHandshakeMaxBytes + 1, oneFrame: true, session: 5, tooBig: true},
		{name: "handshake one byte over the limit, in frames", handshake: wsHandshakeMaxBytes + 1, session: 5, tooBig: true},
		{name: "handshake of megabytes, one frame", handshake: big, oneFrame: true, session: 5, tooBig: true},
		{name: "handshake of megabytes, in frames", handshake: big, session: 5, tooBig: true},
		{name: "session message of megabytes after the handshake, one frame", handshake: 64, oneFrame: true, session: big},
		{name: "session message of megabytes after the handshake, in frames", handshake: 64, session: big},
		{name: "no handshake asked, first message of megabytes, one frame", oneFrame: true, session: big},
		{name: "no handshake asked, first message of megabytes, in frames", session: big},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ci.Parallel(t)

			wsURL, results := serve(t)
			if tc.handshake > 0 {
				wsURL += "/?ws_handshake=true"
			}

			var conn *websocket.Conn
			if tc.oneFrame {
				conn = wsDialOneFrame(t, wsURL, max(tc.handshake, tc.session))
			} else {
				var err error
				conn, _, err = websocket.DefaultDialer.Dial(wsURL, nil)
				must.NoError(t, err)
			}
			defer conn.Close()
			must.NoError(t, conn.SetWriteDeadline(time.Now().Add(20*time.Second)))

			session := []byte(strings.Repeat("s", tc.session))

			if tc.tooBig {
				// the server ends the connection while the messages are
				// on their way, so the writes may fail
				_ = conn.WriteMessage(websocket.TextMessage, wsHandshakeOfSize(t, tc.handshake))
				_ = conn.WriteMessage(websocket.TextMessage, session)

				mustWsTooBig(t, conn)
				mustWsConnClosed(t, conn)

				res := <-results
				must.ErrorIs(t, res.err, websocket.ErrReadLimit)
				must.StrContains(t, res.err.Error(), fmt.Sprintf("handshake message larger than %d bytes", wsHandshakeMaxBytes))
				// the read sent the close frame; the one of the handler
				// for the error is not sent after it
				must.ErrorIs(t, res.closeErr, websocket.ErrCloseSent)
				return
			}

			wantToken := 0
			if tc.handshake > 0 {
				must.NoError(t, conn.WriteMessage(websocket.TextMessage, wsHandshakeOfSize(t, tc.handshake)))
				wantToken = tokenLen(tc.handshake)
			}
			must.NoError(t, conn.WriteMessage(websocket.TextMessage, session))

			must.NoError(t, conn.SetReadDeadline(time.Now().Add(20*time.Second)))
			_, msg, err := conn.ReadMessage()
			must.NoError(t, err)
			must.Eq(t, fmt.Sprintf("%d %d", wantToken, tc.session), string(msg))
			must.NoError(t, (<-results).err)
		})
	}
}
