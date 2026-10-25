package main

import (
	"bytes"
	"crypto/tls"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestIsHandshakeAbort(t *testing.T) {
	cases := map[string]bool{
		// Lines from a live cluster: TCP health checks against the TLS port.
		"http: TLS handshake error from 10.42.7.127:48550: write tcp 10.42.5.143:8200->10.42.7.127:48550: write: connection reset by peer\n":               true,
		"http2: server: error reading preface from client 10.42.5.65:36228: read tcp 10.42.5.143:8200->10.42.5.65:36228: read: connection reset by peer\n": true,
		"http: TLS handshake error from 10.42.7.127:48551: EOF\n":                                                                                          true,
		"http: TLS handshake error from 10.42.7.127:48552: write tcp 10.42.5.143:8200->10.42.7.127:48552: write: broken pipe\n":                            true,
		"http: TLS handshake error from [fd00::7]:48553: read tcp [fd00::1]:8200->[fd00::7]:48553: read: connection reset by peer\n":                       true,

		// Real handshake failures an operator needs to see.
		"http: TLS handshake error from 10.0.0.9:5000: tls: client offered only unsupported versions: [301]\n": false,
		"http: TLS handshake error from 10.0.0.9:5001: remote error: tls: bad certificate\n":                   false,
		"http: TLS handshake error from 10.0.0.9:5002: client sent an HTTP request to an HTTPS server\n":       false,
		"http: TLS handshake error from 10.0.0.9:5003: read tcp 10.0.0.1:8200->10.0.0.9:5003: i/o timeout\n":   false,

		// Client-controlled bytes echoed into the error must not smuggle a line
		// past the filter by containing the words it looks for.
		`http: TLS handshake error from 10.0.0.9:5004: tls: client requested unsupported application protocols (["x: connection reset by peer"])` + "\n": false,
		`http: TLS handshake error from 10.0.0.9:5005: tls: client requested unsupported application protocols (["x: EOF"])` + "\n":                      false,
		`http2: server: error reading preface from client 10.0.0.9:5006: bogus greeting "PRI * HTTP/2.0\r\n\r\nSM\r\nX: broken pipe"` + "\n":             false,

		// Anything else net/http logs is untouched, even if it mentions a reset.
		"http: panic serving 10.0.0.9:5007: connection reset by peer\n": false,
		"http: Accept error: accept tcp: too many open files\n":         false,
	}
	for line, want := range cases {
		if got := isHandshakeAbort([]byte(line)); got != want {
			t.Errorf("isHandshakeAbort(%q) = %v, want %v", line, got, want)
		}
	}
}

// syncBuffer is a bytes.Buffer safe for the server's logging goroutines.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// TestHandshakeAbortLogAgainstTLSServer drives a real TLS server, so the test
// fails if net/http changes the wording of the lines being filtered.
func TestHandshakeAbortLogAgainstTLSServer(t *testing.T) {
	var out syncBuffer
	filter := newHandshakeAbortLog(log.New(&out, "", 0))

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	srv.Config.ErrorLog = filter.Logger()
	srv.StartTLS()
	defer srv.Close()
	addr := srv.Listener.Addr().String()

	// A TCP health check: connect, then close without speaking TLS.
	const probes = 3
	for range probes {
		conn, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		_ = conn.Close()
	}

	// A genuine failure: a client speaking plain HTTP to the TLS port.
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = conn.Write([]byte("GET / HTTP/1.0\r\n\r\n"))
	_, _ = conn.Read(make([]byte, 512))
	_ = conn.Close()

	// A client smuggling the filtered words in through ALPN is still logged.
	if c, err := tls.Dial("tcp", addr, &tls.Config{
		InsecureSkipVerify: true, //nolint:gosec // test client; the handshake is meant to fail
		NextProtos:         []string{"x: connection reset by peer"},
	}); err == nil {
		_ = c.Close()
		t.Fatal("handshake with an unknown ALPN protocol unexpectedly succeeded")
	}

	deadline := time.Now().Add(5 * time.Second)
	for filter.suppressed.Load() < probes || strings.Count(out.String(), "\n") < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("suppressed = %d, want %d; log = %q", filter.suppressed.Load(), probes, out.String())
		}
		time.Sleep(10 * time.Millisecond)
	}

	logged := out.String()
	for _, want := range []string{"HTTP request to an HTTPS server", "unsupported application protocols"} {
		if !strings.Contains(logged, want) {
			t.Errorf("want %q logged, got %q", want, logged)
		}
	}
	if n := filter.suppressed.Load(); n != probes {
		t.Errorf("suppressed = %d, want exactly %d", n, probes)
	}
}
