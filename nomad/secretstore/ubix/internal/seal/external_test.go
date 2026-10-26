package seal

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestSealHelperProcess is not a real test: when GO_WANT_SEAL_HELPER=1 it acts as
// the external seal command, so the tests below can exec this very binary as a
// stub KMS. It reads stdin, applies a symmetric byte transform (its own inverse,
// so the same transform serves both wrap and unwrap), writes stdout, and exits.
func TestSealHelperProcess(_ *testing.T) {
	if os.Getenv("GO_WANT_SEAL_HELPER") != "1" {
		return
	}
	if d := os.Getenv("SEAL_HELPER_SLEEP"); d != "" {
		dur, _ := time.ParseDuration(d)
		time.Sleep(dur)
	}
	if os.Getenv("SEAL_HELPER_FAIL") == "1" {
		fmt.Fprintln(os.Stderr, "stub KMS failure")
		os.Exit(1)
	}
	data, _ := io.ReadAll(os.Stdin)
	for i := range data {
		data[i] ^= 0x5A
	}
	_, _ = os.Stdout.Write(data)
	os.Exit(0)
}

// helperSeal returns an External seal that execs this test binary as the stub KMS.
func helperSeal(t *testing.T, extraEnv []string, timeout time.Duration) *External {
	t.Helper()
	env := append([]string{"GO_WANT_SEAL_HELPER=1"}, extraEnv...)
	return NewExternal(os.Args[0], []string{"-test.run=TestSealHelperProcess", "--"}, env, timeout)
}

func TestExternalSealRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := helperSeal(t, nil, time.Minute)
	if s.Type() != "external" {
		t.Fatalf("Type() = %q, want external", s.Type())
	}

	master := []byte("0123456789abcdef0123456789abcdef") // 32-byte master key
	wrapped, err := s.Wrap(ctx, master)
	if err != nil {
		t.Fatalf("Wrap: %v", err)
	}
	if bytes.Equal(wrapped, master) {
		t.Fatal("wrapped blob equals the plaintext master key")
	}
	got, err := s.Unwrap(ctx, wrapped)
	if err != nil {
		t.Fatalf("Unwrap: %v", err)
	}
	if !bytes.Equal(got, master) {
		t.Fatalf("round-trip = %q, want %q", got, master)
	}
}

func TestExternalSealCommandFailure(t *testing.T) {
	s := helperSeal(t, []string{"SEAL_HELPER_FAIL=1"}, time.Minute)
	if _, err := s.Wrap(context.Background(), []byte("x")); err == nil {
		t.Fatal("Wrap should fail when the command exits non-zero")
	}
}

func TestExternalSealMissingCommand(t *testing.T) {
	s := NewExternal(filepath.Join(t.TempDir(), "does-not-exist"), nil, nil, time.Minute)
	if _, err := s.Wrap(context.Background(), []byte("x")); err == nil {
		t.Fatal("Wrap should fail when the command does not exist")
	}
}

func TestExternalSealTimeout(t *testing.T) {
	s := helperSeal(t, []string{"SEAL_HELPER_SLEEP=2s"}, 100*time.Millisecond)
	if _, err := s.Wrap(context.Background(), []byte("x")); err == nil {
		t.Fatal("Wrap should fail when the command exceeds the timeout")
	}
}
