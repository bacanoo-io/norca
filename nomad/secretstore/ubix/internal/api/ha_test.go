package api

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/cwolsen7905/ubixvault/internal/core"
	"github.com/cwolsen7905/ubixvault/internal/storage"
	"github.com/cwolsen7905/ubixvault/internal/storage/storagetest"
)

// haPair starts two auto-unseal replicas over one in-memory database, both
// unsealed with their election loops running, and returns (active, standby)
// handlers plus the root token.
func haPair(t *testing.T) (*Handler, *Handler, string) {
	t.Helper()
	group := storagetest.NewHAGroup(storage.NewMemoryBackend())
	kek := make([]byte, 32)
	kek[0] = 7
	var cores []*core.Core
	var handlers []*Handler
	for _, id := range []string{"r0", "r1"} {
		phys := group.Replica()
		c := core.New(phys, core.WithAutoUnsealKey(kek),
			core.WithHA(core.HAConfig{Backend: phys, HolderID: id, Advertise: "https://" + id}))
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { defer close(done); _ = c.RunHA(ctx, func(string, ...any) {}) }()
		t.Cleanup(func() { cancel(); <-done })
		cores = append(cores, c)
		handlers = append(handlers, NewHandler(c))
	}
	init := decode[initResponse](t, do(t, handlers[0], "POST", "/v1/sys/init", `{}`))
	if err := cores[1].AutoUnseal(context.Background()); err != nil {
		t.Fatalf("AutoUnseal: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for cores[0].Active() == cores[1].Active() {
		if time.Now().After(deadline) {
			t.Fatal("no single active replica")
		}
		time.Sleep(2 * time.Millisecond)
	}
	if cores[0].Active() {
		return handlers[0], handlers[1], init.RootToken
	}
	return handlers[1], handlers[0], init.RootToken
}

func TestHAStandbyRefusesRequests(t *testing.T) {
	active, standby, root := haPair(t)

	if rec := doAuth(t, active, "POST", "/v1/secret/data/app", `{"data":{"k":"v"}}`, root); rec.Code != http.StatusOK {
		t.Fatalf("write on active = %d %s", rec.Code, rec.Body)
	}
	for _, path := range []string{"/v1/secret/data/app", "/v1/sys/policies/acl"} {
		if rec := doAuth(t, standby, "GET", path, "", root); rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("GET %s on standby = %d, want 503", path, rec.Code)
		}
	}
	// Per-replica lifecycle endpoints still answer on a standby.
	if rec := do(t, standby, "GET", "/v1/sys/seal-status", ""); rec.Code != http.StatusOK {
		t.Fatalf("seal-status on standby = %d, want 200", rec.Code)
	}
	if rec := do(t, standby, "GET", "/v1/sys/livez", ""); rec.Code != http.StatusOK {
		t.Fatalf("livez on standby = %d, want 200", rec.Code)
	}
}

func TestHAHealthCodes(t *testing.T) {
	active, standby, _ := haPair(t)
	cases := []struct {
		name string
		h    *Handler
		url  string
		want int
	}{
		{"active", active, "/v1/sys/health", 200},
		{"standby default", standby, "/v1/sys/health", 429},
		{"standbyok", standby, "/v1/sys/health?standbyok=true", 200},
		{"standbycode", standby, "/v1/sys/health?standbycode=473", 473},
		{"activecode", active, "/v1/sys/health?activecode=204", 204},
		{"perfstandbyok ignored", standby, "/v1/sys/health?perfstandbyok=true", 429},
		{"invalid code", active, "/v1/sys/health?standbycode=abc", 400},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if rec := do(t, tc.h, "GET", tc.url, ""); rec.Code != tc.want {
				t.Fatalf("%s = %d, want %d (%s)", tc.url, rec.Code, tc.want, rec.Body)
			}
		})
	}
	body := decode[healthResponse](t, do(t, standby, "GET", "/v1/sys/health?standbyok=true", ""))
	if !body.Standby || body.Sealed {
		t.Fatalf("standby health body = %+v, want standby=true sealed=false", body)
	}
}

func TestHealthCodeOverridesWithoutHA(t *testing.T) {
	h := NewHandler(core.New(storage.NewMemoryBackend()))
	if rec := do(t, h, "GET", "/v1/sys/health?uninitcode=200", ""); rec.Code != 200 {
		t.Fatalf("uninitcode override = %d, want 200", rec.Code)
	}
	body := decode[healthResponse](t, do(t, h, "GET", "/v1/sys/health", ""))
	if body.Standby {
		t.Fatal("non-HA vault reports standby")
	}
}
