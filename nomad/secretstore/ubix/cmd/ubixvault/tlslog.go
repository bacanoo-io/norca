package main

import (
	"context"
	"log"
	"regexp"
	"sync/atomic"
	"time"
)

// handshakeAbortLog filters the http.Server error log. Load balancers and
// ingress controllers health-check backends with bare TCP connects; against a
// TLS listener each one aborts the handshake, and net/http logs a line for it.
// On a normal cluster that is one line per probe, per replica, forever — enough
// to bury everything else in the log.
//
// Only the abort class is held back: the peer closed or reset the connection
// before a handshake (or an HTTP/2 preface) completed. Those lines are counted
// and reported as a periodic summary instead. Every other server error — a
// failed certificate, an unsupported TLS version, a panic in a handler — is
// written through unchanged.
type handshakeAbortLog struct {
	out        *log.Logger
	suppressed atomic.Uint64
}

func newHandshakeAbortLog(out *log.Logger) *handshakeAbortLog {
	return &handshakeAbortLog{out: out}
}

// Logger returns a *log.Logger for http.Server.ErrorLog. It adds no prefix or
// timestamp of its own, so each line reaches Write exactly as net/http
// formatted it and can be matched whole; out adds the timestamp on the way
// through.
func (l *handshakeAbortLog) Logger() *log.Logger {
	return log.New(l, "", 0)
}

func (l *handshakeAbortLog) Write(p []byte) (int, error) {
	if isHandshakeAbort(p) {
		l.suppressed.Add(1)
		return len(p), nil
	}
	return len(p), l.out.Output(2, string(p))
}

// Run logs how many aborts were held back, once per interval and only when
// there were any, until ctx is done.
func (l *handshakeAbortLog) Run(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if n := l.suppressed.Swap(0); n > 0 {
				l.out.Printf("%d aborted TLS handshakes in the last %s (connections closed before completing TLS, e.g. TCP health checks; -log-tls-handshake-aborts logs each)", n, every)
			}
		}
	}
}

// handshakeAbortLine matches, as a whole line, a TLS handshake or HTTP/2
// preface that failed only because the peer went away: the cause must be a
// bare EOF or a *net.OpError reset/broken pipe on the connection itself.
//
// It is anchored at both ends on purpose. Some handshake errors echo
// client-supplied bytes — an offered ALPN protocol name, a bogus HTTP/2
// greeting — so a client could put "connection reset by peer" inside its own
// failure. A substring match would then hide an attacker's handshake from the
// log; a whole-line match cannot be satisfied that way, because the echoed
// bytes are quoted and followed by more text.
var handshakeAbortLine = regexp.MustCompile(
	`^(?:http: TLS handshake error from|http2: server: error reading preface from client) \S+: ` +
		`(?:EOF|(?:read|write) tcp \S+->\S+: (?:read|write): (?:connection reset by peer|broken pipe))\n?$`)

// isHandshakeAbort reports whether a server log line (as net/http wrote it,
// without a timestamp) is a TLS handshake or HTTP/2 preface abandoned by the
// peer.
func isHandshakeAbort(line []byte) bool {
	return handshakeAbortLine.Match(line)
}
