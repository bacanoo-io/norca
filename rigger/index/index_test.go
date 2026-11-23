// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package index_test

import (
	"errors"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/bacanoo-io/norca/rigger/index"
)

const (
	nats   = "nats"
	norca  = "norca"
	commit = "0123456789abcdef0123456789abcdef01234567"
	digest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	// first is the first version of the entries and advisories of the tests.
	first = "1.0.0"
)

func version(v string) index.Version {
	return index.Version{
		Version: v, Repository: "github.com/acme/rigger", Path: "modules/nats", Tag: "nats/v" + v, Commit: commit,
		Digest: digest, Signatures: []index.Signature{{KeyID: digest, Signature: "MEUCIQ=="}}, Listed: "2026-11-18",
	}
}

func TestEntryPath(t *testing.T) {
	t.Parallel()

	if got := index.EntryPath(index.KindModule, norca, nats); got != "modules/n/norca/nats.json" {
		t.Fatalf("EntryPath = %s", got)
	}

	if got := index.EntryPath(index.KindStack, "acme", "data"); got != "stacks/a/acme/data.json" {
		t.Fatalf("EntryPath = %s", got)
	}
}

func TestReadEntry(t *testing.T) {
	t.Parallel()

	e := index.Entry{Namespace: norca, Name: nats, Versions: []index.Version{version("1.0.0"), version("1.1.0")}}

	data, err := index.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}

	fsys := fstest.MapFS{"modules/n/norca/nats.json": {Data: data}}

	got, err := index.ReadEntry(fsys, index.KindModule, norca, nats)
	if err != nil || got.Version("1.1.0") == nil {
		t.Fatalf("ReadEntry = %v, %v", got, err)
	}

	if _, err := index.ReadEntry(fsys, index.KindModule, norca, "redis"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("ReadEntry of none = %v", err)
	}

	if _, err := index.ReadEntry(fsys, index.KindStack, norca, nats); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("ReadEntry of a stack = %v", err)
	}
}

// A file of the index is one JSON value. What follows it was not read at
// all: a second entry, or text that is no JSON, passed as a valid file.
func TestReadEntry_TrailingData(t *testing.T) {
	t.Parallel()

	entry, err := index.Marshal(index.Entry{Namespace: norca, Name: nats, Versions: []index.Version{version(first)}})
	if err != nil {
		t.Fatal(err)
	}

	const (
		entryPath     = "modules/n/norca/nats.json"
		namespacePath = "namespaces/norca.json"
		namespace     = `{"name": "norca", "owner": "bacanoo-io", "repositories": []}`
		advisories    = `{"advisories": []}`
	)

	read := map[string]func(fsys fstest.MapFS) error{
		entryPath: func(fsys fstest.MapFS) error {
			_, err := index.ReadEntry(fsys, index.KindModule, norca, nats)

			return err
		},
		namespacePath: func(fsys fstest.MapFS) error {
			_, err := index.ReadNamespace(fsys, norca)

			return err
		},
		index.AdvisoriesFile: func(fsys fstest.MapFS) error {
			_, err := index.ReadAdvisories(fsys)

			return err
		},
	}

	tests := []struct {
		name    string
		path    string
		data    string
		invalid bool
	}{
		{name: "an entry", path: entryPath, data: string(entry)},
		{name: "an entry and white space", path: entryPath, data: string(entry) + " \n\t\r\n"},
		{name: "an entry and text that is no JSON", path: entryPath, data: string(entry) + "{garbage", invalid: true},
		{name: "an entry and a second entry", path: entryPath, data: string(entry) + string(entry), invalid: true},
		{name: "an entry and a closing brace", path: entryPath, data: string(entry) + "}", invalid: true},
		{name: "an entry and null", path: entryPath, data: string(entry) + "null", invalid: true},
		{name: "a namespace", path: namespacePath, data: namespace + "\n"},
		{name: "a namespace and a second value", path: namespacePath, data: namespace + "\n" + namespace, invalid: true},
		{name: "advisories", path: index.AdvisoriesFile, data: advisories + "\n"},
		{name: "advisories and a second value", path: index.AdvisoriesFile, data: advisories + "\n[]", invalid: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := read[tt.path](fstest.MapFS{tt.path: {Data: []byte(tt.data)}})

			switch {
			case !tt.invalid && err != nil:
				t.Errorf("read = %v, want no error", err)
			case tt.invalid && !errors.Is(err, index.ErrInvalid):
				t.Errorf("read = %v, want an invalid index file", err)
			case tt.invalid && !strings.Contains(err.Error(), "after the JSON value"):
				t.Errorf("read = %v, want it to name the data after the JSON value", err)
			}
		})
	}
}

