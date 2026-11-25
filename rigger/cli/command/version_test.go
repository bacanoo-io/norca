// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"encoding/json"
	"runtime"
	"runtime/debug"
	"strings"
	"testing"
)

// The commit and the version the linker sets in the tests.
const (
	linkedCommit  = "fedcba"
	linkedVersion = "v0.4.0"
)

// The build is what the linker set (the Makefile), else what the go
// command recorded.
func TestReadBuild(t *testing.T) {
	t.Parallel()

	recorded := &debug.BuildInfo{
		Main: debug.Module{Path: "github.com/bacanoo-io/norca/rigger/cli", Version: "v0.4.1-0.20261125100000-0123456789ab"},
		Settings: []debug.BuildSetting{
			{Key: "vcs.revision", Value: "0123456789abcdef"},
			{Key: "vcs.time", Value: commitDate},
			{Key: "vcs.modified", Value: "true"},
		},
	}

	tests := []struct {
		name            string
		version         string
		linked          Build
		info            *debug.BuildInfo
		expectedVersion string
		expected        Build
	}{
		{
			name:    "linked",
			version: linkedVersion, linked: Build{Commit: linkedCommit, Date: "2026-11-24T09:00:00Z"}, info: &debug.BuildInfo{},
			expectedVersion: linkedVersion, expected: Build{Commit: linkedCommit, Date: "2026-11-24T09:00:00Z"},
		},
		{
			name: "recorded", info: recorded,
			expectedVersion: "v0.4.1-0.20261125100000-0123456789ab",
			expected:        Build{Commit: "0123456789abcdef", Date: commitDate, Modified: true},
		},
		{
			name: "linked over recorded", version: linkedVersion, linked: Build{Commit: linkedCommit}, info: recorded,
			expectedVersion: linkedVersion, expected: Build{Commit: linkedCommit, Date: commitDate, Modified: true},
		},
		{name: "a tree without version control", info: &debug.BuildInfo{Main: debug.Module{Version: develVersion}}, expectedVersion: develVersion},
		{name: "no build information", expectedVersion: develVersion},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			version, build := readBuild(tt.version, tt.linked, tt.info)
			if version != tt.expectedVersion || build != tt.expected {
				t.Errorf("readBuild() = %q, %+v; want %q, %+v", version, build, tt.expectedVersion, tt.expected)
			}
		})
	}
}

// rigger version, --version and -version print the version and the build;
// the first line is the one rigger version always wrote.
func TestRun_Version(t *testing.T) {
	t.Parallel()

	platform := runtime.GOOS + "/" + runtime.GOARCH

	tests := []struct {
		name     string
		prog     Program
		args     []string
		expected string
	}{
		{
			name: "version", prog: riggerProgram, args: []string{cmdVersion},
			expected: "rigger v9.9.9\ngo:       " + runtime.Version() + "\nplatform: " + platform + "\n",
		},
		{
			name: optVersionLong, prog: riggerProgram, args: []string{optVersionLong},
			expected: "rigger v9.9.9\ngo:       " + runtime.Version() + "\nplatform: " + platform + "\n",
		},
		{
			name: flagVersion, prog: riggerProgram, args: []string{flagVersion},
			expected: "rigger v9.9.9\ngo:       " + runtime.Version() + "\nplatform: " + platform + "\n",
		},
		{
			name: "a modified build", prog: Program{Name: riggerProgram.Name, Version: develVersion, Build: Build{Commit: "0123abc", Date: commitDate, Modified: true}},
			args: []string{cmdVersion},
			expected: "rigger (devel)\ncommit:   0123abc (modified)\ndate:     " + commitDate + "\ngo:       " + runtime.Version() +
				"\nplatform: " + platform + "\n",
		},
		{
			name: "built into norca", prog: norcaProgram, args: []string{cmdVersion},
			expected: "rigger (built into Norca v1.2.3)\ncommit:   abc123\ndate:     " + commitDate + "\ngo:       " + runtime.Version() +
				"\nplatform: " + platform + "\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			code, stdout, stderr := runProgram(t, tt.prog, tt.args...)
			if code != exitOK || stderr != "" {
				t.Fatalf("exit code %d, stderr %q", code, stderr)
			}

			if stdout != tt.expected {
				t.Errorf("stdout = %q, want %q", stdout, tt.expected)
			}
		})
	}
}

// rigger version --json writes the version and the build as JSON.
func TestRun_VersionJSON(t *testing.T) {
	t.Parallel()

	code, stdout, stderr := runProgram(t, norcaProgram, cmdVersion, "-json")
	if code != exitOK {
		t.Fatalf("exit code %d, stderr %q", code, stderr)
	}

	var got shownVersion
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, stdout)
	}

	want := shownVersion{
		Program: norcaProgram.Name, Version: norcaProgram.Version, Commit: "abc123", Date: commitDate,
		GoVersion: runtime.Version(), Platform: runtime.GOOS + "/" + runtime.GOARCH,
	}
	if got != want {
		t.Errorf("version --json = %+v, want %+v", got, want)
	}

	if !strings.Contains(stdout, `"go": "`) {
		t.Errorf("the Go version is not named go:\n%s", stdout)
	}
}
