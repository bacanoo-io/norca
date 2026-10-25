package api

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cwolsen7905/ubixvault/internal/audit"
	"github.com/cwolsen7905/ubixvault/internal/core"
	"github.com/cwolsen7905/ubixvault/internal/storage"
)

// auditedHandler returns an unsealed handler whose requests are audited to a
// file, plus the root token and the log path.
func auditedHandler(t *testing.T) (*Handler, string, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "audit.log")
	device, err := audit.NewFileDevice(path)
	if err != nil {
		t.Fatalf("NewFileDevice: %v", err)
	}
	c := core.New(storage.NewMemoryBackend())
	h := NewHandler(c, WithAudit(audit.NewBroker(device)))

	init := decode[initResponse](t, do(t, h, "POST", "/v1/sys/init", `{"secret_shares":2,"secret_threshold":2}`))
	do(t, h, "POST", "/v1/sys/unseal", `{"key":"`+init.Keys[0]+`"}`)
	do(t, h, "POST", "/v1/sys/unseal", `{"key":"`+init.Keys[1]+`"}`)
	return h, init.RootToken, path
}

func auditLines(t *testing.T, path string) []map[string]any {
	t.Helper()
	f, err := os.Open(path) //nolint:gosec // G304: test path from t.TempDir()
	if err != nil {
		t.Fatalf("open audit log: %v", err)
	}
	defer func() { _ = f.Close() }()
	var out []map[string]any
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err == nil {
			out = append(out, m)
		}
	}
	return out
}

func TestAuditLogsRequestAndResponse(t *testing.T) {
	h, root, path := auditedHandler(t)

	doAuth(t, h, "POST", "/v1/secret/data/app", `{"data":{"k":"v"}}`, root)

	lines := auditLines(t, path)
	// Find the request/response pair for the secret write.
	var sawReq, sawResp bool
	for _, l := range lines {
		if l["path"] == "secret/data/app" && l["type"] == "request" && l["operation"] == "update" {
			sawReq = true
		}
		if l["path"] == "secret/data/app" && l["type"] == "response" {
			sawResp = true
			if l["status_code"].(float64) != 200 {
				t.Fatalf("response status = %v, want 200", l["status_code"])
			}
		}
	}
	if !sawReq || !sawResp {
		t.Fatalf("missing audit entries (req=%v resp=%v) in %v", sawReq, sawResp, lines)
	}
}

func TestAuditNeverLogsRawToken(t *testing.T) {
	h, root, path := auditedHandler(t)
	doAuth(t, h, "GET", "/v1/secret/data/app", "", root)

	raw, _ := os.ReadFile(path) //nolint:gosec // G304: test path from t.TempDir()
	if strings.Contains(string(raw), root) {
		t.Fatal("root token leaked into audit log")
	}
	// But a token_hmac should be present for the authenticated request.
	var sawHMAC bool
	for _, l := range auditLines(t, path) {
		if l["token_hmac"] != nil && l["token_hmac"] != "" {
			sawHMAC = true
		}
	}
	if !sawHMAC {
		t.Fatal("expected a token_hmac for the authenticated request")
	}
}

// brokenDevice makes the broker fail, to test fail-closed request handling.
type brokenDevice struct{}

func (brokenDevice) Log(context.Context, *audit.Entry) error { return errors.New("boom") }
func (brokenDevice) Close() error                            { return nil }

func TestAuditFailClosedRefusesRequest(t *testing.T) {
	c := core.New(storage.NewMemoryBackend())
	h := NewHandler(c, WithAudit(audit.NewBroker(brokenDevice{})))

	// Even the unauthenticated seal-status endpoint must be refused if it can't
	// be audited.
	rec := do(t, h, "GET", "/v1/sys/seal-status", "")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("audit failure status = %d, want 500 (fail closed)", rec.Code)
	}
}

func TestNoAuditByDefault(t *testing.T) {
	// A handler without WithAudit must still serve normally.
	c := core.New(storage.NewMemoryBackend())
	h := NewHandler(c)
	if rec := do(t, h, "GET", "/v1/sys/seal-status", ""); rec.Code != http.StatusOK {
		t.Fatalf("no-audit handler status = %d, want 200", rec.Code)
	}
}

// TestAuditTokenHMACStableAcrossRestartAndSeal: the audit HMAC key lives in the
// barrier, so the same token HMACs identically after a restart (or on another
// replica over the same storage) and after a seal/unseal. While sealed there is
// no key, and the token is omitted rather than HMAC'd under a throwaway key.
func TestAuditTokenHMACStableAcrossRestartAndSeal(t *testing.T) {
	ctx := context.Background()
	mem := storage.NewMemoryBackend()
	kek := make([]byte, 32)
	kek[0] = 1

	start := func() (*core.Core, *Handler, string) {
		path := filepath.Join(t.TempDir(), "audit.log")
		device, err := audit.NewFileDevice(path)
		if err != nil {
			t.Fatalf("NewFileDevice: %v", err)
		}
		c := core.New(mem, core.WithAutoUnsealKey(kek))
		return c, NewHandler(c, WithAudit(audit.NewBroker(device))), path
	}
	lastHMAC := func(path string) any {
		lines := auditLines(t, path)
		return lines[len(lines)-1]["token_hmac"]
	}

	_, h1, path1 := start()
	init := decode[initResponse](t, do(t, h1, "POST", "/v1/sys/init", `{}`))
	doAuth(t, h1, "GET", "/v1/sys/policies/acl/default", "", init.RootToken)
	first := lastHMAC(path1)
	if first == nil || first == "" {
		t.Fatal("no token_hmac on an unsealed request")
	}

	// Restart: a new process over the same storage.
	c2, h2, path2 := start()
	if err := c2.AutoUnseal(ctx); err != nil {
		t.Fatalf("AutoUnseal: %v", err)
	}
	doAuth(t, h2, "GET", "/v1/sys/policies/acl/default", "", init.RootToken)
	if got := lastHMAC(path2); got != first {
		t.Fatalf("token_hmac after restart = %v, want %v", got, first)
	}

	// Sealed: the token is omitted.
	doAuth(t, h2, "POST", "/v1/sys/seal", "", init.RootToken)
	doAuth(t, h2, "GET", "/v1/sys/policies/acl/default", "", init.RootToken)
	if got := lastHMAC(path2); got != nil {
		t.Fatalf("token_hmac while sealed = %v, want none", got)
	}

	// Unsealed again: same HMAC as before.
	if err := c2.AutoUnseal(ctx); err != nil {
		t.Fatalf("AutoUnseal: %v", err)
	}
	doAuth(t, h2, "GET", "/v1/sys/policies/acl/default", "", init.RootToken)
	if got := lastHMAC(path2); got != first {
		t.Fatalf("token_hmac after seal/unseal = %v, want %v", got, first)
	}
}