func TestEntry_Validate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		change   func(e *index.Entry)
		expected string
	}{
		{name: "versions out of order", change: func(e *index.Entry) { e.Versions[0], e.Versions[1] = e.Versions[1], e.Versions[0] }, expected: "is not after"},
		{name: "no signature", change: func(e *index.Entry) { e.Versions[0].Signatures = nil }, expected: "is signed"},
		{name: "short commit", change: func(e *index.Entry) { e.Versions[0].Commit = "0123" }, expected: "commit"},
		{name: "path out of the repository", change: func(e *index.Entry) { e.Versions[0].Path = "../x" }, expected: "path"},
		{name: "a module pinning modules", change: func(e *index.Entry) {
			e.Versions[0].Modules = []index.LockedModule{{Source: "norca/x", Version: "1.0.0", Digest: digest}}
		}, expected: "pins no modules"},
		{name: "an invalid name", change: func(e *index.Entry) { e.Name = "Nats" }, expected: "is not a namespace and a name"},
		{name: "a category of no list", change: func(e *index.Entry) { e.Versions[0].Category = "databases" }, expected: "category"},
		{name: "a version with a leading v", change: func(e *index.Entry) { e.Versions[1].Version = "v1.1.0" }, expected: `"v1.1.0" is written 1.1.0 in the index`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			e := &index.Entry{Namespace: norca, Name: nats, Versions: []index.Version{version("1.0.0"), version("1.1.0")}}
			tt.change(e)

			err := e.Validate(index.KindModule)
			if err == nil || !strings.Contains(err.Error(), tt.expected) {
				t.Fatalf("Validate = %v, want %q", err, tt.expected)
			}

			if !errors.Is(err, index.ErrInvalid) {
				t.Errorf("Validate = %v, want it to be ErrInvalid", err)
			}
		})
	}
}

// An advisory names its version as the entries write it, so that it is
// found by the version it is on.
func TestAdvisories_Validate(t *testing.T) {
	t.Parallel()

	advisory := func(v string) *index.Advisories {
		return &index.Advisories{Advisories: []index.Advisory{{
			Kind: index.KindModule, Namespace: norca, Name: nats, Version: v, Level: index.LevelBlocked,
			Reason: "a secret", Decision: "https://example.org/1",
		}}}
	}

	tests := []struct {
		name     string
		version  string
		expected string
	}{
		{name: "a version", version: first},
		{name: "a pre-release with build metadata", version: "1.0.0-rc.1+build.5"},
		{name: "a leading v", version: "v1.0.0", expected: `"v1.0.0" is written 1.0.0 in the index`},
		{name: "not a version", version: "1.0", expected: `advisory of version "1.0"`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := advisory(tt.version).Validate()
			if tt.expected == "" {
				if err != nil {
					t.Fatalf("Validate = %v, want no error", err)
				}

				return
			}

			if !errors.Is(err, index.ErrInvalid) || !strings.Contains(err.Error(), tt.expected) {
				t.Fatalf("Validate = %v, want an invalid index file with %q", err, tt.expected)
			}
		})
	}
}

func TestCheckAppendOnly(t *testing.T) {
	t.Parallel()

	prev := &index.Entry{Namespace: norca, Name: nats, Versions: []index.Version{version("1.0.0")}}

	added := &index.Entry{Namespace: norca, Name: nats, Versions: []index.Version{version("1.0.0"), version("1.1.0")}}
	if err := index.CheckAppendOnly(prev, added); err != nil {
		t.Fatalf("a new version: %v", err)
	}

	if err := index.CheckAppendOnly(prev, &index.Entry{Namespace: norca, Name: nats}); !errors.Is(err, index.ErrChanged) {
		t.Fatalf("a removed version: %v", err)
	}

	changed := version("1.0.0")
	changed.Digest = strings.Replace(digest, "0", "1", 1)

	if err := index.CheckAppendOnly(prev, &index.Entry{Namespace: norca, Name: nats, Versions: []index.Version{changed}}); !errors.Is(err, index.ErrChanged) {
		t.Fatalf("a changed version: %v", err)
	}
}

