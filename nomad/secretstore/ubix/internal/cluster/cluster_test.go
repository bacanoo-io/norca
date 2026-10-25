package cluster

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cwolsen7905/ubixvault/internal/storage"
)

// startCluster serves h on a loopback cluster listener secured by certs.
func startCluster(t *testing.T, certs *Certs, h http.Handler) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{Handler: h, TLSConfig: certs.ServerTLS(), ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.ServeTLS(ln, "", "") }()
	t.Cleanup(func() { _ = srv.Close() })
	return "https://" + ln.Addr().String()
}

func TestCAIsCreatedOnceAndSharedByReplicas(t *testing.T) {
	ctx := context.Background()
	mem := storage.NewMemoryBackend()
	active, standby := NewCerts(mem), NewCerts(mem)

	if _, _, err := standby.identity(ctx); !errors.Is(err, ErrNoCA) {
		t.Fatalf("standby identity before the CA exists = %v, want ErrNoCA", err)
	}
	if err := active.EnsureCA(ctx); err != nil {
		t.Fatalf("EnsureCA: %v", err)
	}
	first, _ := mem.Get(ctx, caPath)
	if err := NewCerts(mem).EnsureCA(ctx); err != nil { // another active, later
		t.Fatalf("second EnsureCA: %v", err)
	}
	second, _ := mem.Get(ctx, caPath)
	if string(first.Value) != string(second.Value) {
		t.Fatal("EnsureCA replaced an existing CA")
	}

	poolA, leafA, err := active.identity(ctx)
	if err != nil {
		t.Fatalf("active identity: %v", err)
	}
	_, leafS, err := standby.identity(ctx)
	if err != nil {
		t.Fatalf("standby identity: %v", err)
	}
	for name, leaf := range map[string]*tls.Certificate{"active": leafA, "standby": leafS} {
		cert, _ := x509.ParseCertificate(leaf.Certificate[0])
		if _, err := cert.Verify(x509.VerifyOptions{
			Roots: poolA, DNSName: serverName,
			KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		}); err != nil {
			t.Fatalf("%s leaf does not verify against the shared CA: %v", name, err)
		}
	}
}

func TestLeafIsReissuedAtHalfLife(t *testing.T) {
	ctx := context.Background()
	c := NewCerts(storage.NewMemoryBackend())
	now := time.Now()
	c.now = func() time.Time { return now }
	if err := c.EnsureCA(ctx); err != nil {
		t.Fatalf("EnsureCA: %v", err)
	}
	_, first, _ := c.identity(ctx)
	now = now.Add(leafTTL/2 - time.Minute)
	if _, same, _ := c.identity(ctx); same != first {
		t.Fatal("leaf reissued before half-life")
	}
	now = now.Add(2 * time.Minute)
	if _, fresh, _ := c.identity(ctx); fresh == first {
		t.Fatal("leaf not reissued after half-life")
	}
}

