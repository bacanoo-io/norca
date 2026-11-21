// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/helper/pointer"
	"github.com/bacanoo-io/norca/norca/norca/structs/config"
	"github.com/shoenig/test/must"
	"github.com/shoenig/test/wait"
)

const (
	tooManyConnsTestCA   = "../../helper/tlsutil/testdata/norca-agent-ca.pem"
	tooManyConnsTestCert = "../../helper/tlsutil/testdata/regionFoo-server-norca.pem"
	tooManyConnsTestKey  = "../../helper/tlsutil/testdata/regionFoo-server-norca-key.pem"
)

// A TLS client over http_max_conns_per_client gets the 429 however long its
// handshake takes within https_handshake_timeout: the server gave itself
// 10ms for the handshake and the answer together, so a client that was not
// that fast (a round trip of more than a few milliseconds, or a busy
// server) only saw the connection closed (review finding MUK-34). A client
// that never finishes the handshake is closed at the handshake timeout.
func TestHTTP_ConnLimit_TLSAnswer(t *testing.T) {
	ci.Parallel(t)

	const handshakeTimeout = 3 * time.Second
	s := makeHTTPServer(t, func(c *Config) {
		c.Region = "regionFoo"
		c.TLSConfig = &config.TLSConfig{
			EnableHTTP: true,
			CAFile:     tooManyConnsTestCA,
			CertFile:   tooManyConnsTestCert,
			KeyFile:    tooManyConnsTestKey,
		}
		c.Limits.HTTPSHandshakeTimeout = handshakeTimeout.String()
		c.Limits.HTTPMaxConnsPerClient = pointer.Of(1)
		c.LogLevel = "ERROR"
	})
	defer s.Shutdown()
	addr := s.Server.Addr
	clientTLS := &tls.Config{InsecureSkipVerify: true}

	// the one connection the limit allows
	first, err := tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, "tcp", addr, clientTLS)
	must.NoError(t, err)
	defer first.Close()

	// A client over the limit that starts its handshake late.
	slow, err := net.DialTimeout("tcp", addr, 5*time.Second)
	must.NoError(t, err)
	defer slow.Close()
	// A client over the limit that never starts it.
	stalled, err := net.DialTimeout("tcp", addr, 5*time.Second)
	must.NoError(t, err)
	defer stalled.Close()
	stalledAt := time.Now()

	time.Sleep(200 * time.Millisecond)
	must.NoError(t, slow.SetDeadline(time.Now().Add(10*time.Second)))
	slowTLS := tls.Client(slow, clientTLS)
	must.NoError(t, slowTLS.Handshake(), must.Sprint("the handshake of a client over the limit"))

	// The answer is the 429 and nothing else: the connection is closed.
	resp, err := http.ReadResponse(bufio.NewReader(slowTLS), nil)
	must.NoError(t, err)
	must.Eq(t, http.StatusTooManyRequests, resp.StatusCode)
	body, err := io.ReadAll(resp.Body)
	must.NoError(t, err)
	must.StrContains(t, string(body), "too many concurrent connections")
	rest, err := io.ReadAll(slowTLS)
	must.NoError(t, err)
	must.Eq(t, "", string(rest))

	// The stalled client is closed when the handshake timeout is over, not
	// before and not much later.
	must.NoError(t, stalled.SetReadDeadline(time.Now().Add(handshakeTimeout+10*time.Second)))
	n, err := stalled.Read(make([]byte, 1))
	must.Eq(t, 0, n)
	must.ErrorIs(t, err, io.EOF)
	must.GreaterEq(t, handshakeTimeout-500*time.Millisecond, time.Since(stalledAt))

	// The limit still counts as before: the first connection holds the
	// slot, and once it is closed another connection is served.
	must.NoError(t, first.Close())
	must.Wait(t, wait.InitialSuccess(
		wait.ErrorFunc(func() error {
			conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, "tcp", addr, clientTLS)
			if err != nil {
				return err
			}
			defer conn.Close()
			if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
				return err
			}
			if _, err := conn.Write([]byte("GET /v1/agent/health HTTP/1.1\r\nHost: test\r\nConnection: close\r\n\r\n")); err != nil {
				return err
			}
			resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
			if err != nil {
				return err
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				return fmt.Errorf("status %s", resp.Status)
			}
			return nil
		}),
		wait.Timeout(10*time.Second),
		wait.Gap(200*time.Millisecond),
	))
}