func TestAdvisories(t *testing.T) {
	t.Parallel()

	fsys := fstest.MapFS{index.AdvisoriesFile: {Data: []byte(`{"advisories": [
  {"kind": "module", "namespace": "norca", "name": "nats", "version": "1.0.0", "level": "warning", "reason": "slow"},
  {"kind": "module", "namespace": "norca", "name": "nats", "version": "1.1.0", "level": "blocked", "reason": "a secret", "decision": "https://example.org/1"}
]}`)}}

	a, err := index.ReadAdvisories(fsys)
	if err != nil {
		t.Fatal(err)
	}

	if _, blocked := a.Blocked(index.KindModule, norca, nats, "1.0.0"); blocked {
		t.Fatal("a warning blocks")
	}

	if adv, blocked := a.Blocked(index.KindModule, norca, nats, "1.1.0"); !blocked || adv.Reason != "a secret" {
		t.Fatalf("Blocked = %v, %v", adv, blocked)
	}

	if err := index.CheckAdvisoriesAppendOnly(a, &index.Advisories{Advisories: a.Advisories[:1]}); !errors.Is(err, index.ErrChanged) {
		t.Fatalf("a removed advisory: %v", err)
	}

	bad := fstest.MapFS{index.AdvisoriesFile: {Data: []byte(`{"advisories": [{"kind": "module", "namespace": "norca", "name": "nats", "version": "1.0.0", "level": "blocked", "reason": "x"}]}`)}}
	if _, err := index.ReadAdvisories(bad); !errors.Is(err, index.ErrInvalid) {
		t.Fatalf("a block without a decision: %v", err)
	}

	if a, err := index.ReadAdvisories(fstest.MapFS{}); err != nil || len(a.Advisories) != 0 {
		t.Fatalf("no file: %v, %v", a, err)
	}
}

// An advisory is on a version, not on a way of writing it: the block of
// 1.0.0 holds for v1.0.0 and for 1.0.0 with build metadata, which are the
// same version in the order of versions.
func TestAdvisories_For(t *testing.T) {
	t.Parallel()

	block := func(name, v string) index.Advisory {
		return index.Advisory{
			Kind: index.KindModule, Namespace: norca, Name: name, Version: v, Level: index.LevelBlocked,
			Reason: "a secret", Decision: "https://example.org/1",
		}
	}

	a := &index.Advisories{Advisories: []index.Advisory{
		block(nats, "v1.0.0"), block(nats, "1.1.0"), block(nats, "2.0.0-rc.1"), block("redis", "3.0.0"), block(nats, "main"),
	}}

	tests := []struct {
		name    string
		kind    index.Kind
		module  string
		version string
		blocked bool
	}{
		{name: "the advisory has a leading v", kind: index.KindModule, module: nats, version: first, blocked: true},
		{name: "the version has a leading v", kind: index.KindModule, module: nats, version: "v1.1.0", blocked: true},
		{name: "both written the same way", kind: index.KindModule, module: nats, version: "1.1.0", blocked: true},
		{name: "the version has build metadata", kind: index.KindModule, module: nats, version: "1.1.0+build.5", blocked: true},
		{name: "a pre-release", kind: index.KindModule, module: nats, version: "2.0.0-rc.1", blocked: true},
		{name: "the release of a blocked pre-release", kind: index.KindModule, module: nats, version: "2.0.0"},
		{name: "another pre-release", kind: index.KindModule, module: nats, version: "2.0.0-rc.2"},
		{name: "another version", kind: index.KindModule, module: nats, version: "1.0.1"},
		{name: "another module", kind: index.KindModule, module: "redis", version: first},
		{name: "another kind", kind: index.KindStack, module: nats, version: first},
		{name: "a text that is no version, written the same", kind: index.KindModule, module: nats, version: "main", blocked: true},
		{name: "a text that is no version", kind: index.KindModule, module: nats, version: "1.0"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if _, blocked := a.Blocked(tt.kind, norca, tt.module, tt.version); blocked != tt.blocked {
				t.Errorf("Blocked(%s %s) = %v, want %v", tt.module, tt.version, blocked, tt.blocked)
			}

			if got := len(a.For(tt.kind, norca, tt.module, tt.version)) == 1; got != tt.blocked {
				t.Errorf("For(%s %s) found an advisory: %v, want %v", tt.module, tt.version, got, tt.blocked)
			}
		})
	}
}
