package identity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/cwolsen7905/ubixvault/internal/storage"
)

// ErrGroupNotFound is returned when a group ID or name has no record.
var ErrGroupNotFound = errors.New("identity: group not found")

// Group types.
const (
	GroupInternal = "internal"
	GroupExternal = "external"
)

// Group collects entities (and, nested, other groups) with its own policies. An
// entity that is a transitive member of a group picks up the group's policies,
// on top of its own.
//
// An internal group's direct members are listed in MemberEntityIDs. An external
// group's direct membership is instead asserted by an auth method: an entity is
// a member if it has an alias on MountType whose login-asserted groups included
// GroupName (e.g. an OIDC groups claim). Either kind can be nested inside
// another via MemberGroupIDs.
type Group struct {
	ID              string            `json:"id"`
	Name            string            `json:"name"`
	Type            string            `json:"type"` // "internal" (default) or "external"
	Policies        []string          `json:"policies,omitempty"`
	MemberEntityIDs []string          `json:"member_entity_ids,omitempty"` // internal groups only
	MemberGroupIDs  []string          `json:"member_group_ids,omitempty"`  // child groups; their members inherit this group's policies
	MountType       string            `json:"mount_type,omitempty"`        // external groups: the auth method that asserts membership
	GroupName       string            `json:"group_name,omitempty"`        // external groups: the asserted group name to match
	Metadata        map[string]string `json:"metadata,omitempty"`
	CreatedTime     time.Time         `json:"created_time"`
}

func (e *Engine) groupKey(id string) string    { return e.prefix + "/group/" + id }
func (e *Engine) groupNameKey(n string) string { return e.prefix + "/group-name/" + n }

// GroupInput carries the mutable fields of a group for a write.
type GroupInput struct {
	Type            string
	Policies        []string
	MemberEntityIDs []string
	MemberGroupIDs  []string
	MountType       string // external groups only
	GroupName       string // external groups only
	Metadata        map[string]string
}

// WriteGroup creates or updates the group named name (upsert by name),
// replacing its mutable fields.
func (e *Engine) WriteGroup(ctx context.Context, name string, in GroupInput) (*Group, error) {
	if !validName(name) {
		return nil, ErrInvalidName
	}
	existing, err := e.ReadGroupByName(ctx, name)
	switch {
	case errors.Is(err, ErrGroupNotFound):
		id, err := genID()
		if err != nil {
			return nil, err
		}
		return e.saveGroup(ctx, &Group{ID: id, Name: name, CreatedTime: e.now()}, in)
	case err != nil:
		return nil, err
	default:
		return e.saveGroup(ctx, existing, in)
	}
}

// UpdateGroup replaces the mutable fields of the group addressed by ID, leaving
// its name unchanged. Returns [ErrGroupNotFound] if the ID is unknown.
func (e *Engine) UpdateGroup(ctx context.Context, id string, in GroupInput) (*Group, error) {
	g, err := e.ReadGroup(ctx, id)
	if err != nil {
		return nil, err
	}
	return e.saveGroup(ctx, g, in)
}

func (e *Engine) saveGroup(ctx context.Context, g *Group, in GroupInput) (*Group, error) {
	g.Type = in.Type
	if g.Type == "" {
		g.Type = GroupInternal
	}
	if g.Type != GroupInternal && g.Type != GroupExternal {
		return nil, fmt.Errorf("%w: group type %q", ErrInvalidName, g.Type)
	}
	if g.Type == GroupExternal && (in.MountType == "" || in.GroupName == "") {
		return nil, fmt.Errorf("%w: external group needs mount_type and group_name", ErrInvalidName)
	}
	g.Policies = in.Policies
	g.MemberEntityIDs = in.MemberEntityIDs
	g.MemberGroupIDs = in.MemberGroupIDs
	g.MountType = in.MountType
	g.GroupName = in.GroupName
	g.Metadata = in.Metadata
	blob, err := json.Marshal(g)
	if err != nil {
		return nil, fmt.Errorf("identity: marshal group: %w", err)
	}
	if err := e.store.Put(ctx, &storage.Entry{Key: e.groupKey(g.ID), Value: blob}); err != nil {
		return nil, fmt.Errorf("identity: persist group: %w", err)
	}
	if err := e.store.Put(ctx, &storage.Entry{Key: e.groupNameKey(g.Name), Value: []byte(g.ID)}); err != nil {
		return nil, fmt.Errorf("identity: persist group name index: %w", err)
	}
	return g, nil
}

