// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/hashicorp/nomad/ci"
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
