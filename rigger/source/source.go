// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Package source fetches versions of modules and stacks (design 7.1): the
// address of a source, and the port a host implements to list and fetch
// versions (git, a directory, the modules built into the program; OCI and
// bundles in phase 4).
package source

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"strings"
)

// Kind is the kind of a source.
type Kind string

// The kinds of sources.
const (
	// KindGit is a git repository: versions are tags.
	KindGit Kind = "git"
	// KindDirectory is a directory outside the stack: one version, the one
	// its metadata says.
	KindDirectory Kind = "dir"
	// KindBuiltin is a module built into the program.
	KindBuiltin Kind = "builtin"
)

// Request names a module of a source, and a version to fetch.
type Request struct {
	// Source is the address as written in a stack: "example/postgres",
	// "github.com/acme/rig-modules//nats", "builtin/redis".
	Source string `json:"source"`
	Kind   Kind   `json:"kind"`
	// Registry is the name of the registry of the stack the address names;
	// empty for another address.
	Registry string `json:"registry,omitempty"`
	// Repository is the git repository or the directory; empty for a
	// built-in module.
	Repository string `json:"repository,omitempty"`
	// Path is the directory of the module in the repository: modules/nats
	// in a registry (design 7.4), "." for a repository of one module.
	Path string `json:"path"`
	// TagPrefix is what goes before the version in a tag: "nats/v" in a
	// repository of several modules, "v" in one of one module (design 7.2).
	TagPrefix string `json:"tag_prefix,omitempty"`
	// Version, Ref and Commit name the version to fetch; Commit, when set,
	// is the commit the lock file recorded: a tag that points elsewhere now
	// is refused.
	Version string `json:"version,omitempty"`
	Ref     string `json:"ref,omitempty"`
	Commit  string `json:"commit,omitempty"`
}

// Available is a version a source has.
type Available struct {
	// Version is empty for a source of one version (a directory, a built-in
	// module): the version is the one its metadata says.
	Version string `json:"version"`
	Ref     string `json:"ref,omitempty"`
	Commit  string `json:"commit,omitempty"`
}

// Origin records where fetched content came from, so that a cached copy can
// be verified again before every use (design 12.6).
type Origin struct {
	Source  string `json:"source"`
	Version string `json:"version"`
	// Ref is the tag or manifest the version was found by.
	Ref string `json:"ref,omitempty"`
	// Commit is the commit of a git source.
	Commit string `json:"commit,omitempty"`
	// Digest is the tree digest of the content (design 8.1), set by the
	// engine.
	Digest string `json:"digest"`
}

// Fetched is a fetched version of a module.
type Fetched struct {
	// FS holds the files of the repository at the version, so that a child
	// module of the same repository (../lib/placement) is found. The
	// engine reads only from it.
	FS fs.FS
	// Dir is the directory of the module in FS.
	Dir    string
	Origin Origin
}

// Fetcher lists and fetches the versions of a source. A host implements
// it; the rig program fetches with git, from directories and from the
// modules built into it.
type Fetcher interface {
	// Versions returns the versions of the module of req.
	Versions(ctx context.Context, req Request) ([]Available, error)
	// Fetch returns the files of the version of req. It returns an error
	// matching fs.ErrNotExist when it does not have the version.
	Fetch(ctx context.Context, req Request) (Fetched, error)
}

// ErrAddress is the error of an address that names no source.
var ErrAddress = errors.New("invalid source")

// ErrGlobalRegistry is the error of an address of the global registry,
// which comes with phase 6.
var ErrGlobalRegistry = errors.New("the global registry comes with phase 6; name a registry of the stack (rig { registry \"<name>\" { source } })")

// Parse parses the address of a module that is not a local path (./x):
//
//   - builtin/<name>: a module built into the program;
//   - dir://<path>: a directory;
//   - <host>/<path>[//<dir>], https://, file:// or an absolute path: a git
//     repository, the module in <dir>;
//   - <registry>/<name>: the module modules/<name> of a registry of the
//     stack, registries giving the repository of each registry by name;
//   - <namespace>/<name> of no registry of the stack: the global registry.
func Parse(address string, registries map[string]string) (Request, error) {
	req := Request{Source: address}

	switch {
	case strings.HasPrefix(address, "builtin/"):
		name := strings.TrimPrefix(address, "builtin/")
		if !validName(name) {
			return Request{}, fmt.Errorf("%w: %q", ErrAddress, address)
		}

		req.Kind, req.Path = KindBuiltin, name

		return req, nil
	case strings.HasPrefix(address, "dir://"):
		req.Kind, req.Repository, req.Path = KindDirectory, strings.TrimPrefix(address, "dir://"), "."

		return req, nil
	}

	if repo, ok := gitRepository(address); ok {
		req.Kind, req.Repository, req.Path, req.TagPrefix = KindGit, repo.repository, repo.path, repo.tagPrefix

		return req, nil
	}

	reg, name, ok := strings.Cut(address, "/")
	if !ok || !validName(reg) || !validName(name) {
		return Request{}, fmt.Errorf("%w: %q", ErrAddress, address)
	}

	repository, ok := registries[reg]
	if !ok {
		return Request{}, fmt.Errorf("%s: %w", address, ErrGlobalRegistry)
	}

	inner, err := Parse(repository, nil)
	if err != nil || inner.Kind != KindGit {
		return Request{}, fmt.Errorf("%w: registry %s is not a git repository: %q", ErrAddress, reg, repository)
	}

	req.Kind, req.Repository, req.Path, req.TagPrefix = KindGit, inner.Repository, "modules/"+name, name+"/v"
	req.Registry = reg

	return req, nil
}

type gitAddress struct {
	repository, path, tagPrefix string
}

// gitRepository reads a git address: a URL, an absolute path, or a host
// name (with a dot) and a path; //<dir> names the module inside.
func gitRepository(address string) (gitAddress, bool) {
	scheme := ""

	for _, s := range []string{"https://", "http://", "ssh://", "file://"} {
		if strings.HasPrefix(address, s) {
			scheme = s
		}
	}

	rest := strings.TrimPrefix(address, scheme)
	host, _, _ := strings.Cut(rest, "/")

	if scheme == "" && !strings.HasPrefix(address, "/") && !strings.Contains(host, ".") {
		return gitAddress{}, false
	}

	repo, dir, found := strings.Cut(rest, "//")
	out := gitAddress{repository: scheme + repo, path: ".", tagPrefix: "v"}

	if found && dir != "" {
		out.path = strings.Trim(dir, "/")
		out.tagPrefix = out.path[strings.LastIndex(out.path, "/")+1:] + "/v"
	}

	return out, true
}

func validName(s string) bool {
	if s == "" || len(s) > 63 {
		return false
	}

	for _, c := range s {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' && c != '_' {
			return false
		}
	}

	return true
}
