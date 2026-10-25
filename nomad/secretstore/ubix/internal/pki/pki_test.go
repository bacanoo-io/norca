package pki

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"testing"
	"time"

	"github.com/cwolsen7905/ubixvault/internal/storage"
)

func newEngine(t *testing.T) *Engine {
	t.Helper()
	return New(storage.NewMemoryBackend(), "pki")
}

func parseCert(t *testing.T, certPEM string) *x509.Certificate {
	t.Helper()
	block, _ := pem.Decode([]byte(certPEM))
	if block == nil {
		t.Fatal("not a PEM certificate")
	}
	c, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse cert: %v", err)
	}
	return c
}

func TestGenerateRootAndIssueVerifies(t *testing.T) {
	ctx := context.Background()
	e := newEngine(t)

	caPEM, err := e.GenerateRoot(ctx, RootConfig{CommonName: "uBix Root"})
	if err != nil {
		t.Fatalf("GenerateRoot: %v", err)
	}
	if err := e.WriteRole(ctx, "web", Role{AllowedDomains: []string{"example.com"}, AllowSubdomains: true, MaxTTL: time.Hour}); err != nil {
		t.Fatalf("WriteRole: %v", err)
	}

	issued, err := e.Issue(ctx, "web", IssueRequest{CommonName: "app.example.com", TTL: 30 * time.Minute})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	// The leaf must chain to the CA and be valid for the requested name.
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM([]byte(caPEM)) {
		t.Fatal("failed to add CA to pool")
	}
	leaf := parseCert(t, issued.CertificatePEM)
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: roots, DNSName: "app.example.com"}); err != nil {
		t.Fatalf("leaf did not verify against the CA: %v", err)
	}
	// The private key and issuing CA are returned.
	if issued.PrivateKeyPEM == "" || issued.IssuingCAPEM == "" || issued.SerialNumber == "" {
		t.Fatalf("issued cert missing fields: %+v", issued)
	}
	// TTL is honored (30m requested, under the 1h cap).
	if d := time.Until(leaf.NotAfter); d > 40*time.Minute {
		t.Fatalf("leaf NotAfter too far out: %v", d)
	}
}

func TestRoleDeniesDisallowedName(t *testing.T) {
	ctx := context.Background()
	e := newEngine(t)
	_, _ = e.GenerateRoot(ctx, RootConfig{CommonName: "Root"})
	_ = e.WriteRole(ctx, "web", Role{AllowedDomains: []string{"example.com"}})

	if _, err := e.Issue(ctx, "web", IssueRequest{CommonName: "evil.com"}); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("want ErrNotAllowed, got %v", err)
	}
	// A subdomain is denied when AllowSubdomains is false.
	if _, err := e.Issue(ctx, "web", IssueRequest{CommonName: "sub.example.com"}); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("subdomain should be denied, got %v", err)
	}
}

func TestMaxTTLCap(t *testing.T) {
	ctx := context.Background()
	e := newEngine(t)
	_, _ = e.GenerateRoot(ctx, RootConfig{CommonName: "Root"})
	_ = e.WriteRole(ctx, "short", Role{AllowedDomains: []string{"x.test"}, MaxTTL: time.Hour})

	issued, err := e.Issue(ctx, "short", IssueRequest{CommonName: "x.test", TTL: 720 * time.Hour})
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if d := time.Until(parseCert(t, issued.CertificatePEM).NotAfter); d > 2*time.Hour {
		t.Fatalf("TTL not capped to MaxTTL: %v", d)
	}
}

func TestCALifecycleErrors(t *testing.T) {
	ctx := context.Background()
	e := newEngine(t)
	// Issue before a CA exists.
	_ = e.WriteRole(ctx, "web", Role{AllowedDomains: []string{"x.test"}})
	if _, err := e.Issue(ctx, "web", IssueRequest{CommonName: "x.test"}); !errors.Is(err, ErrNoCA) {
		t.Fatalf("want ErrNoCA, got %v", err)
	}
	if _, err := e.GenerateRoot(ctx, RootConfig{CommonName: "Root"}); err != nil {
		t.Fatal(err)
	}
	// A second root is refused.
	if _, err := e.GenerateRoot(ctx, RootConfig{CommonName: "Root2"}); !errors.Is(err, ErrCAExists) {
		t.Fatalf("want ErrCAExists, got %v", err)
	}
}