const tooManyConnsTestResponse = "HTTP/1.1 429 Too Many Requests\r\nContent-Length: 0\r\nConnection: close\r\n\r\n"

func newTestTooManyConnsTLS(t *testing.T, timeout time.Duration, writers int) *tooManyConnsTLS {
	cert, err := tls.LoadX509KeyPair(tooManyConnsTestCert, tooManyConnsTestKey)
	must.NoError(t, err)
	return newTooManyConnsTLS(&tls.Config{
		Certificates: []tls.Certificate{cert},
		NextProtos:   []string{"h2", "http/1.1"},
	}, timeout, []byte(tooManyConnsTestResponse), writers)
}

// slotsFree waits until no answer of a is on its way.
func slotsFree(t *testing.T, a *tooManyConnsTLS) {
	t.Helper()
	must.Wait(t, wait.InitialSuccess(
		wait.BoolFunc(func() bool { return len(a.slots) == 0 }),
		wait.Timeout(10*time.Second),
		wait.Gap(10*time.Millisecond),
	))
}

// The answer comes after the handshake, as HTTP/1.1 also for a client that
// offers HTTP/2, and then the connection is closed.
func TestTooManyConnsTLS_Answer(t *testing.T) {
	ci.Parallel(t)

	must.Nil(t, newTooManyConnsTLS(nil, time.Second, nil, 1))
	must.Eq(t, tooManyConnsTimeout, newTestTooManyConnsTLS(t, 0, 1).timeout)

	a := newTestTooManyConnsTLS(t, 10*time.Second, 1)
	server, client := net.Pipe()
	defer client.Close()
	must.True(t, a.answer(server))

	must.NoError(t, client.SetDeadline(time.Now().Add(10*time.Second)))
	tlsClient := tls.Client(client, &tls.Config{
		InsecureSkipVerify: true,
		NextProtos:         []string{"h2", "http/1.1"},
	})
	must.NoError(t, tlsClient.Handshake())
	must.Eq(t, "http/1.1", tlsClient.ConnectionState().NegotiatedProtocol)

	resp, err := http.ReadResponse(bufio.NewReader(tlsClient), nil)
	must.NoError(t, err)
	must.Eq(t, http.StatusTooManyRequests, resp.StatusCode)
	rest, err := io.ReadAll(tlsClient)
	must.NoError(t, err)
	must.Eq(t, "", string(rest))

	slotsFree(t, a)
}

// No more answers are on their way than there are slots: a connection
// beyond them is closed at once, without an answer. A client that does not
// do the handshake is closed when the timeout is over, and its slot is free
// again.
func TestTooManyConnsTLS_Slots(t *testing.T) {
	ci.Parallel(t)

	const timeout = 2 * time.Second
	a := newTestTooManyConnsTLS(t, timeout, 2)

	// closedWithoutAnswer reads the end of a connection the server closed.
	closedWithoutAnswer := func(client net.Conn) {
		t.Helper()
		// a pipe whose other end is closed already refuses the deadline
		_ = client.SetReadDeadline(time.Now().Add(timeout + 10*time.Second))
		n, err := client.Read(make([]byte, 1))
		must.Eq(t, 0, n)
		must.ErrorIs(t, err, io.EOF)
	}

	// two clients that never start the handshake hold the two slots
	start := time.Now()
	var stalled [2]net.Conn
	for i := range stalled {
		server, client := net.Pipe()
		defer client.Close()
		must.True(t, a.answer(server))
		stalled[i] = client
	}

	// a third one is closed at once
	server, client := net.Pipe()
	defer client.Close()
	must.False(t, a.answer(server))
	closedWithoutAnswer(client)
	must.Less(t, timeout, time.Since(start))

	// the stalled ones when the timeout is over
	for _, client := range stalled {
		closedWithoutAnswer(client)
	}
	must.GreaterEq(t, timeout-100*time.Millisecond, time.Since(start))
	slotsFree(t, a)

	// then the next connection is answered again; its client goes away
	server, client = net.Pipe()
	must.True(t, a.answer(server))
	must.NoError(t, client.Close())
	slotsFree(t, a)
}
