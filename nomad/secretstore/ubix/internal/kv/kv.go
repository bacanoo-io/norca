// Package kv implements a versioned key/value secrets engine (KV v2), matching
// the model of HashiCorp Vault's kv-v2 (docs/DESIGN.md §3.6).
//
// Each secret path holds an ordered history of versions. Writing creates a new
// version; reads return the latest or a specific version. Versions can be
// soft-deleted (recoverable), undeleted, or destroyed (data permanently
// removed). A per-secret max-versions bound ages out the oldest versions.
//
// The engine operates over a small [Storage] interface — defined here, on the
// consumer side (docs/DESIGN.md §7.1) — which both *barrier.Barrier and the raw
// storage backends satisfy. In production it is wired to the unsealed barrier so
// all secret data is encrypted at rest.
package kv

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/cwolsen7905/ubixvault/internal/storage"
)

// DefaultMaxVersions is the number of versions retained per secret when a mount
// does not specify otherwise.
const DefaultMaxVersions = 10

// Storage is the subset of a backend the engine needs. Both *barrier.Barrier and
// the storage backends satisfy it.
type Storage interface {
	Get(ctx context.Context, key string) (*storage.Entry, error)
	Put(ctx context.Context, entry *storage.Entry) error
	Delete(ctx context.Context, key string) error
	List(ctx context.Context, prefix string) ([]string, error)
}

// Engine errors.
var (
	ErrSecretNotFound   = errors.New("kv: secret not found")
	ErrVersionNotFound  = errors.New("kv: version not found")
	ErrVersionDeleted   = errors.New("kv: version is deleted")
	ErrVersionDestroyed = errors.New("kv: version is destroyed")
	ErrInvalidPath      = errors.New("kv: invalid secret path")
)

// VersionMeta describes one version of a secret.
type VersionMeta struct {
	Version     int       `json:"version"`
	CreatedTime time.Time `json:"created_time"`
	Deleted     bool      `json:"deleted"`
	Destroyed   bool      `json:"destroyed"`
}

// Metadata is the version history for a secret path.
type Metadata struct {
	CurrentVersion int                  `json:"current_version"`
	OldestVersion  int                  `json:"oldest_version"`
	MaxVersions    int                  `json:"max_versions"`
	CreatedTime    time.Time            `json:"created_time"`
	UpdatedTime    time.Time            `json:"updated_time"`
	Versions       map[int]*VersionMeta `json:"versions"`
}

// Engine is a KV v2 secrets engine mounted at a storage prefix.
type Engine struct {
	store  Storage
	prefix string
	maxVer int
	now    func() time.Time
}

// New returns a KV engine storing under prefix (e.g. "secret").
func New(store Storage, prefix string) *Engine {
	return &Engine{
		store:  store,
		prefix: strings.Trim(prefix, "/"),
		maxVer: DefaultMaxVersions,
		now:    func() time.Time { return time.Now().UTC() },
	}
}

func (e *Engine) metaKey(path string) string { return e.prefix + "/meta/" + path }
func (e *Engine) dataKey(path string, version int) string {
	return e.prefix + "/data/" + path + "/" + strconv.Itoa(version)
}

// validatePath ensures the composed storage keys are well-formed.
func (e *Engine) validatePath(path string) error {
	if path == "" {
		return ErrInvalidPath
	}
	if err := storage.ValidateKey(e.metaKey(path)); err != nil {
		return ErrInvalidPath
	}
	return nil
}

// Write stores data as a new version and returns its metadata.
func (e *Engine) Write(ctx context.Context, path string, data map[string]any) (*VersionMeta, error) {
	if err := e.validatePath(path); err != nil {
		return nil, err
	}
	meta, err := e.loadMeta(ctx, path)
	if err != nil {
		return nil, err
	}
	now := e.now()
	version := meta.CurrentVersion + 1

	blob, err := json.Marshal(data)
	if err != nil {
		return nil, fmt.Errorf("kv: marshal data: %w", err)
	}
	if err := e.store.Put(ctx, &storage.Entry{Key: e.dataKey(path, version), Value: blob}); err != nil {
		return nil, fmt.Errorf("kv: store version: %w", err)
	}

	vm := &VersionMeta{Version: version, CreatedTime: now}
	meta.Versions[version] = vm
	meta.CurrentVersion = version
	if meta.OldestVersion == 0 {
		meta.OldestVersion = version
	}
	if meta.CreatedTime.IsZero() {
		meta.CreatedTime = now
	}
	meta.UpdatedTime = now

	if err := e.enforceMaxVersions(ctx, path, meta); err != nil {
		return nil, err
	}
	if err := e.saveMeta(ctx, path, meta); err != nil {
		return nil, err
	}
	return vm, nil
}

// Read returns the data and metadata for a version (0 means the latest).
func (e *Engine) Read(ctx context.Context, path string, version int) (map[string]any, *VersionMeta, error) {
	if err := e.validatePath(path); err != nil {
		return nil, nil, err
	}
	meta, err := e.readExistingMeta(ctx, path)
	if err != nil {
		return nil, nil, err
	}
	if version == 0 {
		version = meta.CurrentVersion
	}
	vm, ok := meta.Versions[version]
	if !ok {
		return nil, nil, ErrVersionNotFound
	}
	if vm.Destroyed {
		return nil, vm, ErrVersionDestroyed
	}
	if vm.Deleted {
		return nil, vm, ErrVersionDeleted
	}

	entry, err := e.store.Get(ctx, e.dataKey(path, version))
	if err != nil {
		return nil, nil, fmt.Errorf("kv: read version: %w", err)
	}
	if entry == nil {
		return nil, vm, ErrVersionNotFound
	}
	var data map[string]any
	if err := json.Unmarshal(entry.Value, &data); err != nil {
		return nil, nil, fmt.Errorf("kv: unmarshal data: %w", err)
	}
	return data, vm, nil
}

