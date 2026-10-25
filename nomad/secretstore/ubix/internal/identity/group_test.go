package identity

import (
	"context"
	"errors"
	"sort"
	"testing"
)

func TestWriteReadGroup(t *testing.T) {
	ctx := context.Background()
	e := newEngine()
	g, err := e.WriteGroup(ctx, "platform", GroupInput{Policies: []string{"p-admin"}, MemberEntityIDs: []string{"e1", "e2"}, MemberGroupIDs: nil, Metadata: nil})
	if err != nil {
		t.Fatalf("WriteGroup: %v", err)
	}
	got, err := e.ReadGroup(ctx, g.ID)
	if err != nil || got.Name != "platform" || len(got.MemberEntityIDs) != 2 {
		t.Fatalf("ReadGroup = %+v, %v", got, err)
	}
	byName, err := e.ReadGroupByName(ctx, "platform")
	if err != nil || byName.ID != g.ID {
		t.Fatalf("ReadGroupByName = %+v, %v", byName, err)
	}
}

func TestWriteGroupUpsertsByName(t *testing.T) {
	ctx := context.Background()
	e := newEngine()
	first, _ := e.WriteGroup(ctx, "g", GroupInput{Policies: []string{"a"}, MemberEntityIDs: nil, MemberGroupIDs: nil, Metadata: nil})
	second, err := e.WriteGroup(ctx, "g", GroupInput{Policies: []string{"b"}, MemberEntityIDs: nil, MemberGroupIDs: nil, Metadata: nil})
	if err != nil {
		t.Fatalf("second WriteGroup: %v", err)
	}
	if second.ID != first.ID {
		t.Fatalf("upsert changed ID: %s -> %s", first.ID, second.ID)
	}
}

// TestGroupPolicyInPoliciesFor is the core Phase-2 behavior: an entity that is a
// member of a group picks up the group's policies through PoliciesFor.
func TestGroupPolicyInPoliciesFor(t *testing.T) {
	ctx := context.Background()
	e := newEngine()
	ent, _ := e.WriteEntity(ctx, "alice", []string{"own"}, nil, false)
	if _, err := e.WriteGroup(ctx, "platform", GroupInput{Policies: []string{"grp"}, MemberEntityIDs: []string{ent.ID}, MemberGroupIDs: nil, Metadata: nil}); err != nil {
		t.Fatalf("WriteGroup: %v", err)
	}

	got, err := e.PoliciesFor(ctx, ent.ID)
	if err != nil {
		t.Fatalf("PoliciesFor: %v", err)
	}
	sort.Strings(got)
	if len(got) != 2 || got[0] != "grp" || got[1] != "own" {
		t.Fatalf("PoliciesFor = %v, want [grp own]", got)
	}

	// A non-member entity does not get the group policy.
	other, _ := e.WriteEntity(ctx, "bob", []string{"own2"}, nil, false)
	og, _ := e.PoliciesFor(ctx, other.ID)
	if len(og) != 1 || og[0] != "own2" {
		t.Fatalf("non-member PoliciesFor = %v, want [own2]", og)
	}
}

// TestNestedGroupPolicies covers transitive membership: a member of a child
// group inherits the parent group's policies too, and a cycle does not hang.
func TestNestedGroupPolicies(t *testing.T) {
	ctx := context.Background()
	e := newEngine()
	ent, _ := e.WriteEntity(ctx, "carol", nil, nil, false)

	child, _ := e.WriteGroup(ctx, "child", GroupInput{Policies: []string{"child-pol"}, MemberEntityIDs: []string{ent.ID}, MemberGroupIDs: nil, Metadata: nil})
	parent, _ := e.WriteGroup(ctx, "parent", GroupInput{Policies: []string{"parent-pol"}, MemberEntityIDs: nil, MemberGroupIDs: []string{child.ID}, Metadata: nil})
	// Introduce a cycle: child also lists parent as a child.
	if _, err := e.WriteGroup(ctx, "child", GroupInput{Policies: []string{"child-pol"}, MemberEntityIDs: []string{ent.ID}, MemberGroupIDs: []string{parent.ID}, Metadata: nil}); err != nil {
		t.Fatalf("rewrite child: %v", err)
	}

	got, err := e.PoliciesFor(ctx, ent.ID)
	if err != nil {
		t.Fatalf("PoliciesFor: %v", err)
	}
	sort.Strings(got)
	if len(got) != 2 || got[0] != "child-pol" || got[1] != "parent-pol" {
		t.Fatalf("PoliciesFor = %v, want [child-pol parent-pol]", got)
	}
}

