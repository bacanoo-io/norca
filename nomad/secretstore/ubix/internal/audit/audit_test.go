package audit

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readLines(t *testing.T, path string) []map[string]any {
	t.Helper()
	f, err := os.Open(path) //nolint:gosec // G304: test path from t.TempDir()
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = f.Close() }()

	var out []map[string]any
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("bad JSON line %q: %v", sc.Text(), err)
		}
		out = append(out, m)
	}
	return out
}

func TestFileDeviceWritesEntries(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "audit.log")
	d, err := NewFileDevice(path)
	if err != nil {
		t.Fatalf("NewFileDevice: %v", err)
	}
	b := NewBroker(d)

	if err := b.LogRequest(ctx, &Entry{
		Operation: "read", Path: "secret/data/app", ClientToken: "uv.secret-token", RemoteAddr: "10.0.0.1:5555",
	}); err != nil {
		t.Fatalf("LogRequest: %v", err)
	}
	if err := b.LogResponse(ctx, &Entry{Operation: "read", Path: "secret/data/app", StatusCode: 200}); err != nil {
		t.Fatalf("LogResponse: %v", err)
	}

	lines := readLines(t, path)
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2", len(lines))
	}
	req := lines[0]
	if req["type"] != "request" || req["operation"] != "read" || req["path"] != "secret/data/app" {
		t.Fatalf("request entry = %v", req)
	}
	if req["status_code"] != nil {
		t.Fatalf("request should not have status_code: %v", req)
	}
	if lines[1]["type"] != "response" || lines[1]["status_code"].(float64) != 200 {
		t.Fatalf("response entry = %v", lines[1])
	}
}

// TestTokenNeverWrittenInClear is the core guarantee: the raw token must not
// appear anywhere in the log, only its HMAC.
func TestTokenNeverWrittenInClear(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "audit.log")
	d, _ := NewFileDevice(path)
	b := NewBroker(d)
	b.SetHMACKey([]byte("0123456789abcdef0123456789abcdef"))

	const token = "uv.super-secret-token-value"
	_ = b.LogRequest(ctx, &Entry{Operation: "read", Path: "p", ClientToken: token})

	raw, _ := os.ReadFile(path) //nolint:gosec // G304: test path from t.TempDir()
	if strings.Contains(string(raw), token) {
		t.Fatal("raw token leaked into the audit log")
	}
	line := readLines(t, path)[0]
	if line["token_hmac"] == "" || line["token_hmac"] == nil {
		t.Fatalf("expected a token_hmac, got %v", line)
	}
}

// TestTokenOmittedWithoutKey: before the vault supplies its HMAC key (i.e. while
// sealed) a token is left out entirely — never written raw, never HMAC'd under
// a throwaway key that could not be correlated later.
func TestTokenOmittedWithoutKey(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "audit.log")
	d, _ := NewFileDevice(path)
	b := NewBroker(d)

	const token = "uv.super-secret-token-value"
	_ = b.LogRequest(ctx, &Entry{Operation: "read", Path: "p", ClientToken: token})
	b.SetHMACKey([]byte("k"))
	b.SetHMACKey(nil) // cleared again, as on seal
	_ = b.LogRequest(ctx, &Entry{Operation: "read", Path: "p", ClientToken: token})

	raw, _ := os.ReadFile(path) //nolint:gosec // G304: test path from t.TempDir()
	if strings.Contains(string(raw), token) {
		t.Fatal("raw token leaked into the audit log")
	}
	for _, line := range readLines(t, path) {
		if line["token_hmac"] != nil {
			t.Fatalf("token_hmac written without a key: %v", line)
		}
	}
}

// TestSameKeySameHMAC: two devices given the same key — two replicas, or one
// process before and after a restart — HMAC a token identically.
func TestSameKeySameHMAC(t *testing.T) {
	ctx := context.Background()
	key := []byte("0123456789abcdef0123456789abcdef")
	var hmacs []any
	for range 2 {
		path := filepath.Join(t.TempDir(), "audit.log")
		d, _ := NewFileDevice(path)
		b := NewBroker(d)
		b.SetHMACKey(key)
		_ = b.LogRequest(ctx, &Entry{Operation: "read", Path: "p", ClientToken: "uv.t"})
		hmacs = append(hmacs, readLines(t, path)[0]["token_hmac"])
	}
	if hmacs[0] == nil || hmacs[0] != hmacs[1] {
		t.Fatalf("token_hmac differs across devices with the same key: %v", hmacs)
	}
}

func TestTimestampAndTypeSet(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "audit.log")
	d, _ := NewFileDevice(path)
	_ = NewBroker(d).LogRequest(ctx, &Entry{Operation: "list", Path: "p"})

	line := readLines(t, path)[0]
	if line["time"] == "" || line["time"] == nil {
		t.Fatal("entry missing time")
	}
	if line["type"] != "request" {
		t.Fatalf("type = %v, want request", line["type"])
	}
}

// failingDevice always errors, to test fail-closed behavior.
type failingDevice struct{}

func (failingDevice) Log(context.Context, *Entry) error { return errors.New("disk full") }
func (failingDevice) Close() error                      { return nil }

func TestBrokerFailsClosed(t *testing.T) {
	b := NewBroker(failingDevice{})
	if err := b.LogRequest(context.Background(), &Entry{Path: "p"}); err == nil {
		t.Fatal("broker did not surface device failure (must fail closed)")
	}
}

func TestBrokerFansOutToAllDevices(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	d1, _ := NewFileDevice(filepath.Join(dir, "a.log"))
	d2, _ := NewFileDevice(filepath.Join(dir, "b.log"))
	b := NewBroker(d1, d2)

	if err := b.LogRequest(ctx, &Entry{Operation: "read", Path: "p"}); err != nil {
		t.Fatalf("LogRequest: %v", err)
	}
	if len(readLines(t, filepath.Join(dir, "a.log"))) != 1 || len(readLines(t, filepath.Join(dir, "b.log"))) != 1 {
		t.Fatal("entry not written to both devices")
	}
}
