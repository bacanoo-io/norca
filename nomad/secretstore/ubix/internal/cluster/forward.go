package cluster

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync"
	"time"
)

// clientAddrHeader carries the original client's address from a standby to the
// active replica. The active replica honours it only on the cluster listener,
// where every peer has proven it unsealed the same vault; anywhere else it is
// ignored, and a standby always overwrites whatever a client sent.
const clientAddrHeader = "X-Ubixvault-Cluster-Client-Addr"

// leaderCacheTTL bounds how long a standby reuses the leader's address before
// asking storage again; a failed forward drops it at once.
const leaderCacheTTL = time.Second

// LeaderAddr reports the active replica's cluster URL, or "" if there is none
// right now. self is true when the active replica is this one.
type LeaderAddr func(ctx context.Context) (addr string, self bool, err error)

// leaderWait is how long a standby holds a request while no replica is active
// (an election is in progress, e.g. during a planned handoff) before answering
// 503. Waiting is always safe: nothing has been sent anywhere yet.
const leaderWait = 5 * time.Second

// notActiveHeader marks a cluster listener's refusal from a replica that is no
// longer active, so the forwarder can tell it from the API's own 503s.
const notActiveHeader = "X-Ubixvault-Cluster-Not-Active"

var errNotActive = errors.New("cluster: forwarded to a replica that is no longer active")

// Forwarder is the standby side: it proxies a request to the active replica's
// cluster listener and relays the response unchanged.
type Forwarder struct {
	certs  *Certs
	leader LeaderAddr
	proxy  *httputil.ReverseProxy
	wait   time.Duration // leaderWait; shortened in tests

	// A request waiting here for a leader may find the leader is this replica
	// — it won the election while the request waited. It is then served by
	// local, once selfActive confirms this replica is serving as active.
	local      http.Handler
	selfActive func() bool

	mu         sync.Mutex
	cached     string
	cachedSelf bool
	cachedAt   time.Time
}

// ServeLocally sets where a waiting request goes when this replica itself
// becomes the active one: h, the replica's own API, once active reports true.
func (f *Forwarder) ServeLocally(h http.Handler, active func() bool) {
	f.local, f.selfActive = h, active
}

// NewForwarder returns a forwarder that finds the active replica with leader
// and authenticates to it with certs.
func NewForwarder(certs *Certs, leader LeaderAddr) *Forwarder {
	f := &Forwarder{certs: certs, leader: leader, wait: leaderWait}
	transport := &http.Transport{
		DialTLSContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			cfg, err := certs.clientTLS(ctx)
			if err != nil {
				return nil, err
			}
			d := &tls.Dialer{NetDialer: &net.Dialer{Timeout: 5 * time.Second}, Config: cfg}
			return d.DialContext(ctx, network, addr)
		},
		TLSHandshakeTimeout: 5 * time.Second,
		IdleConnTimeout:     90 * time.Second,
		MaxIdleConnsPerHost: 32,
	}
	f.proxy = &httputil.ReverseProxy{
		Transport: transport,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(pr.In.Context().Value(attemptKey{}).(*attempt).target)
			pr.Out.Header.Set(clientAddrHeader, pr.In.RemoteAddr)
			// Rewrite drops X-Forwarded-For; carry it through unchanged so the
			// active replica applies the same -rate-limit-trust-forwarded
			// decision the standby would have.
			if xff, ok := pr.In.Header["X-Forwarded-For"]; ok {
				pr.Out.Header["X-Forwarded-For"] = xff
			}
		},
		ModifyResponse: func(resp *http.Response) error {
			if resp.Header.Get(notActiveHeader) != "" {
				return errNotActive // leadership moved while the request was in flight
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			a := r.Context().Value(attemptKey{}).(*attempt)
			if a.canRetry && retryable(err) {
				a.err = err // nothing written; ServeHTTP tries the new leader
				return
			}
			f.forget()
			writeError(w, http.StatusServiceUnavailable, "the active replica is unreachable; retry")
		},
	}
	return f
}

// attempt is one try at forwarding a request.
type attempt struct {
	target   *url.URL
	canRetry bool  // the request can be sent again (it has no body to replay)
	err      error // set when the try failed in a way worth retrying
}

type attemptKey struct{}

// retryable reports whether a failed forward never reached a live active
// replica: the old leader's address refused the connection, or answered that
// it is no longer active.
func retryable(err error) bool {
	var opErr *net.OpError
	return errors.Is(err, errNotActive) || (errors.As(err, &opErr) && opErr.Op == "dial")
}

// ServeHTTP forwards r to the active replica. While there is none it waits (up
// to leaderWait) for one; a request without a body is also retried against a
// new leader when the one it was sent to turns out to be gone. A request with
// a body is never sent twice.
func (f *Forwarder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	deadline := time.Now().Add(f.wait)
	bodyless := r.Body == nil || r.Body == http.NoBody
	for {
		addr, self, err := f.leaderAddr(r.Context())
		if err == nil && self && f.local != nil && f.selfActive() {
			f.local.ServeHTTP(w, r)
			return
		}
		if err == nil && !self && addr != "" {
			target, perr := url.Parse(addr)
			if perr != nil || target.Host == "" {
				f.forget()
				writeError(w, http.StatusServiceUnavailable, "the active replica advertises an invalid cluster address")
				return
			}
			target.Scheme = "https" // the cluster listener is always TLS
			a := &attempt{target: target, canRetry: bodyless && time.Now().Before(deadline)}
			f.proxy.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), attemptKey{}, a)))
			if a.err == nil {
				return // relayed, or a final error already written
			}
			f.forget()
		}
		if time.Now().After(deadline) || r.Context().Err() != nil {
			writeError(w, http.StatusServiceUnavailable, "no active replica right now; retry")
			return
		}
		select {
		case <-r.Context().Done():
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func (f *Forwarder) leaderAddr(ctx context.Context) (string, bool, error) {
	f.mu.Lock()
	if f.cached != "" && time.Since(f.cachedAt) < leaderCacheTTL {
		addr, self := f.cached, f.cachedSelf
		f.mu.Unlock()
		return addr, self, nil
	}
	f.mu.Unlock()
	addr, self, err := f.leader(ctx)
	if err != nil {
		return "", false, err
	}
	f.mu.Lock()
	f.cached, f.cachedSelf, f.cachedAt = addr, self, time.Now()
	f.mu.Unlock()
	return addr, self, nil
}

func (f *Forwarder) forget() {
	f.mu.Lock()
	f.cached = ""
	f.mu.Unlock()
}

// ServerHandler is the active side: it serves the API to forwarded requests
// arriving on the cluster listener. A request reaching a replica that is not
// active (leadership moved while it was in flight) is refused rather than
// forwarded again, so a request never loops between replicas.
func ServerHandler(api http.Handler, active func() bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !active() {
			w.Header().Set(notActiveHeader, "1")
			writeError(w, http.StatusServiceUnavailable, "this replica is no longer active; retry")
			return
		}
		if addr := r.Header.Get(clientAddrHeader); addr != "" {
			r.RemoteAddr = addr
		}
		r.Header.Del(clientAddrHeader)
		api.ServeHTTP(w, r)
	})
}

// writeError matches the API's error body ({"errors": [...]}).
func writeError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string][]string{"errors": {msg}})
}