// Delete soft-deletes the given versions (0 means the latest). Data is retained
// and can be recovered with Undelete.
func (e *Engine) Delete(ctx context.Context, path string, versions ...int) error {
	return e.mutateVersions(ctx, path, versions, func(vm *VersionMeta) {
		if !vm.Destroyed {
			vm.Deleted = true
		}
	})
}

// Undelete clears the soft-delete flag on the given versions.
func (e *Engine) Undelete(ctx context.Context, path string, versions ...int) error {
	return e.mutateVersions(ctx, path, versions, func(vm *VersionMeta) {
		if !vm.Destroyed {
			vm.Deleted = false
		}
	})
}

// Destroy permanently removes the data for the given versions.
func (e *Engine) Destroy(ctx context.Context, path string, versions ...int) error {
	if err := e.validatePath(path); err != nil {
		return err
	}
	meta, err := e.readExistingMeta(ctx, path)
	if err != nil {
		return err
	}
	targets := versions
	if len(targets) == 0 {
		targets = []int{meta.CurrentVersion}
	}
	for _, v := range targets {
		vm, ok := meta.Versions[v]
		if !ok {
			continue
		}
		if err := e.store.Delete(ctx, e.dataKey(path, v)); err != nil {
			return fmt.Errorf("kv: destroy version %d: %w", v, err)
		}
		vm.Destroyed = true
	}
	meta.UpdatedTime = e.now()
	return e.saveMeta(ctx, path, meta)
}

// ReadMetadata returns the version history for a secret.
func (e *Engine) ReadMetadata(ctx context.Context, path string) (*Metadata, error) {
	if err := e.validatePath(path); err != nil {
		return nil, err
	}
	return e.readExistingMeta(ctx, path)
}

// DeleteMetadata removes a secret entirely: all versions' data and the metadata.
func (e *Engine) DeleteMetadata(ctx context.Context, path string) error {
	if err := e.validatePath(path); err != nil {
		return err
	}
	meta, err := e.readExistingMeta(ctx, path)
	if errors.Is(err, ErrSecretNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	for v := range meta.Versions {
		if err := e.store.Delete(ctx, e.dataKey(path, v)); err != nil {
			return fmt.Errorf("kv: delete version %d: %w", v, err)
		}
	}
	return e.store.Delete(ctx, e.metaKey(path))
}

// List returns the immediate child secret names under path ("" for the root).
func (e *Engine) List(ctx context.Context, path string) ([]string, error) {
	prefix := e.prefix + "/meta/"
	if path != "" {
		prefix += strings.TrimSuffix(path, "/") + "/"
	}
	return e.store.List(ctx, prefix)
}

// mutateVersions applies fn to the named versions (0 => latest) and saves.
func (e *Engine) mutateVersions(ctx context.Context, path string, versions []int, fn func(*VersionMeta)) error {
	if err := e.validatePath(path); err != nil {
		return err
	}
	meta, err := e.readExistingMeta(ctx, path)
	if err != nil {
		return err
	}
	targets := versions
	if len(targets) == 0 {
		targets = []int{meta.CurrentVersion}
	}
	for _, v := range targets {
		if vm, ok := meta.Versions[v]; ok {
			fn(vm)
		}
	}
	meta.UpdatedTime = e.now()
	return e.saveMeta(ctx, path, meta)
}

// enforceMaxVersions destroys the oldest versions until the retained count is
// within the max, advancing OldestVersion.
func (e *Engine) enforceMaxVersions(ctx context.Context, path string, meta *Metadata) error {
	for meta.CurrentVersion-meta.OldestVersion+1 > meta.MaxVersions {
		old := meta.OldestVersion
		if err := e.store.Delete(ctx, e.dataKey(path, old)); err != nil {
			return fmt.Errorf("kv: age out version %d: %w", old, err)
		}
		delete(meta.Versions, old)
		meta.OldestVersion = old + 1
	}
	return nil
}

// loadMeta returns existing metadata or a fresh, empty one for a new secret.
func (e *Engine) loadMeta(ctx context.Context, path string) (*Metadata, error) {
	meta, err := e.readExistingMeta(ctx, path)
	if errors.Is(err, ErrSecretNotFound) {
		return &Metadata{MaxVersions: e.maxVer, Versions: map[int]*VersionMeta{}}, nil
	}
	return meta, err
}

func (e *Engine) readExistingMeta(ctx context.Context, path string) (*Metadata, error) {
	entry, err := e.store.Get(ctx, e.metaKey(path))
	if err != nil {
		return nil, fmt.Errorf("kv: read metadata: %w", err)
	}
	if entry == nil {
		return nil, ErrSecretNotFound
	}
	var meta Metadata
	if err := json.Unmarshal(entry.Value, &meta); err != nil {
		return nil, fmt.Errorf("kv: unmarshal metadata: %w", err)
	}
	if meta.Versions == nil {
		meta.Versions = map[int]*VersionMeta{}
	}
	if meta.MaxVersions == 0 {
		meta.MaxVersions = e.maxVer
	}
	return &meta, nil
}

func (e *Engine) saveMeta(ctx context.Context, path string, meta *Metadata) error {
	blob, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("kv: marshal metadata: %w", err)
	}
	if err := e.store.Put(ctx, &storage.Entry{Key: e.metaKey(path), Value: blob}); err != nil {
		return fmt.Errorf("kv: save metadata: %w", err)
	}
	return nil
}
