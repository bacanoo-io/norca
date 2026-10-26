// Package storage defines uBixVault's durable persistence layer.
//
// A [Backend] is a simple key/value blob store. It persists opaque bytes and has
// no knowledge of encryption: the barrier encrypts every value before it reaches
// a Backend, so a Backend never sees plaintext (see docs/DESIGN.md §3.2). Keeping
// this interface small and encryption-agnostic is what lets alternative backends
// (in-memory, file, and later Raft) be substituted freely — the Liskov and
// Interface-Segregation principles from docs/DESIGN.md §7 applied concretely.
//
// Keys are "/"-separated paths that form a logical hierarchy. See [Backend.List]
// for how that hierarchy is enumerated.
package storage

import (
	"context"
	"errors"
	"strings"
)

// ErrInvalidKey is returned by Backend methods when a key does not satisfy the
// rules enforced by [ValidateKey].
var ErrInvalidKey = errors.New("storage: invalid key")

// Entry is a single key/value pair stored in a [Backend]. Value is opaque bytes;
// the storage layer never interprets it.
type Entry struct {
	Key   string
	Value []byte
}

// Backend is a durable key/value store over "/"-separated keys.
//
// Implementations must be safe for concurrent use by multiple goroutines.
//
// A missing key is not an error: [Backend.Get] returns (nil, nil) when the key
// does not exist, and [Backend.Delete] is a no-op on a missing key. This mirrors
// HashiCorp Vault's physical-backend convention, consistent with the wire
// compatibility goal in docs/DECISIONS.md (D-003).
type Backend interface {
	// Get returns the entry stored at key, or (nil, nil) if it does not exist.
	Get(ctx context.Context, key string) (*Entry, error)

	// Put stores entry, overwriting any existing value at entry.Key.
	Put(ctx context.Context, entry *Entry) error

	// Delete removes the value at key. It is a no-op if key does not exist.
	Delete(ctx context.Context, key string) error

	// List returns the immediate children under prefix, not a recursive walk.
	//
	// prefix must be empty (the root) or end with "/". Leaf keys are returned as
	// their final path segment; nested subtrees are returned as "segment/" (with
	// a trailing slash). Results are sorted ascending and de-duplicated. A prefix
	// with no children returns an empty slice, not an error.
	//
	// For keys {"a", "b/c", "b/d", "e/f/g"}:
	//   List("")   -> ["a", "b/", "e/"]
	//   List("b/") -> ["c", "d"]
	//   List("e/") -> ["f/"]
	List(ctx context.Context, prefix string) ([]string, error)
}

// ValidateKey reports whether key is a well-formed storage key. A valid key:
//   - is non-empty,
//   - does not start or end with "/",
//   - contains no empty segments (i.e. no "//"),
//   - contains no "." or ".." segments (which could escape the store).
//
// It returns [ErrInvalidKey] for any violation. Rejecting "." and ".." segments
// is a defense-in-depth measure so that file-backed stores cannot be coerced into
// path traversal.
func ValidateKey(key string) error {
	if key == "" || strings.HasPrefix(key, "/") || strings.HasSuffix(key, "/") {
		return ErrInvalidKey
	}
	for _, seg := range strings.Split(key, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return ErrInvalidKey
		}
	}
	return nil
}

// validatePrefix reports whether prefix is acceptable for [Backend.List]: either
// empty (the root) or a non-empty, non-root path ending in "/".
func validatePrefix(prefix string) error {
	if prefix == "" {
		return nil
	}
	if !strings.HasSuffix(prefix, "/") {
		return ErrInvalidKey
	}
	// Strip the trailing slash and validate the remainder as a key.
	return ValidateKey(strings.TrimSuffix(prefix, "/"))
}

// Walk visits every entry in b, calling fn for each leaf (in unspecified order).
// It descends the "/"-separated hierarchy exposed by [Backend.List], reading each
// leaf with [Backend.Get]. If fn returns an error, the walk stops and returns it.
func Walk(ctx context.Context, b Backend, fn func(*Entry) error) error {
	return walk(ctx, b, "", fn)
}

func walk(ctx context.Context, b Backend, prefix string, fn func(*Entry) error) error {
	children, err := b.List(ctx, prefix)
	if err != nil {
		return err
	}
	for _, c := range children {
		if strings.HasSuffix(c, "/") {
			if err := walk(ctx, b, prefix+c, fn); err != nil {
				return err
			}
			continue
		}
		entry, err := b.Get(ctx, prefix+c)
		if err != nil {
			return err
		}
		if entry != nil {
			if err := fn(entry); err != nil {
				return err
			}
		}
	}
	return nil
}

// childUnder returns the immediate child of prefix on the path to key, and
// whether key lies under prefix at all. If the child is itself an intermediate
// segment (key has further path elements below it), the returned child carries a
// trailing "/". prefix must be "" or end with "/".
//
// e.g. childUnder("b/", "b/c/d") == ("c/", true); childUnder("b/", "b/c") == ("c", true).
func childUnder(prefix, key string) (string, bool) {
	if !strings.HasPrefix(key, prefix) {
		return "", false
	}
	rest := key[len(prefix):]
	if rest == "" {
		return "", false
	}
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		return rest[:i] + "/", true
	}
	return rest, true
}
