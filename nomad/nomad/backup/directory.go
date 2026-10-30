// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package backup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/hashicorp/nomad/nomad/structs"
)

// ErrNoDownloadURL is returned for a pre-signed download URL of an object on
// a directory destination: only S3 stores sign URLs.
var ErrNoDownloadURL = errors.New("download URLs are not available for directory destinations")

// tmpMarker marks the temporary file of an object being written. A crash
// leaves it behind; AbortIncompleteUploads removes it.
const tmpMarker = ".norca-tmp-"

// dirDestination is a directory of the server's filesystem. Objects are files
// below <path>/<prefix>, in the same layout as the keys of an S3 store.
//
// Backups are taken by the leader of the region, so a directory destination
// only works across leader changes if the directory is the same storage on
// every server (a shared mount at the same path), or if the region has a
// single server.
type dirDestination struct {
	name string
	root string

	// server is set on the destinations of the server's configuration: a
	// backup that its record lists on the destination but that is missing
	// then points at the shared storage requirement.
	server bool
}

// newDirDestination creates a directory destination. It does not touch the
// filesystem; the directories are created by the first write.
func newDirDestination(cfg *DestinationConfig, region string) (*dirDestination, error) {
	if !filepath.IsAbs(cfg.Path) {
		return nil, fmt.Errorf("path %q must be absolute", cfg.Path)
	}
	prefix := cfg.Prefix
	if prefix == "" {
		prefix = DefaultPrefix
	}
	prefix = strings.Trim(strings.ReplaceAll(prefix, "${region}", region), "/")
	if hasDotDot(prefix) {
		return nil, fmt.Errorf("prefix %q must not contain \"..\"", prefix)
	}
	return &dirDestination{
		name: cfg.Name,
		root: filepath.Join(filepath.Clean(cfg.Path), filepath.FromSlash(prefix)),
	}, nil
}

// hasDotDot reports whether a slash-separated path has a ".." element. A
// backslash separates elements too, as on Windows.
func hasDotDot(p string) bool {
	for _, el := range strings.FieldsFunc(p, func(r rune) bool { return r == '/' || r == '\\' }) {
		if el == ".." {
			return true
		}
	}
	return false
}

// fileURL returns the file:// URL of an absolute path, for display. On
// Windows a drive path is file:///C:/... and a UNC path file://host/share/...
func fileURL(p string) string {
	s := filepath.ToSlash(p)
	if strings.HasPrefix(s, "//") {
		return "file:" + s
	}
	if !strings.HasPrefix(s, "/") {
		s = "/" + s
	}
	return "file://" + s
}

func (d *dirDestination) Name() string { return d.name }

// resolve returns the file of a key. It never leaves the root of the
// destination.
func (d *dirDestination) resolve(key string) (string, error) {
	if hasDotDot(key) {
		return "", fmt.Errorf("invalid key %q", key)
	}
	full := filepath.Join(d.root, filepath.FromSlash(key))
	rel, err := filepath.Rel(d.root, full)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", fmt.Errorf("invalid key %q", key)
	}
	return full, nil
}

func (d *dirDestination) Location(key string) string {
	full, err := d.resolve(key)
	if err != nil {
		full = d.root
	}
	loc := fileURL(full)
	if strings.HasSuffix(key, "/") && !strings.HasSuffix(loc, "/") {
		loc += "/"
	}
	return loc
}

// ctxReader stops a copy when the context ends.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// Put writes an object crash-safely: into a temporary file next to it, which
// is synced and then renamed over the object; the directory is synced after
// the rename where the OS supports it. Files are created 0600, directories
// 0700.
func (d *dirDestination) Put(ctx context.Context, key string, r io.Reader, size int64, _ string) error {
	full, err := d.resolve(key)
	if err != nil {
		return err
	}
	dir := filepath.Dir(full)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("failed to write %s: %w", d.Location(key), err)
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(full)+tmpMarker+"*")
	if err != nil {
		return fmt.Errorf("failed to write %s: %w", d.Location(key), err)
	}
	committed := false
	defer func() {
		if !committed {
			tmp.Close()
			os.Remove(tmp.Name())
		}
	}()

	n, err := io.Copy(tmp, ctxReader{ctx: ctx, r: r})
	if err == nil && size >= 0 && n != size {
		err = fmt.Errorf("wrote %d bytes, expected %d", n, size)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), full)
	}
	if err != nil {
		return fmt.Errorf("failed to write %s: %w", d.Location(key), err)
	}
	committed = true
	if err := syncDir(dir); err != nil {
		return fmt.Errorf("failed to write %s: %w", d.Location(key), err)
	}
	return nil
}

