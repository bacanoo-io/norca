// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package lang_test

import (
	"bytes"
	"reflect"
	"strings"
	"testing"

	"github.com/bacanoo-io/norca/rigger/internal/lang"
)

// A lock file that was written is read again as the same lock, and written
// again as the same bytes. A module may have no version (design 6.4: a
// module of a directory whose metadata leaves it out), and its entry is as
// readable as any other.
func TestLock_RoundTrip(t *testing.T) {
	t.Parallel()

	full := &lang.LockedModule{
		Source: "norca/redis", Version: "0.3.0", Constraint: "~> 0.3",
		Repository: "github.com/example/registry", Path: "modules/redis", Ref: "redis/v0.3.0",
		Commit: "4c1e0b7a9d2f6e3855a1c0d94b7f2e6a8c3d5f10", Digest: "sha256:aa",
		Signers: []string{"sha256:11", "sha256:22"},
	}

	tests := []struct {
		name string
		lock *lang.Lock
	}{
		{name: "an empty lock", lock: &lang.Lock{}},
		{name: "a module with every field", lock: &lang.Lock{Modules: []*lang.LockedModule{full}}},
		{
			name: "a module without a version",
			lock: &lang.Lock{Modules: []*lang.LockedModule{{Source: "dir:///opt/x", Path: ".", Digest: "sha256:aa"}}},
		},
		{
			name: "modules and stacks",
			lock: &lang.Lock{
				Modules: []*lang.LockedModule{{Source: "dir:///opt/x", Digest: "sha256:aa"}, full},
				Stacks:  []*lang.LockedModule{{Source: "norca/data-platform", Version: "1.2.0", Digest: "sha256:bb"}},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			written := tt.lock.Bytes()

			read, diags := lang.ParseLock(written)
			if diags.HasErrors() {
				t.Fatalf("ParseLock() of the written lock: %s\n%s", diags.Error(), written)
			}

			for _, m := range tt.lock.Modules {
				if got := read.Module(m.Source); !reflect.DeepEqual(got, m) {
					t.Errorf("Module(%q) = %+v, want %+v", m.Source, got, m)
				}
			}

			for _, s := range tt.lock.Stacks {
				if got := read.Stack(s.Source); !reflect.DeepEqual(got, s) {
					t.Errorf("Stack(%q) = %+v, want %+v", s.Source, got, s)
				}
			}

			if len(read.Modules) != len(tt.lock.Modules) || len(read.Stacks) != len(tt.lock.Stacks) {
				t.Errorf("read %d modules and %d stacks, want %d and %d",
					len(read.Modules), len(read.Stacks), len(tt.lock.Modules), len(tt.lock.Stacks))
			}

			if again := read.Bytes(); !bytes.Equal(again, written) {
				t.Errorf("written again:\n%s\nwant:\n%s", again, written)
			}
		})
	}
}

// What an entry must have is still asked for: a lock without the digest of
// a module pins nothing.
func TestParseLock_Errors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		src      string
		expected string
	}{
		{
			name:     "no digest",
			src:      "module \"norca/redis\" {\n  version = \"0.3.0\"\n}\n",
			expected: `The argument "digest" is required`,
		},
		{
			name:     "an attribute that is not of a lock",
			src:      "module \"norca/redis\" {\n  digest = \"sha256:aa\"\n  checksum = \"x\"\n}\n",
			expected: `An argument named "checksum" is not expected here`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, diags := lang.ParseLock([]byte(tt.src))
			if !diags.HasErrors() {
				t.Fatal("ParseLock() gave no error")
			}

			var all strings.Builder
			for _, d := range diags {
				all.WriteString(d.Error() + "; ")
			}

			if got := all.String(); !strings.Contains(got, tt.expected) {
				t.Errorf("diagnostics = %q, want them to contain %q", got, tt.expected)
			}
		})
	}
}
