// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Package fetch is the fetcher of the rig program (source.Fetcher): git
// repositories through the git program, directories, and the modules built
// into the program. What it fetches is kept in a cache directory, a version
// of a repository by its commit (design 12.6).
package fetch

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/bacanoo-io/norca-rig/cli/builtin"
	"github.com/bacanoo-io/norca-rig/cli/internal/oci"
	"github.com/bacanoo-io/norca-rig/source"
	"github.com/bacanoo-io/norca-rig/version"
)

// Fetcher fetches modules into a cache directory.
type Fetcher struct {
	cache string
	oci   *oci.Client

	mu      sync.Mutex
	updated map[string]bool // mirrors fetched in this run
}

var _ source.Fetcher = (*Fetcher)(nil)

// New returns a fetcher with its cache in dir.
func New(dir string) *Fetcher {
	return &Fetcher{cache: dir, oci: oci.NewClient(), updated: map[string]bool{}}
}

// CacheDir is the cache of the rig program: RIG_CACHE_DIR, else rig in the
// cache directory of the user.
func CacheDir() (string, error) {
	if dir := os.Getenv("RIG_CACHE_DIR"); dir != "" {
		return dir, nil
	}

	dir, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}

	return filepath.Join(dir, "rig"), nil
}

// Versions returns the versions of the module of req.
func (f *Fetcher) Versions(ctx context.Context, req source.Request) ([]source.Available, error) {
	switch req.Kind {
	case source.KindGit:
		return f.gitVersions(ctx, req)
	case source.KindDirectory, source.KindBuiltin:
		return []source.Available{{}}, nil
	case source.KindOCI:
		return f.ociVersions(ctx, req)
	default:
		return nil, fmt.Errorf("%w: kind %q", source.ErrAddress, req.Kind)
	}
}

// Fetch returns the files of the version of req.
func (f *Fetcher) Fetch(ctx context.Context, req source.Request) (source.Fetched, error) {
	origin := source.Origin{Source: req.Source, Version: req.Version, Ref: req.Ref}

	switch req.Kind {
	case source.KindGit:
		return f.gitFetch(ctx, req)
	case source.KindOCI:
		return f.ociFetch(ctx, req)
	case source.KindDirectory:
		info, err := os.Stat(req.Repository)
		if err != nil {
			return source.Fetched{}, err
		}

		if !info.IsDir() {
			return source.Fetched{}, fmt.Errorf("%s is not a directory", req.Repository)
		}

		return source.Fetched{FS: os.DirFS(req.Repository), Dir: ".", Origin: origin}, nil
	case source.KindBuiltin:
		dir := path.Clean(req.Path)
		if _, err := fs.Stat(builtin.FS(), dir+"/metadata.hcl"); err != nil {
			return source.Fetched{}, fmt.Errorf("rig has no built-in module %s: %w", path.Base(dir), fs.ErrNotExist)
		}

		return source.Fetched{FS: builtin.FS(), Dir: dir, Origin: origin}, nil
	default:
		return source.Fetched{}, fmt.Errorf("%w: kind %q", source.ErrAddress, req.Kind)
	}
}

// repositoryURL is the URL git reads a repository by: a host and a path is
// https.
func repositoryURL(repo string) string {
	for _, s := range []string{"https://", "http://", "ssh://", "file://", "/"} {
		if strings.HasPrefix(repo, s) {
			return repo
		}
	}

	return "https://" + repo
}

// mirror returns the bare mirror of a repository in the cache, made or
// fetched once per run.
func (f *Fetcher) mirror(ctx context.Context, repo string) (string, error) {
	url := repositoryURL(repo)
	sum := sha256.Sum256([]byte(url))
	dir := filepath.Join(f.cache, "git", hex.EncodeToString(sum[:12])+".git")

	f.mu.Lock()
	defer f.mu.Unlock()

	if f.updated[dir] {
		return dir, nil
	}

	if _, err := os.Stat(dir); err == nil {
		if _, err := git(ctx, dir, "fetch", "--prune", "--prune-tags", "--tags", "--quiet", "origin"); err != nil {
			return "", err
		}
	} else {
		if err := os.MkdirAll(filepath.Dir(dir), 0o750); err != nil {
			return "", err
		}

		if _, err := git(ctx, "", "clone", "--mirror", "--quiet", url, dir); err != nil {
			return "", err
		}
	}

	f.updated[dir] = true

	return dir, nil
}

// git runs the git program, in the repository dir when it is set.
func git(ctx context.Context, dir string, args ...string) ([]byte, error) {
	if dir != "" {
		args = append([]string{"--git-dir", dir}, args...)
	}

	var stderr bytes.Buffer

	cmd := exec.CommandContext(ctx, "git", args...) //nolint:gosec // G204: the arguments are git's, the repository the stack's
	cmd.Stderr = &stderr

	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")

	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", args[len(args)-1], err, strings.TrimSpace(stderr.String()))
	}

	return out, nil
}

