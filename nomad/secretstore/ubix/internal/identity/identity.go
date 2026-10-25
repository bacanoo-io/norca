// Package identity implements the identity layer's first phase: entities and
// aliases (docs/design/identity-entities-groups.md, ADR D-016).
//
// An entity is the canonical subject; an alias maps one auth-method login
// (mount type + login name) to an entity. At login an auth method resolves its
// (mountType, name) to an entity — auto-creating one on first sight unless
// disabled — and the resulting token carries the entity's ID. At request time
// the authorizer unions the entity's policies with the token's own, so policy
// attached to a subject applies across every method that subject logs in
// through, and takes effect without re-issuing tokens.
//
// Groups (internal and external) and identity templating are later phases; this
// package deliberately implements only entities and aliases. Everything is a
// JSON record on the barrier — no new dependency.
package identity

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/cwolsen7905/ubixvault/internal/storage"
)

// Errors.
var (
	ErrEntityNotFound = errors.New("identity: entity not found")
	ErrAliasNotFound  = errors.New("identity: alias not found")
	ErrInvalidName    = errors.New("identity: invalid name")
	ErrNameTaken      = errors.New("identity: entity name already in use")
)

// Entity is the canonical subject. Its policies are added to those of any token
// bound to it.
type Entity struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Policies    []string          `json:"policies,omitempty"`
	Metadata    map[string]string `json:"metadata,omitempty"`
	Disabled    bool              `json:"disabled,omitempty"`
	CreatedTime time.Time         `json:"created_time"`
}

// Alias maps an auth-method login to an entity.
type Alias struct {
	ID          string            `json:"id"`
	EntityID    string            `json:"entity_id"`
	MountType   string            `json:"mount_type"`
	Name        string            `json:"name"`
	Groups      []string          `json:"groups,omitempty"` // group memberships the auth method asserted at the last login
	Metadata    map[string]string `json:"metadata,omitempty"`
	CreatedTime time.Time         `json:"created_time"`
}

// Storage is the subset of a backend the engine needs.
type Storage interface {
	Get(ctx context.Context, key string) (*storage.Entry, error)
	Put(ctx context.Context, entry *storage.Entry) error
	Delete(ctx context.Context, key string) error
	List(ctx context.Context, prefix string) ([]string, error)
}

// Engine stores and resolves entities and aliases.
type Engine struct {
	store      Storage
	prefix     string
	now        func() time.Time
	autoCreate bool

	mu sync.Mutex // serializes alias auto-creation so a first login makes one entity
}

// New returns an engine storing under prefix (e.g. "identity"). Alias
// auto-creation is on by default, matching Vault.
func New(store Storage, prefix string) *Engine {
	return &Engine{
		store:      store,
		prefix:     strings.Trim(prefix, "/"),
		now:        func() time.Time { return time.Now().UTC() },
		autoCreate: true,
	}
}

// SetAutoCreate toggles whether an unknown (mountType, name) login materializes
// an entity and alias. With it off, such a login still succeeds but its token
// gets no entity (no identity policies).
func (e *Engine) SetAutoCreate(on bool) { e.autoCreate = on }

func (e *Engine) entityKey(id string) string    { return e.prefix + "/entity/" + id }
func (e *Engine) entityNameKey(n string) string { return e.prefix + "/entity-name/" + n }
func (e *Engine) aliasKey(id string) string     { return e.prefix + "/alias/" + id }
func (e *Engine) aliasIndexKey(mount, n string) string {
	return e.prefix + "/alias-index/" + mount + "/" + n
}

func validName(name string) bool { return name != "" && !strings.Contains(name, "/") }