func TestDeleteGroup(t *testing.T) {
	ctx := context.Background()
	e := newEngine()
	ent, _ := e.WriteEntity(ctx, "dave", nil, nil, false)
	g, _ := e.WriteGroup(ctx, "temp", GroupInput{Policies: []string{"gp"}, MemberEntityIDs: []string{ent.ID}, MemberGroupIDs: nil, Metadata: nil})

	if err := e.DeleteGroup(ctx, g.ID); err != nil {
		t.Fatalf("DeleteGroup: %v", err)
	}
	if _, err := e.ReadGroup(ctx, g.ID); !errors.Is(err, ErrGroupNotFound) {
		t.Fatalf("group survived delete: %v", err)
	}
	// The entity no longer inherits the deleted group's policy.
	if got, _ := e.PoliciesFor(ctx, ent.ID); len(got) != 0 {
		t.Fatalf("PoliciesFor after group delete = %v, want empty", got)
	}
}

// TestExternalGroupMatchesAssertedGroups covers phase 3: an entity whose login
// asserted a group name is a member of an external group naming that (mount,
// group), and picks up its policies.
func TestExternalGroupMatchesAssertedGroups(t *testing.T) {
	ctx := context.Background()
	e := newEngine()

	// A jwt login for alice asserts membership in "platform" and "eng".
	entID, err := e.ResolveAlias(ctx, "jwt", "alice@idp", []string{"platform", "eng"})
	if err != nil {
		t.Fatalf("ResolveAlias: %v", err)
	}

	// An external group matching the "platform" jwt group, carrying a policy.
	if _, err := e.WriteGroup(ctx, "platform-team", GroupInput{
		Type: GroupExternal, MountType: "jwt", GroupName: "platform", Policies: []string{"platform-pol"},
	}); err != nil {
		t.Fatalf("WriteGroup external: %v", err)
	}
	// An external group for a group the login did NOT assert.
	if _, err := e.WriteGroup(ctx, "secops", GroupInput{
		Type: GroupExternal, MountType: "jwt", GroupName: "secops", Policies: []string{"secops-pol"},
	}); err != nil {
		t.Fatalf("WriteGroup external 2: %v", err)
	}

	got, err := e.PoliciesFor(ctx, entID)
	if err != nil {
		t.Fatalf("PoliciesFor: %v", err)
	}
	if len(got) != 1 || got[0] != "platform-pol" {
		t.Fatalf("PoliciesFor = %v, want [platform-pol]", got)
	}

	// A different mount asserting the same group name does not match a jwt group.
	if _, err := e.ResolveAlias(ctx, "userpass", "alice", []string{"platform"}); err != nil {
		t.Fatalf("ResolveAlias userpass: %v", err)
	}
	// (The userpass alias belongs to a different entity; the jwt entity's
	// membership is unchanged.)
	got2, _ := e.PoliciesFor(ctx, entID)
	if len(got2) != 1 || got2[0] != "platform-pol" {
		t.Fatalf("PoliciesFor after unrelated login = %v, want [platform-pol]", got2)
	}
}

// TestExternalGroupMembershipFollowsLatestLogin confirms the asserted groups are
// refreshed each login, so losing an IdP group drops the membership.
func TestExternalGroupMembershipFollowsLatestLogin(t *testing.T) {
	ctx := context.Background()
	e := newEngine()

	entID, _ := e.ResolveAlias(ctx, "jwt", "bob@idp", []string{"platform"})
	if _, err := e.WriteGroup(ctx, "platform-team", GroupInput{Type: GroupExternal, MountType: "jwt", GroupName: "platform", Policies: []string{"pol"}}); err != nil {
		t.Fatalf("WriteGroup: %v", err)
	}
	if got, _ := e.PoliciesFor(ctx, entID); len(got) != 1 {
		t.Fatalf("initial PoliciesFor = %v, want [pol]", got)
	}

	// Next login no longer asserts "platform".
	if _, err := e.ResolveAlias(ctx, "jwt", "bob@idp", []string{"eng"}); err != nil {
		t.Fatalf("re-login: %v", err)
	}
	if got, _ := e.PoliciesFor(ctx, entID); len(got) != 0 {
		t.Fatalf("PoliciesFor after dropping group = %v, want empty", got)
	}
}

func TestWriteExternalGroupValidation(t *testing.T) {
	ctx := context.Background()
	e := newEngine()
	// External group without mount_type/group_name is rejected.
	if _, err := e.WriteGroup(ctx, "bad", GroupInput{Type: GroupExternal}); !errors.Is(err, ErrInvalidName) {
		t.Fatalf("external group missing fields = %v, want ErrInvalidName", err)
	}
	// Unknown type is rejected.
	if _, err := e.WriteGroup(ctx, "bad2", GroupInput{Type: "weird"}); !errors.Is(err, ErrInvalidName) {
		t.Fatalf("unknown type = %v, want ErrInvalidName", err)
	}
	// Empty type defaults to internal.
	g, err := e.WriteGroup(ctx, "ok", GroupInput{})
	if err != nil || g.Type != GroupInternal {
		t.Fatalf("default type = %q, %v, want internal", g.Type, err)
	}
}

func TestReadGroupMissing(t *testing.T) {
	if _, err := newEngine().ReadGroup(context.Background(), "nope"); !errors.Is(err, ErrGroupNotFound) {
		t.Fatalf("ReadGroup missing = %v, want ErrGroupNotFound", err)
	}
}