// gitVersions lists the tags of the module: <prefix><version>.
func (f *Fetcher) gitVersions(ctx context.Context, req source.Request) ([]source.Available, error) {
	dir, err := f.mirror(ctx, req.Repository)
	if err != nil {
		return nil, err
	}

	out, err := git(ctx, dir, "for-each-ref", "--format=%(refname:strip=2) %(objectname) %(*objectname)", "refs/tags/"+req.TagPrefix+"*")
	if err != nil {
		return nil, err
	}

	var versions []source.Available

	for line := range strings.Lines(string(out)) {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}

		commit := fields[1]
		if len(fields) > 2 {
			commit = fields[2] // the commit of an annotated tag
		}

		versions = append(versions, source.Available{
			Version: strings.TrimPrefix(fields[0], req.TagPrefix), Ref: fields[0], Commit: commit,
		})
	}

	return versions, nil
}

// ErrTagMoved is the error of a tag that points to another commit than the
// lock file recorded (design 7.2: a tag is never moved).
var ErrTagMoved = errors.New("the tag points to another commit than the lock file records")

var commitPattern = regexp.MustCompile(`^[0-9a-f]{40,64}$`)

// TreeDir is the directory of the full tree of a commit in the cache.
func (f *Fetcher) TreeDir(commit string) string {
	return filepath.Join(f.cache, "trees", commit)
}

// BundleDir is the directory of the files of a commit imported from a
// bundle: the directories of its modules only, kept apart from the full
// trees.
func (f *Fetcher) BundleDir(commit string) string {
	return filepath.Join(f.cache, "bundles", commit)
}

// cachedTree returns a tree of the commit the request locks when the
// repository can't be reached: the full tree, else the files of a bundle.
func (f *Fetcher) cachedTree(req source.Request) (source.Fetched, bool) {
	if !commitPattern.MatchString(req.Commit) {
		return source.Fetched{}, false
	}

	for _, dir := range []string{f.TreeDir(req.Commit), f.BundleDir(req.Commit)} {
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(req.Path), "metadata.hcl")); err == nil {
			return source.Fetched{
				FS: os.DirFS(dir), Dir: path.Clean(req.Path),
				Origin: source.Origin{Source: req.Source, Version: req.Version, Ref: req.Ref, Commit: req.Commit},
			}, true
		}
	}

	return source.Fetched{}, false
}

// gitFetch extracts the tree of the commit of the tag into the cache. When
// the repository can't be reached, a cached tree of the commit the lock
// file records serves (an install without a network, from a bundle).
func (f *Fetcher) gitFetch(ctx context.Context, req source.Request) (source.Fetched, error) {
	dir, err := f.mirror(ctx, req.Repository)
	if err != nil {
		if cached, ok := f.cachedTree(req); ok {
			return cached, nil
		}

		return source.Fetched{}, err
	}

	ref := req.Ref
	if ref == "" {
		ref = req.TagPrefix + req.Version
	}

	out, err := git(ctx, dir, "rev-parse", "--verify", "--quiet", "refs/tags/"+ref+"^{commit}")
	if err != nil {
		return source.Fetched{}, fmt.Errorf("%s has no tag %s: %w", req.Repository, ref, fs.ErrNotExist)
	}

	commit := strings.TrimSpace(string(out))
	if !commitPattern.MatchString(commit) {
		return source.Fetched{}, fmt.Errorf("git: %q is not a commit", commit)
	}

	if req.Commit != "" && req.Commit != commit {
		return source.Fetched{}, fmt.Errorf("%s %s: %w (%s, now %s)", req.Repository, ref, ErrTagMoved, req.Commit, commit)
	}

	tree := filepath.Join(f.cache, "trees", commit)
	if _, err := os.Stat(tree); err != nil {
		if err := f.extract(ctx, dir, commit, tree); err != nil {
			return source.Fetched{}, err
		}
	}

	return source.Fetched{
		FS: os.DirFS(tree), Dir: path.Clean(req.Path),
		Origin: source.Origin{Source: req.Source, Version: req.Version, Ref: ref, Commit: commit},
	}, nil
}

// ociVersions lists the tags of a repository that are versions.
func (f *Fetcher) ociVersions(ctx context.Context, req source.Request) ([]source.Available, error) {
	tags, err := f.oci.Tags(ctx, req.Repository)
	if err != nil {
		return nil, err
	}

	var out []source.Available

	for _, tag := range tags {
		if _, err := version.Parse(tag); err == nil {
			out = append(out, source.Available{Version: tag, Ref: tag})
		}
	}

	return out, nil
}

