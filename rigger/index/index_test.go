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
