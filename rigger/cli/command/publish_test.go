// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/bacanoo-io/norca/rigger/oci/ocitest"
)

// The directory of module publish may follow its flags, as that of module
// render: it is the directory that is published, not the current one.
func TestRunPublish_DirectoryAfterFlags(t *testing.T) {
	registry := ocitest.New()
	defer registry.Close()

	repo := greeterRepo(t, "1.0.0")
	keys := publishEnv(t)
	module := filepath.Join(repo, "modules", "greeter")
	to := "oci://" + registry.Host() + "/rigger/greeter"

	// The current directory is another module: a publish that does not
	// read its argument publishes this one.
	t.Chdir(writeTree(t, map[string]string{metadataFile: "module {\n  name    = \"other\"\n  version = \"9.9.9\"\n}\n"}))

	riggerRun(t, exitOK, []string{"published greeter 1.0.0 to " + to}, "module", "publish", "-to", to, module)
	riggerRun(t, exitOK, []string{taggedGreeter}, "module", "publish", "-key", filepath.Join(keys, "acme.key"), module)
}

func TestRunPublish_OneDirectory(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		args []string
	}{
		{name: "two directories", args: []string{"a", "b"}},
		{name: "a directory before and after the flags", args: []string{"a", "-to", "oci://localhost/x/y", "b"}},
		{name: "the current directory before the flags, another after", args: []string{".", "-key", "k.key", "b"}},
		{name: "two directories after the flags", args: []string{"-key", "k.key", "a", "b"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var stdout, stderr strings.Builder

			code := Run(t.Context(), append([]string{cmdModule, cmdPublish}, tt.args...), &stdout, &stderr)
			if code != exitError || !strings.Contains(stderr.String(), "module publish takes one directory") {
				t.Errorf("Run() = %d, stderr %q, want the refusal of a second directory", code, stderr.String())
			}
		})
	}
}
