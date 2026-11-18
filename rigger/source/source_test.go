// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package source_test

import (
	"errors"
	"testing"

	"github.com/bacanoo-io/norca-rig/source"
)

func TestParse(t *testing.T) {
	t.Parallel()

	registries := map[string]string{"example": "github.com/example/rig-registry", "local": "file:///srv/registry"}

	tests := []struct {
		address  string
		expected source.Request
		err      error
	}{
		{address: "example/postgres", expected: source.Request{Kind: source.KindGit, Registry: "example", Repository: "github.com/example/rig-registry", Path: "modules/postgres", TagPrefix: "postgres/v"}},
		{address: "local/nats", expected: source.Request{Kind: source.KindGit, Registry: "local", Repository: "file:///srv/registry", Path: "modules/nats", TagPrefix: "nats/v"}},
		{address: "github.com/acme/rig-modules//nats", expected: source.Request{Kind: source.KindGit, Repository: "github.com/acme/rig-modules", Path: "nats", TagPrefix: "nats/v"}},
		{address: "github.com/acme/redis", expected: source.Request{Kind: source.KindGit, Repository: "github.com/acme/redis", Path: ".", TagPrefix: "v"}},
		{address: "https://git.internal/rig.git//modules/db", expected: source.Request{Kind: source.KindGit, Repository: "https://git.internal/rig.git", Path: "modules/db", TagPrefix: "db/v"}},
		{address: "builtin/redis", expected: source.Request{Kind: source.KindBuiltin, Path: "redis"}},
		{address: "dir:///opt/rig/worker", expected: source.Request{Kind: source.KindDirectory, Repository: "/opt/rig/worker", Path: "."}},
		{address: "oci://registry.internal/rig/nats", expected: source.Request{Kind: source.KindOCI, Repository: "registry.internal/rig/nats"}},
		{address: "oci://registry.internal", err: source.ErrAddress},
		{address: "norca/nats", err: source.ErrGlobalRegistry},
		{address: "Example/x", err: source.ErrAddress},
	}

	for _, tt := range tests {
		got, err := source.Parse(tt.address, registries)
		if !errors.Is(err, tt.err) {
			t.Fatalf("Parse(%q) error = %v, want %v", tt.address, err, tt.err)
		}

		tt.expected.Source = tt.address
		if tt.err == nil && got != tt.expected {
			t.Errorf("Parse(%q) = %+v, want %+v", tt.address, got, tt.expected)
		}
	}
}