// sameStrings reports whether a and b hold the same elements in the same order.
func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func genID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("identity: generate id: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// --- Entities ---

// WriteEntity creates or updates the entity named name (upsert by name),
// replacing its policies, metadata, and disabled flag. It returns the stored
// entity (with its stable ID).
func (e *Engine) WriteEntity(ctx context.Context, name string, policies []string, metadata map[string]string, disabled bool) (*Entity, error) {
	if !validName(name) {
		return nil, ErrInvalidName
	}
	existing, err := e.ReadEntityByName(ctx, name)
	switch {
	case errors.Is(err, ErrEntityNotFound):
		// new entity
	case err != nil:
		return nil, err
	default:
		existing.Policies = policies
		existing.Metadata = metadata
		existing.Disabled = disabled
		if err := e.putEntity(ctx, existing); err != nil {
			return nil, err
		}
		return existing, nil
	}

	return e.createEntity(ctx, name, policies, metadata, disabled)
}

// UpdateEntity replaces the policies, metadata, and disabled flag of an existing
// entity addressed by ID, leaving its name unchanged. This is how an
// auto-created entity (whose name contains "/") is edited, since WriteEntity
// addresses entities by their user-facing name. It returns [ErrEntityNotFound]
// if the ID is unknown.
func (e *Engine) UpdateEntity(ctx context.Context, id string, policies []string, metadata map[string]string, disabled bool) (*Entity, error) {
	ent, err := e.ReadEntity(ctx, id)
	if err != nil {
		return nil, err
	}
	ent.Policies = policies
	ent.Metadata = metadata
	ent.Disabled = disabled
	if err := e.putEntity(ctx, ent); err != nil {
		return nil, err
	}
	return ent, nil
}

// createEntity persists a brand-new entity with a generated ID. It does not
// enforce the user-facing name rule, so the auto-create path can give entities
// a descriptive "<mountType>/<name>" name that a hand-written name (no "/")
// cannot collide with.
func (e *Engine) createEntity(ctx context.Context, name string, policies []string, metadata map[string]string, disabled bool) (*Entity, error) {
	id, err := genID()
	if err != nil {
		return nil, err
	}
	ent := &Entity{ID: id, Name: name, Policies: policies, Metadata: metadata, Disabled: disabled, CreatedTime: e.now()}
	if err := e.putEntity(ctx, ent); err != nil {
		return nil, err
	}
	return ent, nil
}

func (e *Engine) putEntity(ctx context.Context, ent *Entity) error {
	blob, err := json.Marshal(ent)
	if err != nil {
		return fmt.Errorf("identity: marshal entity: %w", err)
	}
	if err := e.store.Put(ctx, &storage.Entry{Key: e.entityKey(ent.ID), Value: blob}); err != nil {
		return fmt.Errorf("identity: persist entity: %w", err)
	}
	if err := e.store.Put(ctx, &storage.Entry{Key: e.entityNameKey(ent.Name), Value: []byte(ent.ID)}); err != nil {
		return fmt.Errorf("identity: persist entity name index: %w", err)
	}
	return nil
}

// ReadEntity returns the entity with the given ID, or [ErrEntityNotFound].
func (e *Engine) ReadEntity(ctx context.Context, id string) (*Entity, error) {
	entry, err := e.store.Get(ctx, e.entityKey(id))
	if err != nil {
		return nil, fmt.Errorf("identity: read entity: %w", err)
	}
	if entry == nil {
		return nil, ErrEntityNotFound
	}
	var ent Entity
	if err := json.Unmarshal(entry.Value, &ent); err != nil {
		return nil, fmt.Errorf("identity: unmarshal entity: %w", err)
	}
	return &ent, nil
}

// ReadEntityByName returns the entity with the given name, or [ErrEntityNotFound].
func (e *Engine) ReadEntityByName(ctx context.Context, name string) (*Entity, error) {
	entry, err := e.store.Get(ctx, e.entityNameKey(name))
	if err != nil {
		return nil, fmt.Errorf("identity: read entity name index: %w", err)
	}
	if entry == nil {
		return nil, ErrEntityNotFound
	}
	return e.ReadEntity(ctx, string(entry.Value))
}

// ListEntities returns all entity IDs.
func (e *Engine) ListEntities(ctx context.Context) ([]string, error) {
	ids, err := e.store.List(ctx, e.prefix+"/entity/")
	if err != nil {
		return nil, fmt.Errorf("identity: list entities: %w", err)
	}
	return ids, nil
}

// DeleteEntity removes an entity, its name index, and every alias pointing at
// it. It is a no-op if the entity is absent.
func (e *Engine) DeleteEntity(ctx context.Context, id string) error {
	ent, err := e.ReadEntity(ctx, id)
	if errors.Is(err, ErrEntityNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	aliasIDs, err := e.store.List(ctx, e.prefix+"/alias/")
	if err != nil {
		return fmt.Errorf("identity: list aliases: %w", err)
	}
	for _, aid := range aliasIDs {
		a, err := e.ReadAlias(ctx, aid)
		if err != nil {
			continue
		}
		if a.EntityID == id {
			if err := e.DeleteAlias(ctx, aid); err != nil {
				return err
			}
		}
	}
	if err := e.store.Delete(ctx, e.entityNameKey(ent.Name)); err != nil {
		return fmt.Errorf("identity: delete entity name index: %w", err)
	}
	if err := e.store.Delete(ctx, e.entityKey(id)); err != nil {
		return fmt.Errorf("identity: delete entity: %w", err)
	}
	return nil
}

// --- Aliases ---

// CreateAlias binds (mountType, name) to an existing entity. It errors with
// [ErrEntityNotFound] if the entity is absent and [ErrInvalidName] on a bad
// mount type or name.
func (e *Engine) CreateAlias(ctx context.Context, entityID, mountType, name string) (*Alias, error) {
	if !validName(mountType) || name == "" {
		return nil, ErrInvalidName
	}
	if _, err := e.ReadEntity(ctx, entityID); err != nil {
		return nil, err
	}
	return e.putAlias(ctx, entityID, mountType, name, nil)
}

func (e *Engine) putAlias(ctx context.Context, entityID, mountType, name string, groups []string) (*Alias, error) {
	id, err := genID()
	if err != nil {
		return nil, err
	}
	a := &Alias{ID: id, EntityID: entityID, MountType: mountType, Name: name, Groups: groups, CreatedTime: e.now()}
	blob, err := json.Marshal(a)
	if err != nil {
		return nil, fmt.Errorf("identity: marshal alias: %w", err)
	}
	if err := e.store.Put(ctx, &storage.Entry{Key: e.aliasKey(id), Value: blob}); err != nil {
		return nil, fmt.Errorf("identity: persist alias: %w", err)
	}
	if err := e.store.Put(ctx, &storage.Entry{Key: e.aliasIndexKey(mountType, name), Value: []byte(id)}); err != nil {
		return nil, fmt.Errorf("identity: persist alias index: %w", err)
	}
	return a, nil
}

// saveAlias re-persists an existing alias record (its index is unchanged).
func (e *Engine) saveAlias(ctx context.Context, a *Alias) error {
	blob, err := json.Marshal(a)
	if err != nil {
		return fmt.Errorf("identity: marshal alias: %w", err)
	}
	if err := e.store.Put(ctx, &storage.Entry{Key: e.aliasKey(a.ID), Value: blob}); err != nil {
		return fmt.Errorf("identity: persist alias: %w", err)
	}
	return nil
}

// ReadAlias returns the alias with the given ID, or [ErrAliasNotFound].
func (e *Engine) ReadAlias(ctx context.Context, id string) (*Alias, error) {
	entry, err := e.store.Get(ctx, e.aliasKey(id))
	if err != nil {
		return nil, fmt.Errorf("identity: read alias: %w", err)
	}
	if entry == nil {
		return nil, ErrAliasNotFound
	}
	var a Alias
	if err := json.Unmarshal(entry.Value, &a); err != nil {
		return nil, fmt.Errorf("identity: unmarshal alias: %w", err)
	}
	return &a, nil
}

// DeleteAlias removes an alias and its index. It is a no-op if absent.
func (e *Engine) DeleteAlias(ctx context.Context, id string) error {
	a, err := e.ReadAlias(ctx, id)
	if errors.Is(err, ErrAliasNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := e.store.Delete(ctx, e.aliasIndexKey(a.MountType, a.Name)); err != nil {
		return fmt.Errorf("identity: delete alias index: %w", err)
	}
	if err := e.store.Delete(ctx, e.aliasKey(id)); err != nil {
		return fmt.Errorf("identity: delete alias: %w", err)
	}
	return nil
}

// --- Resolution (the token.Aliaser seam) ---

// ResolveAlias maps an auth-method login to its entity ID. If no alias exists
// and auto-creation is on, it materializes an entity (named "<mountType>/<name>")
// and alias and returns the new ID; if auto-creation is off it returns "" (the
// login proceeds with no identity). groups is the set of group memberships the
// auth method asserted for this login; it is recorded on the alias (replacing
// any prior assertion) so external groups can match against it. A disabled
// entity still resolves (so the login's own role policies still apply) but
// contributes no policies — see [Engine.PoliciesFor]; hard login-blocking on
// disable is a later-phase refinement.
func (e *Engine) ResolveAlias(ctx context.Context, mountType, name string, groups []string) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	idxKey := e.aliasIndexKey(mountType, name)
	entry, err := e.store.Get(ctx, idxKey)
	if err != nil {
		return "", fmt.Errorf("identity: read alias index: %w", err)
	}
	if entry != nil {
		alias, err := e.ReadAlias(ctx, string(entry.Value))
		if err != nil {
			return "", err
		}
		ent, err := e.ReadEntity(ctx, alias.EntityID)
		if err != nil {
			return "", err
		}
		// Refresh the asserted groups from this login.
		if !sameStrings(alias.Groups, groups) {
			alias.Groups = groups
			if err := e.saveAlias(ctx, alias); err != nil {
				return "", err
			}
		}
		return ent.ID, nil
	}

	if !e.autoCreate {
		return "", nil
	}
	ent, err := e.createEntity(ctx, mountType+"/"+name, nil, nil, false)
	if err != nil {
		return "", err
	}
	if _, err := e.putAlias(ctx, ent.ID, mountType, name, groups); err != nil {
		return "", err
	}
	return ent.ID, nil
}

// TemplateValues returns the identity placeholder values for an entity, keyed as
// they appear in policy templates ("identity.entity.id", "identity.entity.name",
// "identity.entity.metadata.<key>"). It returns nil if the entity is absent or
// disabled, so a templated rule referencing it drops (fail-closed).
func (e *Engine) TemplateValues(ctx context.Context, entityID string) (map[string]string, error) {
	if entityID == "" {
		return nil, nil
	}
	ent, err := e.ReadEntity(ctx, entityID)
	if errors.Is(err, ErrEntityNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if ent.Disabled {
		return nil, nil
	}
	vals := map[string]string{
		"identity.entity.id":   ent.ID,
		"identity.entity.name": ent.Name,
	}
	for k, v := range ent.Metadata {
		vals["identity.entity.metadata."+k] = v
	}
	return vals, nil
}

// PoliciesFor returns the policies contributed by an entity: its own policies
// plus those of every group it is a transitive member of. It returns nil if the
// entity is absent or disabled.
func (e *Engine) PoliciesFor(ctx context.Context, entityID string) ([]string, error) {
	if entityID == "" {
		return nil, nil
	}
	ent, err := e.ReadEntity(ctx, entityID)
	if errors.Is(err, ErrEntityNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if ent.Disabled {
		return nil, nil
	}
	groupPolicies, err := e.groupPoliciesForEntity(ctx, entityID)
	if err != nil {
		return nil, err
	}
	if len(groupPolicies) == 0 {
		return ent.Policies, nil
	}
	return append(append([]string{}, ent.Policies...), groupPolicies...), nil
}