// TestClusterListenerRequiresClusterCert: only a peer holding a leaf from the
// vault's own CA gets through — not a client without a certificate, and not
// one with a certificate from some other CA.
func TestClusterListenerRequiresClusterCert(t *testing.T) {
	ctx := context.Background()
	mem := storage.NewMemoryBackend()
	server := NewCerts(mem)
	if err := server.EnsureCA(ctx); err != nil {
		t.Fatalf("EnsureCA: %v", err)
	}
	url := startCluster(t, server, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	get := func(cfg *tls.Config) error {
		c := &http.Client{Transport: &http.Transport{TLSClientConfig: cfg}, Timeout: 5 * time.Second}
		resp, err := c.Get(url)
		if err == nil {
			_ = resp.Body.Close()
		}
		return err
	}

	peerCfg, err := NewCerts(mem).clientTLS(ctx) // a replica of the same vault
	if err != nil {
		t.Fatalf("clientTLS: %v", err)
	}
	if err := get(peerCfg); err != nil {
		t.Fatalf("replica of the same vault rejected: %v", err)
	}

	noCert := peerCfg.Clone()
	noCert.Certificates = nil
	if err := get(noCert); err == nil {
		t.Fatal("client without a certificate was accepted")
	}

	strangerCfg, err := NewCerts(storage.NewMemoryBackend()).mustCA(t).clientTLS(ctx) // another vault
	if err != nil {
		t.Fatalf("stranger clientTLS: %v", err)
	}
	strangerCfg.RootCAs = peerCfg.RootCAs // it trusts our server, but its own cert is foreign
	if err := get(strangerCfg); err == nil {
		t.Fatal("client with a certificate from another CA was accepted")
	}

	selfSigned := peerCfg.Clone()
	selfSigned.Certificates = []tls.Certificate{selfSignedLeaf(t)}
	if err := get(selfSigned); err == nil {
		t.Fatal("client with a self-signed certificate was accepted")
	}
}

func (c *Certs) mustCA(t *testing.T) *Certs {
	t.Helper()
	if err := c.EnsureCA(context.Background()); err != nil {
		t.Fatalf("EnsureCA: %v", err)
	}
	return c
}

func selfSignedLeaf(t *testing.T) tls.Certificate {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: randomSerial(), Subject: pkix.Name{CommonName: serverName}, DNSNames: []string{serverName},
		NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("self-signed: %v", err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// TestForwardCarriesClientAddress: the active replica sees the original
// client's address — never one a client tried to assert itself — and the
// original X-Forwarded-For.
func TestForwardCarriesClientAddress(t *testing.T) {
	ctx := context.Background()
	mem := storage.NewMemoryBackend()
	activeCerts := NewCerts(mem)
	if err := activeCerts.EnsureCA(ctx); err != nil {
		t.Fatalf("EnsureCA: %v", err)
	}
	var seenAddr, seenHeader, seenXFF, seenBody string
	api := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAddr, seenHeader = r.RemoteAddr, r.Header.Get(clientAddrHeader)
		seenXFF = r.Header.Get("X-Forwarded-For")
		b, _ := io.ReadAll(r.Body)
		seenBody = string(b)
		w.Header().Set("X-Test", "from-active")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, "active says hi")
	})
	url := startCluster(t, activeCerts, ServerHandler(api, func() bool { return true }))
	fwd := NewForwarder(NewCerts(mem), func(context.Context) (string, bool, error) { return url, false, nil })

	req := httptest.NewRequest("POST", "/v1/secret/data/app?x=1", strings.NewReader(`{"data":{}}`))
	req.RemoteAddr = "203.0.113.9:4444"
	req.Header.Set(clientAddrHeader, "10.0.0.1:1") // a client trying to spoof its address
	req.Header.Set("X-Forwarded-For", "198.51.100.7")
	rec := httptest.NewRecorder()
	fwd.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated || rec.Body.String() != "active says hi" || rec.Header().Get("X-Test") != "from-active" {
		t.Fatalf("relayed response = %d %q %v", rec.Code, rec.Body, rec.Header())
	}
	if seenAddr != "203.0.113.9:4444" {
		t.Fatalf("active saw client address %q, want the standby's view 203.0.113.9:4444", seenAddr)
	}
	if seenHeader != "" {
		t.Fatalf("cluster header leaked to the API handler: %q", seenHeader)
	}
	if seenXFF != "198.51.100.7" {
		t.Fatalf("X-Forwarded-For = %q, want it carried through", seenXFF)
	}
	if seenBody != `{"data":{}}` {
		t.Fatalf("body = %q", seenBody)
	}
}

// deadAddr is a cluster URL nothing listens on.
func deadAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := "https://" + ln.Addr().String()
	_ = ln.Close()
	return addr
}

// liveActive starts a cluster listener answering "active: <method> <body>".
func liveActive(t *testing.T, certs *Certs) string {
	t.Helper()
	return startCluster(t, certs, ServerHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, "active: "+r.Method+" "+string(b)) //nolint:gosec // G705: test echo server, plain text, never rendered
	}), func() bool { return true }))
}

func clusterCerts(t *testing.T) *Certs {
	t.Helper()
	return NewCerts(storage.NewMemoryBackend()).mustCA(t)
}

func TestForwardGivesUpWithoutLeader(t *testing.T) {
	fwd := NewForwarder(clusterCerts(t), func(context.Context) (string, bool, error) { return "", false, nil })
	fwd.wait = 200 * time.Millisecond
	start := time.Now()
	rec := httptest.NewRecorder()
	fwd.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/secret/data/x", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no leader = %d, want 503", rec.Code)
	}
	if took := time.Since(start); took < 200*time.Millisecond {
		t.Fatalf("gave up after %v, before waiting for a leader", took)
	}
}

