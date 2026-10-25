package storage

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// filePrefix is prepended to the on-disk filename of every leaf key. It keeps a
// leaf ("a/b") and an intermediate directory of the same name ("a/b/c") from
// colliding: the leaf becomes the file "_b" while the subtree is the directory
// "b". This mirrors HashiCorp Vault's file backend.
const filePrefix = "_"

// tempFilePrefix names the transient files Put writes before renaming into place.
// It deliberately does NOT start with filePrefix: a crash between creating a temp
// file and renaming it leaves the temp file behind, and List only surfaces
// filePrefix-prefixed files as keys — so a leftover temp file is never mistaken
// for a data key (in List, Walk, or a snapshot). The leading "." also keeps it
// out of ordinary listings.
const tempFilePrefix = ".uvtmp-"

const (
	dirPerm  os.FileMode = 0o700 // secrets hygiene: not group/world accessible
	filePerm os.FileMode = 0o600
)

// FileBackend is a [Backend] that persists each entry as a file under a root
// directory. Values are stored as-is (already encrypted by the barrier); the
// restrictive permissions are defense in depth, not the primary protection.
type FileBackend struct {
	root string
}

// NewFileBackend returns a file backend rooted at dir, creating dir if needed.
func NewFileBackend(dir string) (*FileBackend, error) {
	if dir == "" {
		return nil, fmt.Errorf("storage: file backend root must not be empty")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("storage: resolve root: %w", err)
	}
	if err := os.MkdirAll(abs, dirPerm); err != nil {
		return nil, fmt.Errorf("storage: create root: %w", err)
	}
	return &FileBackend{root: abs}, nil
}

var _ Backend = (*FileBackend)(nil)

// pathFor maps a validated key to the directory holding it and the full path of
// its leaf file. It also verifies the result stays within the backend root, a
// defense-in-depth guard against path traversal on top of [ValidateKey].
func (b *FileBackend) pathFor(key string) (dir, full string, err error) {
	segs := strings.Split(key, "/")
	last := segs[len(segs)-1]
	dir = filepath.Join(append([]string{b.root}, segs[:len(segs)-1]...)...)
	full = filepath.Join(dir, filePrefix+last)

	rel, err := filepath.Rel(b.root, full)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", "", ErrInvalidKey
	}
	return dir, full, nil
}

// Get implements [Backend].
func (b *FileBackend) Get(_ context.Context, key string) (*Entry, error) {
	if err := ValidateKey(key); err != nil {
		return nil, err
	}
	_, full, err := b.pathFor(key)
	if err != nil {
		return nil, err
	}
	// full is derived from a key that passed ValidateKey (no "." / ".." / empty
	// segments) and pathFor confirms it stays within b.root, so this read cannot
	// be coerced outside the store.
	data, err := os.ReadFile(full) //nolint:gosec // G304: path validated and root-confined above

	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("storage: read %q: %w", key, err)
	}
	return &Entry{Key: key, Value: data}, nil
}

// Put implements [Backend]. The write is atomic: data is written to a temporary
// file in the destination directory and renamed into place.
func (b *FileBackend) Put(_ context.Context, entry *Entry) error {
	if err := ValidateKey(entry.Key); err != nil {
		return err
	}
	dir, full, err := b.pathFor(entry.Key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return fmt.Errorf("storage: create dir for %q: %w", entry.Key, err)
	}

	tmp, err := os.CreateTemp(dir, tempFilePrefix+"*")
	if err != nil {
		return fmt.Errorf("storage: temp file for %q: %w", entry.Key, err)
	}
	tmpName := tmp.Name()
	// Best-effort cleanup if we fail before the rename succeeds.
	defer func() { _ = os.Remove(tmpName) }()

	if err := tmp.Chmod(filePerm); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("storage: chmod temp for %q: %w", entry.Key, err)
	}
	if _, err := tmp.Write(entry.Value); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("storage: write %q: %w", entry.Key, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("storage: sync %q: %w", entry.Key, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("storage: close temp for %q: %w", entry.Key, err)
	}
	if err := os.Rename(tmpName, full); err != nil {
		return fmt.Errorf("storage: rename %q: %w", entry.Key, err)
	}
	return nil
}

// Delete implements [Backend]. Empty parent directories are pruned best-effort.
func (b *FileBackend) Delete(_ context.Context, key string) error {
	if err := ValidateKey(key); err != nil {
		return err
	}
	dir, full, err := b.pathFor(key)
	if err != nil {
		return err
	}
	if err := os.Remove(full); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("storage: delete %q: %w", key, err)
	}
	b.pruneEmptyDirs(dir)
	return nil
}

// pruneEmptyDirs removes now-empty directories from dir upward, stopping at (and
// never removing) the backend root. Failures are ignored: a non-empty directory
// simply stops the walk.
func (b *FileBackend) pruneEmptyDirs(dir string) {
	for dir != b.root && strings.HasPrefix(dir, b.root) {
		if err := os.Remove(dir); err != nil {
			return // not empty, or gone — stop pruning
		}
		dir = filepath.Dir(dir)
	}
}

// List implements [Backend].
func (b *FileBackend) List(_ context.Context, prefix string) ([]string, error) {
	if err := validatePrefix(prefix); err != nil {
		return nil, err
	}

	dir := b.root
	if prefix != "" {
		segs := strings.Split(strings.TrimSuffix(prefix, "/"), "/")
		dir = filepath.Join(append([]string{b.root}, segs...)...)
	}

	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return []string{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("storage: list %q: %w", prefix, err)
	}

	out := make([]string, 0, len(entries))
	for _, e := range entries {
		name := e.Name()
		switch {
		case e.IsDir():
			out = append(out, name+"/")
		case strings.HasPrefix(name, filePrefix):
			out = append(out, strings.TrimPrefix(name, filePrefix))
		}
	}
	sort.Strings(out)
	return out, nil
}
