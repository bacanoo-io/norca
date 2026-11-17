// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"context"
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/controller/secrets"
)

// The leader revokes by the lease ID when it never got the answer of the
// issue: the user created for that lease must go.
func TestRevoke_ByLeaseID(t *testing.T) {
	ctx := context.Background()
	b := newBackend()
	lease := "0123456789abcdef"
	if _, err := b.Issue(ctx, &secrets.IssueRequest{Role: "app", LeaseID: lease}); err != nil {
		t.Fatal(err)
	}
	other, err := b.Issue(ctx, &secrets.IssueRequest{Role: "app", LeaseID: "fedcba9876543210"})
	if err != nil {
		t.Fatal(err)
	}

	revoked := map[string]time.Time{lease: time.Now().Add(time.Hour), "unknown": time.Now().Add(time.Hour)}
	if err := b.Revoke(ctx, &secrets.RevokeRequest{Revoked: revoked}); err != nil {
		t.Fatal(err)
	}
	if n := len(b.byLease); n != 1 {
		t.Fatalf("%d credentials are left after the revoke by lease ID, expected 1", n)
	}
	if len(b.refs) != 1 || len(b.config) != 1 {
		t.Fatalf("leftovers: refs=%d config=%d", len(b.refs), len(b.config))
	}

	// Revoking by the reference still works, and repeating it is harmless.
	for range 2 {
		err := b.Revoke(ctx, &secrets.RevokeRequest{Revoked: map[string]time.Time{other.Ref: time.Now()}})
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(b.byLease) != 0 || len(b.refs) != 0 || len(b.config) != 0 {
		t.Fatalf("leftovers: byLease=%d refs=%d config=%d", len(b.byLease), len(b.refs), len(b.config))
	}
}

// The handler only requires a lease ID that is not empty.
func TestIssue_ShortLeaseID(t *testing.T) {
	b := newBackend()
	cred, err := b.Issue(context.Background(), &secrets.IssueRequest{Role: "app", LeaseID: "abc"})
	if err != nil {
		t.Fatal(err)
	}
	if cred.Ref != "v-app-abc" {
		t.Fatalf("ref %q", cred.Ref)
	}
}