// syncDir makes a rename in a directory durable. Windows can't sync a
// directory; its rename is durable once the file's data is.
func syncDir(dir string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	err = f.Sync()
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

func (d *dirDestination) Get(_ context.Context, key string) (io.ReadCloser, error) {
	full, err := d.resolve(key)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(full)
	if err != nil {
		return nil, d.wrapErr(key, err)
	}
	if info, err := f.Stat(); err != nil || !info.Mode().IsRegular() {
		f.Close()
		if err == nil {
			err = errors.New("not a regular file")
		}
		return nil, d.wrapErr(key, err)
	}
	return f, nil
}

func (d *dirDestination) Size(_ context.Context, key string) (int64, error) {
	full, err := d.resolve(key)
	if err != nil {
		return 0, err
	}
	info, err := os.Stat(full)
	if err != nil {
		return 0, d.wrapErr(key, err)
	}
	if !info.Mode().IsRegular() {
		return 0, d.wrapErr(key, errors.New("not a regular file"))
	}
	return info.Size(), nil
}

func (d *dirDestination) wrapErr(key string, err error) error {
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%s: %w", d.Location(key), ErrNotFound)
	}
	return fmt.Errorf("%s: %w", d.Location(key), err)
}

// exists reports whether the folder or file of a key exists.
func (d *dirDestination) exists(key string) (bool, error) {
	full, err := d.resolve(key)
	if err != nil {
		return false, err
	}
	_, err = os.Lstat(full)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	default:
		return false, d.wrapErr(key, err)
	}
}

// DeletePrefix deletes the files under a key prefix, then the directories it
// leaves empty, up to the root of the destination. A prefix ending in "/" is
// a folder; the root itself is never removed.
func (d *dirDestination) DeletePrefix(_ context.Context, prefix string) error {
	full, err := d.resolve(prefix)
	if err != nil {
		return err
	}
	if prefix == "" || strings.HasSuffix(prefix, "/") {
		if full == d.root {
			return d.removeChildren(full, "")
		}
		if err := os.RemoveAll(full); err != nil {
			return fmt.Errorf("failed to delete %s: %w", d.Location(prefix), err)
		}
	} else if err := d.removeChildren(filepath.Dir(full), filepath.Base(full)); err != nil {
		return err
	}
	d.removeEmptyParents(filepath.Dir(full))
	return nil
}

// removeChildren removes the entries of a directory whose names start with
// a prefix.
func (d *dirDestination) removeChildren(dir, prefix string) error {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("failed to delete under %s: %w", fileURL(dir), err)
	}
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), prefix) {
			continue
		}
		if err := os.RemoveAll(filepath.Join(dir, e.Name())); err != nil {
			return fmt.Errorf("failed to delete %s: %w", fileURL(filepath.Join(dir, e.Name())), err)
		}
	}
	return nil
}

// removeEmptyParents removes empty directories from dir up to, not
// including, the root.
func (d *dirDestination) removeEmptyParents(dir string) {
	for {
		rel, err := filepath.Rel(d.root, dir)
		if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return
		}
		// Fails, and stops, on a directory that is not empty.
		if err := os.Remove(dir); err != nil {
			return
		}
		dir = filepath.Dir(dir)
	}
}

// AbortIncompleteUploads removes the temporary files of interrupted writes
// under a key prefix (a folder).
func (d *dirDestination) AbortIncompleteUploads(_ context.Context, prefix string) (int, error) {
	full, err := d.resolve(prefix)
	if err != nil {
		return 0, err
	}
	var removed int
	err = filepath.WalkDir(full, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if e.IsDir() || !strings.HasPrefix(e.Name(), ".") || !strings.Contains(e.Name(), tmpMarker) {
			return nil
		}
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		removed++
		return nil
	})
	if err != nil {
		return removed, fmt.Errorf("failed to remove incomplete files under %s: %w", d.Location(prefix), err)
	}
	return removed, nil
}

func (d *dirDestination) PresignGet(_ context.Context, key string, _ time.Duration) (string, error) {
	return "", fmt.Errorf("%w; the object is %s on the servers", ErrNoDownloadURL, d.Location(key))
}

// sharedStorageHint explains a backup that its record lists on a directory
// destination of the server's configuration but that this server does not
// find.
func sharedStorageHint(d *dirDestination, record *structs.BackupRecord) string {
	return fmt.Sprintf("backup %s was written to directory destination %q by server %q, but this server does not find it at %s: "+
		"a directory destination must be the same shared storage (the same mount at the same path) on every server of the region, "+
		"or the region must have a single server",
		record.ID, d.name, record.LeaderID, d.Location(record.Path+"/"))
}

// missingError adds the shared storage requirement to a missing object of a
// backup recorded on a directory destination of the server, when the whole
// folder of the backup is missing.
func missingError(dest Destination, record *structs.BackupRecord, err error) error {
	d, ok := dest.(*dirDestination)
	if !ok || !d.server || !errors.Is(err, ErrNotFound) {
		return err
	}
	if found, ferr := d.exists(record.Path); ferr != nil || found {
		return err
	}
	return fmt.Errorf("%w; %s", err, sharedStorageHint(d, record))
}
