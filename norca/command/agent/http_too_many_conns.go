// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package agent

import (
	"crypto/tls"
	"net"
	"time"
)

const (
	// tooManyConnsTimeout is the time a TLS client over
	// http_max_conns_per_client has for its handshake and to take the 429
	// when https_handshake_timeout is 0 (no timeout). It is the default of
	// https_handshake_timeout.
	tooManyConnsTimeout = 5 * time.Second

	// tooManyConnsWriters is the most TLS connections that are being told
	// 429 at a time. Each one holds a goroutine and a file descriptor for
	// as long as its client takes, the timeout at most; the rate limit of
	// the answers alone would allow 100 of them and 10 more for every
	// second of the timeout.
	tooManyConnsWriters = 100
)

// tooManyConnsTLS tells the TLS clients over http_max_conns_per_client 429.
//
// The answer can only go out after the TLS handshake, which takes the
// client's round trips, and the http.Server calls the hook that refuses a
// connection before it accepts the next one. So the handshake and the
// answer run in a goroutine of their own, on the network connection that
// was taken away from the server (connprefix.Detach), with one deadline for
// both. At most cap(slots) of them run at a time.
type tooManyConnsTLS struct {
	config   *tls.Config
	timeout  time.Duration
	response []byte
	slots    chan struct{}
}

// newTooManyConnsTLS returns nil without a TLS configuration, that is for a
// server without TLS. A handshakeTimeout of 0 means tooManyConnsTimeout.
func newTooManyConnsTLS(config *tls.Config, handshakeTimeout time.Duration, response []byte, writers int) *tooManyConnsTLS {
	if config == nil {
		return nil
	}
	if handshakeTimeout <= 0 {
		handshakeTimeout = tooManyConnsTimeout
	}
	// The answer is an HTTP/1.1 response: do not agree on HTTP/2.
	config = config.Clone()
	config.NextProtos = []string{"http/1.1"}

	return &tooManyConnsTLS{
		config:   config,
		timeout:  handshakeTimeout,
		response: response,
		slots:    make(chan struct{}, writers),
	}
}

// answer takes over conn, the network connection of a client, and closes
// it: after the handshake and the answer, or when the timeout is over, or
// at once and without an answer when too many answers are on their way. It
// does not wait for any of that, and reports whether an answer is on its
// way.
func (a *tooManyConnsTLS) answer(conn net.Conn) bool {
	select {
	case a.slots <- struct{}{}:
	default:
		conn.Close()
		return false
	}

	go func() {
		defer func() { <-a.slots }()

		tlsConn := tls.Server(conn, a.config)
		// Close also closes conn, with or without a handshake.
		defer tlsConn.Close()

		conn.SetDeadline(time.Now().Add(a.timeout))
		// Write does the handshake first.
		tlsConn.Write(a.response)
	}()
	return true
}
