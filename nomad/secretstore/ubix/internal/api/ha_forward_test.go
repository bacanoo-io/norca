package api

import (
	"context"
	"net"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/cwolsen7905/ubixvault/internal/audit"
	"github.com/cwolsen7905/ubixvault/internal/cluster"
	"github.com/cwolsen7905/ubixvault/internal/core"
	"github.com/cwolsen7905/ubixvault/internal/storage"
	"github.com/cwolsen7905/ubixvault/internal/storage/storagetest"
)

type fwdReplica struct {
	core     *core.Core
	h        *Handler
	auditLog string
}

// forwardingPair wires two replicas the way the server does: shared storage, a
// real mutual-TLS cluster listener each, and standby forwarding. It returns
// (active, standby) once one is active, plus the root token.
func forwardingPair(t *testing.T) (*fwdReplica, *fwdReplica, string) {
	t.Helper()
	group := storagetest.NewHAGroup(storage.NewMemoryBackend())
	kek := make([]byte, 32)
	kek[0] = 9
	var rs []*fwdReplica
	for _, id := range []string{"r0", "r1"} {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		phys := group.Replica()
		c := core.New(phys, core.WithAutoUnsealKey(kek), core.WithHA(core.HAConfig{
			Backend: phys, HolderID: id, Advertise: "https://api-" + id, ClusterAddr: "https://" + ln.Addr().String(),
		}))
		certs := cluster.NewCerts(c.Barrier())
		c.OnActive(func() { _ = certs.EnsureCA(context.Background()) })
		c.OnSeal(certs.Reset)
		fwd := cluster.NewForwarder(certs, func(ctx context.Context) (string, bool, error) {
			info, err := c.Leader(ctx)
			return info.ClusterAddr, info.IsSelf, err
		})
		logPath := filepath.Join(t.TempDir(), "audit.log")
		device, err := audit.NewFileDevice(logPath)
		if err != nil {
			t.Fatalf("audit: %v", err)
		}
		h := NewHandler(c, WithForwarder(fwd), WithAudit(audit.NewBroker(device)))
		fwd.ServeLocally(h, c.Active)
		srv := &http.Server{Handler: cluster.ServerHandler(h, c.Active), TLSConfig: certs.ServerTLS(), ReadHeaderTimeout: 5 * time.Second}
		go func() { _ = srv.ServeTLS(ln, "", "") }()
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { defer close(done); _ = c.RunHA(ctx, func(string, ...any) {}) }()
		t.Cleanup(func() { cancel(); <-done; _ = srv.Close() })
		rs = append(rs, &fwdReplica{core: c, h: h, auditLog: logPath})
	}
	init := decode[initResponse](t, do(t, rs[0].h, "POST", "/v1/sys/init", `{}`))
	if err := rs[1].core.AutoUnseal(context.Background()); err != nil {
		t.Fatalf("AutoUnseal: %v", err)
	}
	waitFor(t, "one active replica", func() bool { return rs[0].core.Active() != rs[1].core.Active() })
	if rs[0].core.Active() {
		return rs[0], rs[1], init.RootToken
	}
	return rs[1], rs[0], init.RootToken
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for: %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestHAStandbyForwardsToActive(t *testing.T) {
	active, standby, root := forwardingPair(t)

	rec := doAuth(t, standby.h, "POST", "/v1/secret/data/app", `{"data":{"k":"via-standby"}}`, root)
	if rec.Code != http.StatusOK {
		t.Fatalf("write via standby = %d %s", rec.Code, rec.Body)
	}
	got := doAuth(t, active.h, "GET", "/v1/secret/data/app", "", root)
	if got.Code != http.StatusOK || !contains(got.Body.String(), "via-standby") {
		t.Fatalf("read on active = %d %s", got.Code, got.Body)
	}
	back := doAuth(t, standby.h, "GET", "/v1/secret/data/app", "", root)
	if back.Code != http.StatusOK || !contains(back.Body.String(), "via-standby") {
		t.Fatalf("read via standby = %d %s", back.Code, back.Body)
	}
	// Errors are relayed too, not turned into forwarding failures.
	if rec := doAuth(t, standby.h, "GET", "/v1/secret/data/app", "", "uv.bogus"); rec.Code != http.StatusForbidden {
		t.Fatalf("bad token via standby = %d, want the active's 403", rec.Code)
	}

	// The active replica audited the forwarded request under the client's own
	// address (httptest's 192.0.2.1), not the standby's.
	var sawClient bool
	for _, l := range auditLines(t, active.auditLog) {
		if l["path"] == "secret/data/app" && l["remote_addr"] == "192.0.2.1:1234" {
			sawClient = true
		}
	}
	if !sawClient {
		t.Fatalf("forwarded request not audited with the client address: %v", auditLines(t, active.auditLog))
	}
}

func TestHALeaderEndpoint(t *testing.T) {
	active, standby, _ := forwardingPair(t)
	a := decode[leaderResponse](t, do(t, active.h, "GET", "/v1/sys/leader", ""))
	s := decode[leaderResponse](t, do(t, standby.h, "GET", "/v1/sys/leader", ""))
	if !a.HAEnabled || !a.IsSelf || a.ActiveTime == "" {
		t.Fatalf("active sys/leader = %+v", a)
	}
	if !s.HAEnabled || s.IsSelf || s.LeaderAddress != a.LeaderAddress || s.LeaderClusterAddress == "" {
		t.Fatalf("standby sys/leader = %+v (active reports %+v)", s, a)
	}
	solo := decode[leaderResponse](t, do(t, NewHandler(core.New(storage.NewMemoryBackend())), "GET", "/v1/sys/leader", ""))
	if solo.HAEnabled {
		t.Fatal("non-HA vault reports ha_enabled")
	}
}

func TestHAStepDownViaStandby(t *testing.T) {
	active, standby, root := forwardingPair(t)
	// Sent to the standby, forwarded to the active, which steps down.
	if rec := doAuth(t, standby.h, "PUT", "/v1/sys/step-down", "", root); rec.Code != http.StatusNoContent {
		t.Fatalf("step-down via standby = %d %s", rec.Code, rec.Body)
	}
	waitFor(t, "the former standby to become active", standby.core.Active)
	if active.core.Active() {
		t.Fatal("stepped-down replica still active")
	}
	if rec := doAuth(t, standby.h, "PUT", "/v1/sys/step-down", "", "uv.bogus"); rec.Code != http.StatusForbidden {
		t.Fatalf("step-down with a bad token = %d, want 403", rec.Code)
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
