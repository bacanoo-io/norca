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
	"path/filepath"
	"regexp"
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
	// KindOCI is a repository of an OCI registry: versions are tags, the
	// manifest digest of a tag stands for its commit.
	KindOCI Kind = "oci"
	// KindGlobal is a module of the global registry (design 7.3): its
	// entry names the versions, and for each the git repository, tag and
	// commit of its author, the tree digest and the signatures.
	KindGlobal Kind = "global"
)

// Request names a module of a source, and a version to fetch.
type Request struct {
	// Source is the address as written in a stack: "example/postgres",
	// "github.com/acme/rigger-modules//nats", "builtin/redis".
	Source string `json:"source"`
	Kind   Kind   `json:"kind"`
	// Registry is the name of the registry of the stack the address names;
	// empty for another address.
	Registry string `json:"registry,omitempty"`
	// Namespace and Name a module or stack of the global registry.
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name,omitempty"`
	// Stack is set for a published stack (design 7.5): the stacks of the
	// global registry, stacks/<name> of a registry repository.
	Stack bool `json:"stack,omitempty"`
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
	// Digest, when set, is the tree digest the lock file recorded: a host
	// may allow content by it.
	Digest string `json:"digest,omitempty"`
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
	// Repository is the git repository the version came from when the
	// address does not name it (the global registry).
	Repository string `json:"repository,omitempty"`
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
	// Listing is what the index of a registry records of the version (the
	// global registry); nil for a source without an index.
	Listing *Listing
	// TrustedKeys are the host's own rules for the source: the engine takes
	// the content only signed by one of the keys (PEM) of each, whatever
	// the stack trusts.
	TrustedKeys [][]string
}

// Listing is what the index of a registry records of a version of a module
// (design 7.3). The engine refuses content of another digest or name, and
// takes a version only signed by a key of the namespace.
type Listing struct {
	// Name is the name the module's metadata must have.
	Name string
	// Digest is the tree digest the index accepted.
	Digest string
	// Signatures are the signatures of the statement of the version
	// (sign.Statement).
	Signatures []ListedSignature
	// Keys are the public keys (PEM) of the namespace.
	Keys [][]byte
	// Warnings are the advisories of the version that do not block it.
	Warnings []string
}

// ListedSignature is a signature an index records: the key and the
// signature, without the statement, which the engine builds again.
type ListedSignature struct {
	KeyID     string
	Signature string
}

// Fetcher lists and fetches the versions of a source. A host implements
// it; the rigger program fetches with git, from directories and from the
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

// Parse parses the address of a module that is not a local path (./x):
//
//   - builtin/<name>: a module built into the program;
//   - dir://<path>: a directory;
//   - oci://<host>/<repository>: a repository of an OCI registry, tagged
//     by version;
//   - <host>/<path>[//<dir>], https://, file:// or an absolute path: a git
//     repository, the module in <dir>;
//   - <registry>/<name>: the module modules/<name> of a registry of the
//     stack, registries giving the repository of each registry by name;
//   - <namespace>/<name> of no registry of the stack: the global registry.
//
// An address that carries credentials is refused (ErrCredentials).
func Parse(address string, registries map[string]string) (Request, error) {
	if req, ok, err := parseScheme(address); ok {
		return req, err
	}

	req := Request{Source: address}

	if repo, ok := gitRepository(address); ok {
		if err := CheckRepository(repo.repository); err != nil {
			return Request{}, err
		}

		req.Kind, req.Repository, req.Path, req.TagPrefix = KindGit, repo.repository, repo.path, repo.tagPrefix

		return req, nil
	}

	return parseRegistry(address, registries)
}

// parseScheme parses the addresses with a prefix of their own: builtin/,
// dir://, oci://.
func parseScheme(address string) (Request, bool, error) {
	req := Request{Source: address}

	switch {
	case strings.HasPrefix(address, "builtin/"):
		name := strings.TrimPrefix(address, "builtin/")
		if !validName(name) {
			return Request{}, true, fmt.Errorf("%w: %q", ErrAddress, Redact(address))
		}

		req.Kind, req.Path = KindBuiltin, "modules/"+name

		return req, true, nil
	case strings.HasPrefix(address, "dir://"):
		req.Kind, req.Repository, req.Path = KindDirectory, strings.TrimPrefix(address, "dir://"), "."

		return req, true, nil
	case strings.HasPrefix(address, "oci://"):
		if err := CheckRepository(address); err != nil {
			return Request{}, true, err
		}

		repo := strings.TrimPrefix(address, "oci://")
		if host, name, ok := strings.Cut(repo, "/"); !ok || host == "" || name == "" {
			return Request{}, true, fmt.Errorf("%w: %q is not oci://<host>/<repository>", ErrAddress, address)
		}

		req.Kind, req.Repository = KindOCI, repo

		return req, true, nil
	default:
		return Request{}, false, nil
	}
}

// parseRegistry parses <registry>/<name>, a module of a registry of the
// stack, else <namespace>/<name> of the global registry.
func parseRegistry(address string, registries map[string]string) (Request, error) {
	req := Request{Source: address}

	reg, name, ok := strings.Cut(address, "/")
	if !ok || !validName(reg) || !validName(name) {
		return Request{}, fmt.Errorf("%w: %q", ErrAddress, Redact(address))
	}

	repository, ok := registries[reg]
	if !ok {
		req.Kind, req.Namespace, req.Name, req.Path = KindGlobal, reg, name, "."

		return req, nil
	}

	inner, err := Parse(repository, nil)
	if errors.Is(err, ErrCredentials) {
		return Request{}, fmt.Errorf("registry %s: %w", reg, err)
	}

	if err != nil || inner.Kind != KindGit {
		return Request{}, fmt.Errorf("%w: registry %s is not a git repository: %q", ErrAddress, reg, Redact(repository))
	}

	req.Kind, req.Repository, req.Path, req.TagPrefix = KindGit, inner.Repository, "modules/"+name, name+"/v"
	req.Registry = reg

	return req, nil
}

// scpAddress is user@host:path, with no // after the colon (that is a URL).
var scpAddress = regexp.MustCompile(`^[A-Za-z0-9._-]+@[A-Za-z0-9.-]+:/?[^/\\]`)

// IsSCPAddress reports whether a repository is written as git writes one
// for ssh, user@host:path. It stays as written: its path is of the home of
// the user, which an ssh URL doesn't say.
func IsSCPAddress(repository string) bool { return scpAddress.MatchString(repository) }

type gitAddress struct {
	repository, path, tagPrefix string
}

// gitRepository reads a git address: a URL, an absolute path (on Windows a
// path of a drive too), or a host name (with a dot) and a path; //<dir>
// names the module inside.
func gitRepository(address string) (gitAddress, bool) {
	scheme := ""

	for _, s := range []string{"https://", "http://", "ssh://", "file://"} {
		if strings.HasPrefix(address, s) {
			scheme = s
		}
	}

	rest := strings.TrimPrefix(address, scheme)
	host, _, _ := strings.Cut(rest, "/")

	// An scp-like address names a host as it is, with a dot or without.
	named := strings.HasPrefix(address, "/") || filepath.IsAbs(address) || IsSCPAddress(address) || strings.Contains(host, ".")

	if scheme == "" && !named {
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