// OCIDir is the directory of the layer of a manifest in the cache.
func (f *Fetcher) OCIDir(manifest string) string {
	return filepath.Join(f.cache, "oci", strings.TrimPrefix(manifest, "sha256:"))
}

// ociFetch resolves the tag of a version to its manifest, refuses a
// manifest other than the one the lock file records, and extracts its
// layer into the cache. When the registry can't be reached, the cached
// layer of the locked manifest serves.
func (f *Fetcher) ociFetch(ctx context.Context, req source.Request) (source.Fetched, error) {
	tag := req.Ref
	if tag == "" {
		tag = req.Version
	}

	origin := source.Origin{Source: req.Source, Version: req.Version, Ref: tag}

	m, digest, err := f.oci.Resolve(ctx, req.Repository, tag)
	if err != nil {
		cached := f.OCIDir(req.Commit)
		if req.Commit != "" && req.Path != "" {
			if _, statErr := os.Stat(filepath.Join(cached, filepath.FromSlash(req.Path), "metadata.hcl")); statErr == nil {
				origin.Commit = req.Commit

				return source.Fetched{FS: os.DirFS(cached), Dir: req.Path, Origin: origin}, nil
			}
		}

		return source.Fetched{}, err
	}

	if req.Commit != "" && req.Commit != digest {
		return source.Fetched{}, fmt.Errorf("%s:%s: %w (%s, now %s)", req.Repository, tag, ErrTagMoved, req.Commit, digest)
	}

	dir := f.OCIDir(digest)
	if _, err := os.Stat(dir); err != nil {
		if err := f.extractLayer(ctx, req.Repository, m.Layers[0].Digest, dir); err != nil {
			return source.Fetched{}, err
		}
	}

	mod := oci.ModuleOf(m)
	if !fs.ValidPath(mod.Path) {
		return source.Fetched{}, fmt.Errorf("%s:%s: the module path %q is not a path", req.Repository, tag, mod.Path)
	}

	origin.Commit = digest

	return source.Fetched{FS: os.DirFS(dir), Dir: mod.Path, Origin: origin}, nil
}

// extractLayer writes the files of a layer into dir.
func (f *Fetcher) extractLayer(ctx context.Context, repository, digest, dir string) error {
	blob, err := f.oci.Blob(ctx, repository, digest)
	if err != nil {
		return err
	}

	gz, err := gzip.NewReader(bytes.NewReader(blob))
	if err != nil {
		return err
	}

	data, err := io.ReadAll(io.LimitReader(gz, 256<<20))
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(dir), 0o750); err != nil {
		return err
	}

	tmp, err := os.MkdirTemp(filepath.Dir(dir), "layer.tmp-")
	if err != nil {
		return err
	}

	if err := untar(data, tmp); err != nil {
		_ = os.RemoveAll(tmp)

		return err
	}

	if err := os.Rename(tmp, dir); err != nil {
		_ = os.RemoveAll(tmp)

		if _, statErr := os.Stat(dir); statErr == nil {
			return nil
		}

		return err
	}

	return nil
}

// extract writes the tree of a commit into dir, through a directory of its
// own that is renamed into place when complete.
func (f *Fetcher) extract(ctx context.Context, repo, commit, dir string) error {
	data, err := git(ctx, repo, "archive", "--format=tar", commit)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(dir), 0o750); err != nil {
		return err
	}

	tmp, err := os.MkdirTemp(filepath.Dir(dir), commit[:12]+".tmp-")
	if err != nil {
		return err
	}

	if err := untar(data, tmp); err != nil {
		_ = os.RemoveAll(tmp)

		return err
	}

	if err := os.Rename(tmp, dir); err != nil {
		_ = os.RemoveAll(tmp)

		if _, statErr := os.Stat(dir); statErr == nil {
			return nil // extracted meanwhile by another run
		}

		return err
	}

	return nil
}

// untar writes the regular files and directories of a tar archive into
// dir, through an os.Root: no entry escapes it.
func untar(data []byte, dir string) error {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close() //nolint:errcheck // the files are closed one by one

	tr := tar.NewReader(bytes.NewReader(data))

	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}

		if err != nil {
			return err
		}

		name := path.Clean(h.Name)

		switch h.Typeflag {
		case tar.TypeDir:
			if err := root.MkdirAll(name, 0o750); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := writeEntry(root, name, tr); err != nil {
				return err
			}
		default:
			// Links and other entries are not part of a module.
		}
	}
}

func writeEntry(root *os.Root, name string, r io.Reader) error {
	if err := root.MkdirAll(path.Dir(name), 0o750); err != nil {
		return err
	}

	f, err := root.OpenFile(name, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o640)
	if err != nil {
		return err
	}

	if _, err := io.Copy(f, r); err != nil {
		_ = f.Close()

		return err
	}

	return f.Close()
}
