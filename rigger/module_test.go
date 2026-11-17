// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package rig_test

import (
	"bytes"
	"image"
	"image/png"
	"log/slog"
	"os"
	"strings"
	"testing"
	"testing/fstest"

	rig "github.com/bacanoo-io/norca-rig"
)

func engine(t *testing.T) *rig.Engine {
	t.Helper()

	e, err := rig.New(rig.WithLogger(slog.New(slog.DiscardHandler)))
	if err != nil {
		t.Fatal(err)
	}

	return e
}

func TestEngine_LoadModule(t *testing.T) {
	t.Parallel()

	m, diags := engine(t).LoadModule(t.Context(), os.DirFS("examples/registry/modules/redis"))
	if diags.HasErrors() {
		t.Fatal(diags)
	}

	if md := m.Metadata(); md.Name != "redis" || md.Version != "0.3.0" || md.License != "BSD-3-Clause" {
		t.Errorf("metadata = %+v", md)
	}

	vars := m.Variables()
	if len(vars) != 15 || vars[0].Name != "job_name" || vars[0].Required {
		t.Errorf("variables = %+v", vars)
	}

	for _, v := range vars {
		if v.Name == "redis_password" && !v.Sensitive {
			t.Error("redis_password is not sensitive")
		}
	}
}

// Every module of examples/ validates: its icon and its example values.
func TestEngine_ValidateModule_Examples(t *testing.T) {
	t.Parallel()

	for _, dir := range []string{
		"module/web", "registry/modules/redis", "registry/modules/nats", "registry/modules/postgres",
		"registry/modules/lib/placement",
	} {
		t.Run(dir, func(t *testing.T) {
			t.Parallel()

			e := engine(t)

			m, diags := e.LoadModule(t.Context(), os.DirFS("examples/"+dir))
			if diags.HasErrors() {
				t.Fatal(diags)
			}

			if diags := e.ValidateModule(t.Context(), m); diags.HasErrors() {
				t.Error(diags)
			}
		})
	}
}

func icon(t *testing.T, w, h int) []byte {
	t.Helper()

	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewGray(image.Rect(0, 0, w, h))); err != nil {
		t.Fatal(err)
	}

	return buf.Bytes()
}

func TestEngine_ValidateModule_Icon(t *testing.T) {
	t.Parallel()

	const metadata = "module {\n  name = \"app\"\n  version = \"1.0.0\"\n}\n"

	tests := []struct {
		name     string
		icon     []byte
		expected string
	}{
		{name: "64 by 64", icon: icon(t, 64, 64)},
		{name: "another size", icon: icon(t, 128, 64), expected: "is 128x64 pixels, not 64x64"},
		{name: "not a PNG", icon: []byte("<svg/>"), expected: "is not a PNG"},
		{name: "too large", icon: make([]byte, 33<<10), expected: "more than 32768"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			e := engine(t)

			m, diags := e.LoadModule(t.Context(), fstest.MapFS{
				"metadata.hcl": {Data: []byte(metadata)},
				"icon.png":     {Data: tt.icon},
			})
			if diags.HasErrors() {
				t.Fatal(diags)
			}

			diags = e.ValidateModule(t.Context(), m)

			if tt.expected == "" {
				if diags.HasErrors() {
					t.Error(diags)
				}

				return
			}

			if !strings.Contains(diags.Error(), tt.expected) {
				t.Errorf("diagnostics = %q, want them to contain %q", diags.Error(), tt.expected)
			}
		})
	}
}
