// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Package source fetches versions of modules and stacks (design 7.1).
//
// It holds the port a host implements when it has a cache of its own, such
// as the registry cache of a Norca leader (design 12.2). The built-in
// fetchers (git, OCI, directory, bundle, built-in, the global registry)
// come with phase 3.
package source

import (
	"context"
	"io/fs"
)

// Request names a version to fetch.
type Request struct {
	// Source is the address as written in a stack: "norca/nats",
	// "github.com/acme/rig-modules//nats", "oci://registry.internal/rig/nats".
	Source string `json:"source"`
	// Version is the exact version, resolved from the constraint before the
	// fetch.
	Version string `json:"version"`
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
	// Digest is the tree digest of the content (design 8.1).
	Digest string `json:"digest"`
}

// Fetched is a fetched version of a module or a stack.
type Fetched struct {
	// FS holds the files of the version. The engine reads only from it.
	FS fs.FS
	// Origin records where the files came from.
	Origin Origin
}

// Fetcher fetches versions.
type Fetcher interface {
	// Fetch returns the files of a version. It returns an error matching
	// fs.ErrNotExist when it does not have the version, so that the engine
	// can try its own sources.
	Fetch(ctx context.Context, req Request) (Fetched, error)
}
