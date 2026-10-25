package identity

import (
	"context"
	"errors"
	"testing"

	"github.com/cwolsen7905/ubixvault/internal/storage"
)

func newEngine() *Engine {
	return New(storage.NewMemoryBackend(), "identity")
}

func TestWriteReadEntity(t *testing.T) {
	ctx := context.Background()
	e := newEngine()

	ent, err := e.WriteEntity(ctx, "alice", []string{"team-platform"}, map[string]string{"email": "a@x"}, false)
	if err != nil {
		t.Fatalf("WriteEntity: %v", err)
	}
	if ent.ID == "" {
		t.Fatal("entity got no ID")
	}
	byID, err := e.ReadEntity(ctx, ent.ID)
	if err != nil || byID.Name != "alice" {
		t.Fatalf("ReadEntity = %+v, %v", byID, err)
	}
	byName, err := e.ReadEntityByName(ctx, "alice")
	if err != nil || byName.ID != ent.ID {
		t.Fatalf("ReadEntityByName = %+v, %v", byName, err)
	}
}

func TestWriteEntityUpsertsByName(t *testing.T) {
	ctx := context.Background()
	e := newEngine()

	first, _ := e.WriteEntity(ctx, "bob", []string{"p1"}, nil, false)
	second, err := e.WriteEntity(ctx, "bob", []string{"p2"}, nil, false)
	if err != nil {
		t.Fatalf("second WriteEntity: %v", err)
	}
	if second.ID != first.ID {
		t.Fatalf("upsert changed the ID: %s -> %s", first.ID, second.ID)
	}
	got, _ := e.ReadEntity(ctx, first.ID)
	if len(got.Policies) != 1 || got.Policies[0] != "p2" {
		t.Fatalf("policies not replaced: %v", got.Policies)
	}
}

func TestWriteEntityRejectsBadName(t *testing.T) {
	for _, n := range []string{"", "has/slash"} {
		if _, err := newEngine().WriteEntity(context.Background(), n, nil, nil, false); !errors.Is(err, ErrInvalidName) {
			t.Fatalf("WriteEntity(%q) = %v, want ErrInvalidName", n, err)
		}
	}
}

// TestResolveAliasAutoCreate is the core Phase-1 behavior: a first login through
// a method materializes an entity + alias, and a second login through a
// different method for the *same* entity (via an explicit alias) resolves to it.
func TestResolveAliasAutoCreate(t *testing.T) {
	ctx := context.Background()
	e := newEngine()

	// First userpass login for "alice" auto-creates.
	id1, err := e.ResolveAlias(ctx, "userpass", "alice", nil)
	if err != nil || id1 == "" {
		t.Fatalf("first ResolveAlias = %q, %v", id1, err)
	}
	// Second login resolves to the same entity (no duplicate).
	id2, err := e.ResolveAlias(ctx, "userpass", "alice", nil)
	if err != nil || id2 != id1 {
		t.Fatalf("second ResolveAlias = %q, %v (want %q)", id2, err, id1)
	}
	ids, _ := e.ListEntities(ctx)
	if len(ids) != 1 {
		t.Fatalf("entities = %d, want 1 (no duplicate on repeat login)", len(ids))
	}

	// A JWT login for the same human, wired to the same entity via an explicit
	// alias, resolves to that one entity — "multiple logins, one subject".
	if _, err := e.CreateAlias(ctx, id1, "jwt", "alice@idp"); err != nil {
		t.Fatalf("CreateAlias: %v", err)
	}
	viaJWT, err := e.ResolveAlias(ctx, "jwt", "alice@idp", nil)
	if err != nil || viaJWT != id1 {
		t.Fatalf("ResolveAlias via jwt = %q, %v (want %q)", viaJWT, err, id1)
	}
}

func TestResolveAliasAutoCreateOff(t *testing.T) {
	ctx := context.Background()
	e := newEngine()
	e.SetAutoCreate(false)

	id, err := e.ResolveAlias(ctx, "userpass", "nobody", nil)
	if err != nil {
		t.Fatalf("ResolveAlias: %v", err)
	}
	if id != "" {
		t.Fatalf("auto-create off should yield no entity, got %q", id)
	}
	if ids, _ := e.ListEntities(ctx); len(ids) != 0 {
		t.Fatalf("entities = %d, want 0", len(ids))
	}
}

func TestPoliciesFor(t *testing.T) {
	ctx := context.Background()
	e := newEngine()
	ent, _ := e.WriteEntity(ctx, "dave", []string{"a", "b"}, nil, false)

	got, err := e.PoliciesFor(ctx, ent.ID)
	if err != nil || len(got) != 2 {
		t.Fatalf("PoliciesFor = %v, %v", got, err)
	}
	// Empty entity ID and unknown ID contribute nothing (not an error).
	if got, _ := e.PoliciesFor(ctx, ""); got != nil {
		t.Fatalf("PoliciesFor(\"\") = %v, want nil", got)
	}
	if got, _ := e.PoliciesFor(ctx, "deadbeef"); got != nil {
		t.Fatalf("PoliciesFor(unknown) = %v, want nil", got)
	}
	// A disabled entity contributes nothing.
	dis, _ := e.WriteEntity(ctx, "eve", []string{"x"}, nil, true)
	if got, _ := e.PoliciesFor(ctx, dis.ID); got != nil {
		t.Fatalf("PoliciesFor(disabled) = %v, want nil", got)
	}
}

func TestCreateAliasRequiresEntity(t *testing.T) {
	if _, err := newEngine().CreateAlias(context.Background(), "nope", "userpass", "x"); !errors.Is(err, ErrEntityNotFound) {
		t.Fatalf("CreateAlias for missing entity = %v, want ErrEntityNotFound", err)
	}
}

func TestDeleteEntityRemovesAliases(t *testing.T) {
	ctx := context.Background()
	e := newEngine()
	id, _ := e.ResolveAlias(ctx, "userpass", "frank", nil) // auto-creates entity + alias

	if err := e.DeleteEntity(ctx, id); err != nil {
		t.Fatalf("DeleteEntity: %v", err)
	}
	if _, err := e.ReadEntity(ctx, id); !errors.Is(err, ErrEntityNotFound) {
		t.Fatalf("entity survived delete: %v", err)
	}
	// The alias index is gone, so a later login auto-creates a fresh entity.
	id2, _ := e.ResolveAlias(ctx, "userpass", "frank", nil)
	if id2 == id {
		t.Fatal("alias not cleared: resolved to the deleted entity's ID")
	}
}
