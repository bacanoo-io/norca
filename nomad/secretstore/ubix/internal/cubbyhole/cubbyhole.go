// Package cubbyhole implements per-token private storage: a scratch space whose
// contents are readable and writable only by the token that created them.
//
// Unlike the KV engine, a cubbyhole is not shared. Each token gets its own
// namespace, keyed by the SHA-256 of the token value (never the value itself, so
// it does not leak in on-disk key names — the same reasoning as the token
// store). No policy grants one token access to another's cubbyhole, not even a
// root token; the scoping is structural, not an ACL rule. When a token is
// revoked its cubbyhole is destroyed (see [Engine.Destroy]), so the data never
// outlives the credential.
package cubbyhole

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/cwolsen7905/ubixvault/internal/storage"
)

// Errors.
var (
	ErrSecretNotFound = errors.New("cubbyhole: secret not found")
	ErrInvalidPath    = errors.New("cubbyhole: invalid path")
)

// Storage is the subset of a backend the engine needs.
type Storage interface {
	Get(ctx context.Context, key string) (*storage.Entry, error)
	Put(ctx context.Context, entry *storage.Entry) error
	Delete(ctx context.Context, key string) error
	List(ctx context.Context, prefix string) ([]string, error)
}

// Engine is the cubbyhole secrets engine.
type Engine struct {
	store  Storage
	prefix string
}

// New returns an engine storing under prefix (e.g. "cubbyhole").
func New(store Storage, prefix string) *Engine {
	return &Engine{store: store, prefix: strings.Trim(prefix, "/")}
}

// tokenScope maps a token value to its private storage prefix. Hashing keeps the
// raw token out of (unencrypted) key names, matching the token store's scheme.
func (e *Engine) tokenScope(tokenID string) string {
	sum := sha256.Sum256([]byte(tokenID))
	return e.prefix + "/" + hex.EncodeToString(sum[:]) + "/"
}

// key is the storage key for a path within a token's cubbyhole.
func (e *Engine) key(tokenID, path string) string {
	return e.tokenScope(tokenID) + path
}

// validPath rejects empty and slash-bounded paths; a cubbyhole path may nest
// ("a/b/c") but must name a leaf.
func validPath(path string) bool {
	return path != "" && !strings.HasPrefix(path, "/") && !strings.HasSuffix(path, "/")
}

// Write stores data at path within tokenID's cubbyhole, replacing any prior
// value. Writing an empty map is allowed (it creates the entry).
func (e *Engine) Write(ctx context.Context, tokenID, path string, data map[string]any) error {
	if !validPath(path) {
		return ErrInvalidPath
	}
	blob, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("cubbyhole: marshal: %w", err)
	}
	if err := e.store.Put(ctx, &storage.Entry{Key: e.key(tokenID, path), Value: blob}); err != nil {
		return fmt.Errorf("cubbyhole: persist: %w", err)
	}
	return nil
}

// Read returns the data stored at path within tokenID's cubbyhole, or
// [ErrSecretNotFound].
func (e *Engine) Read(ctx context.Context, tokenID, path string) (map[string]any, error) {
	if !validPath(path) {
		return nil, ErrInvalidPath
	}
	entry, err := e.store.Get(ctx, e.key(tokenID, path))
	if err != nil {
		return nil, fmt.Errorf("cubbyhole: read: %w", err)
	}
	if entry == nil {
		return nil, ErrSecretNotFound
	}
	var data map[string]any
	if err := json.Unmarshal(entry.Value, &data); err != nil {
		return nil, fmt.Errorf("cubbyhole: unmarshal: %w", err)
	}
	return data, nil
}

// List returns the immediate child keys under path within tokenID's cubbyhole
// (subdirectories carry a trailing "/"). An empty path lists the root.
func (e *Engine) List(ctx context.Context, tokenID, path string) ([]string, error) {
	if path != "" && (strings.HasPrefix(path, "/") || !strings.HasSuffix(path, "/")) {
		// A list path names a directory: it may be "" (root) or end in "/".
		path += "/"
	}
	keys, err := e.store.List(ctx, e.tokenScope(tokenID)+path)
	if err != nil {
		return nil, fmt.Errorf("cubbyhole: list: %w", err)
	}
	return keys, nil
}

// Delete removes the entry at path within tokenID's cubbyhole. It is a no-op if
// the path is absent.
func (e *Engine) Delete(ctx context.Context, tokenID, path string) error {
	if !validPath(path) {
		return ErrInvalidPath
	}
	if err := e.store.Delete(ctx, e.key(tokenID, path)); err != nil {
		return fmt.Errorf("cubbyhole: delete: %w", err)
	}
	return nil
}

// Destroy removes a token's entire cubbyhole. It is called when the token is
// revoked so its private data does not outlive it, and is a no-op if the token
// never wrote anything.
func (e *Engine) Destroy(ctx context.Context, tokenID string) error {
	if err := e.deleteTree(ctx, e.tokenScope(tokenID)); err != nil {
		return fmt.Errorf("cubbyhole: destroy: %w", err)
	}
	return nil
}

// deleteTree recursively removes every entry under prefix. List returns
// immediate children only, with subdirectories suffixed by "/".
func (e *Engine) deleteTree(ctx context.Context, prefix string) error {
	children, err := e.store.List(ctx, prefix)
	if err != nil {
		return err
	}
	for _, c := range children {
		if strings.HasSuffix(c, "/") {
			if err := e.deleteTree(ctx, prefix+c); err != nil {
				return err
			}
			continue
		}
		if err := e.store.Delete(ctx, prefix+c); err != nil {
			return err
		}
	}
	return nil
}