// TestForwardWaitsForElection: a request arriving while no replica is active
// — a planned handoff in progress — is held until one is, then forwarded, body
// and all, instead of failing.
func TestForwardWaitsForElection(t *testing.T) {
	certs := clusterCerts(t)
	live := liveActive(t, certs)
	var electedAt atomic.Int64
	electedAt.Store(time.Now().Add(300 * time.Millisecond).UnixNano())
	fwd := NewForwarder(certs, func(context.Context) (string, bool, error) {
		if time.Now().UnixNano() < electedAt.Load() {
			return "", false, nil
		}
		return live, false, nil
	})
	for _, tc := range []struct{ method, body string }{{"GET", ""}, {"POST", `{"k":"v"}`}} {
		electedAt.Store(time.Now().Add(300 * time.Millisecond).UnixNano())
		fwd.forget()
		rec := httptest.NewRecorder()
		fwd.ServeHTTP(rec, httptest.NewRequest(tc.method, "/v1/secret/data/x", strings.NewReader(tc.body)))
		if want := "active: " + tc.method + " " + tc.body; rec.Code != http.StatusOK || rec.Body.String() != want {
			t.Fatalf("%s during an election = %d %q, want 200 %q", tc.method, rec.Code, rec.Body, want)
		}
	}
}

// TestForwardRetriesBodylessOnNewLeader: a read sent to a leader that has just
// gone away — or that answers it is no longer active — is re-sent to the new
// one. A request with a body is not: it may already have been consumed.
func TestForwardRetriesBodylessOnNewLeader(t *testing.T) {
	certs := clusterCerts(t)
	live := liveActive(t, certs)
	stale := startCluster(t, certs, ServerHandler(http.NotFoundHandler(), func() bool { return false }))

	for name, old := range map[string]string{"gone": deadAddr(t), "no longer active": stale} {
		t.Run(name, func(t *testing.T) {
			var calls atomic.Int32
			leader := func(context.Context) (string, bool, error) {
				if calls.Add(1) == 1 {
					return old, false, nil // what the cache still says
				}
				return live, false, nil
			}
			fwd := NewForwarder(certs, leader)
			rec := httptest.NewRecorder()
			fwd.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/secret/data/x", nil))
			if rec.Code != http.StatusOK || rec.Body.String() != "active: GET " {
				t.Fatalf("GET after the leader moved = %d %q, want it retried on the new leader", rec.Code, rec.Body)
			}

			calls.Store(0)
			fwd = NewForwarder(certs, leader)
			rec = httptest.NewRecorder()
			fwd.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/secret/data/x", strings.NewReader(`{"k":"v"}`)))
			if rec.Code != http.StatusServiceUnavailable {
				t.Fatalf("POST after the leader moved = %d %q, want 503 (a body is never sent twice)", rec.Code, rec.Body)
			}
		})
	}
}

// TestForwardServesLocallyOnceSelfIsActive: a request that waited while this
// very replica won the election is served here — but only once it is serving
// as active, not merely holding the lock.
func TestForwardServesLocallyOnceSelfIsActive(t *testing.T) {
	var active atomic.Bool
	var electedAt atomic.Int64
	electedAt.Store(time.Now().Add(200 * time.Millisecond).UnixNano())
	fwd := NewForwarder(clusterCerts(t), func(context.Context) (string, bool, error) {
		if time.Now().UnixNano() < electedAt.Load() {
			return "", false, nil
		}
		return "https://self:8201", true, nil // this replica holds the lock...
	})
	fwd.ServeLocally(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if !active.Load() {
			t.Error("served locally before this replica was active")
		}
		_, _ = io.WriteString(w, "local")
	}), active.Load)
	go func() {
		time.Sleep(400 * time.Millisecond) // ...and finishes becoming active later
		active.Store(true)
	}()
	rec := httptest.NewRecorder()
	fwd.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/secret/data/x", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "local" {
		t.Fatalf("request held while this replica became active = %d %q, want it served locally", rec.Code, rec.Body)
	}
}

func TestServerHandlerRefusesWhenNotActive(t *testing.T) {
	called := false
	h := ServerHandler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }),
		func() bool { return false })
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/secret/data/x", nil))
	if rec.Code != http.StatusServiceUnavailable || called {
		t.Fatalf("not-active cluster request = %d (handler called: %v), want 503 without forwarding on", rec.Code, called)
	}
}