// ReadGroup returns the group with the given ID, or [ErrGroupNotFound].
func (e *Engine) ReadGroup(ctx context.Context, id string) (*Group, error) {
	entry, err := e.store.Get(ctx, e.groupKey(id))
	if err != nil {
		return nil, fmt.Errorf("identity: read group: %w", err)
	}
	if entry == nil {
		return nil, ErrGroupNotFound
	}
	var g Group
	if err := json.Unmarshal(entry.Value, &g); err != nil {
		return nil, fmt.Errorf("identity: unmarshal group: %w", err)
	}
	return &g, nil
}

// ReadGroupByName returns the group with the given name, or [ErrGroupNotFound].
func (e *Engine) ReadGroupByName(ctx context.Context, name string) (*Group, error) {
	entry, err := e.store.Get(ctx, e.groupNameKey(name))
	if err != nil {
		return nil, fmt.Errorf("identity: read group name index: %w", err)
	}
	if entry == nil {
		return nil, ErrGroupNotFound
	}
	return e.ReadGroup(ctx, string(entry.Value))
}

// ListGroups returns all group IDs.
func (e *Engine) ListGroups(ctx context.Context) ([]string, error) {
	ids, err := e.store.List(ctx, e.prefix+"/group/")
	if err != nil {
		return nil, fmt.Errorf("identity: list groups: %w", err)
	}
	return ids, nil
}

// DeleteGroup removes a group and its name index. It is a no-op if absent.
// References to the deleted group in other groups' MemberGroupIDs simply stop
// resolving; they are harmless.
func (e *Engine) DeleteGroup(ctx context.Context, id string) error {
	g, err := e.ReadGroup(ctx, id)
	if errors.Is(err, ErrGroupNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := e.store.Delete(ctx, e.groupNameKey(g.Name)); err != nil {
		return fmt.Errorf("identity: delete group name index: %w", err)
	}
	if err := e.store.Delete(ctx, e.groupKey(id)); err != nil {
		return fmt.Errorf("identity: delete group: %w", err)
	}
	return nil
}

// groupPoliciesForEntity returns the policies of every group the entity is a
// transitive member of: groups that list it directly, plus every ancestor group
// that lists one of those groups as a child (to any depth). It loads all groups
// and computes the closure by fixpoint, which tolerates cycles in
// MemberGroupIDs.
func (e *Engine) groupPoliciesForEntity(ctx context.Context, entityID string) ([]string, error) {
	ids, err := e.ListGroups(ctx)
	if err != nil {
		return nil, err
	}
	groups := make([]*Group, 0, len(ids))
	for _, id := range ids {
		g, err := e.ReadGroup(ctx, id)
		if errors.Is(err, ErrGroupNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		groups = append(groups, g)
	}

	// The (mountType, assertedGroup) pairs this entity's logins carry, for
	// matching external groups.
	asserted, err := e.assertedGroups(ctx, entityID)
	if err != nil {
		return nil, err
	}

	// Seed: groups the entity is a direct member of — internal groups that list
	// it, and external groups whose (mount, name) an alias asserted.
	applicable := make(map[string]bool)
	for _, g := range groups {
		switch g.Type {
		case GroupExternal:
			if asserted[g.MountType+"\x00"+g.GroupName] {
				applicable[g.ID] = true
			}
		default: // internal
			if contains(g.MemberEntityIDs, entityID) {
				applicable[g.ID] = true
			}
		}
	}
	// Fixpoint: a group applies if it lists an already-applicable group as a child.
	for changed := true; changed; {
		changed = false
		for _, g := range groups {
			if applicable[g.ID] {
				continue
			}
			for _, child := range g.MemberGroupIDs {
				if applicable[child] {
					applicable[g.ID] = true
					changed = true
					break
				}
			}
		}
	}

	var policies []string
	for _, g := range groups {
		if applicable[g.ID] {
			policies = append(policies, g.Policies...)
		}
	}
	return policies, nil
}

// assertedGroups returns the set of "<mountType>\x00<groupName>" keys the
// entity's aliases carry from their most recent logins, used to match external
// groups.
func (e *Engine) assertedGroups(ctx context.Context, entityID string) (map[string]bool, error) {
	ids, err := e.store.List(ctx, e.prefix+"/alias/")
	if err != nil {
		return nil, fmt.Errorf("identity: list aliases: %w", err)
	}
	out := make(map[string]bool)
	for _, id := range ids {
		a, err := e.ReadAlias(ctx, id)
		if errors.Is(err, ErrAliasNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if a.EntityID != entityID {
			continue
		}
		for _, gname := range a.Groups {
			out[a.MountType+"\x00"+gname] = true
		}
	}
	return out, nil
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
