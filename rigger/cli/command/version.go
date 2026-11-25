// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"encoding/json"
	"fmt"
	"io"
	"runtime"
	"runtime/debug"
	"strings"

	"github.com/spf13/cobra"
)

// The build of the rigger program, set by the linker (the target build of
// the Makefile):
//
//	go build -ldflags "-X github.com/bacanoo-io/norca/rigger/cli/command.buildVersion=v0.4.0
//	  -X github.com/bacanoo-io/norca/rigger/cli/command.buildCommit=<commit>
//	  -X github.com/bacanoo-io/norca/rigger/cli/command.buildDate=<RFC 3339>"
//
// Those not set are read from what the go command recorded (programBuild).
var (
	buildVersion string
	buildCommit  string
	buildDate    string
)

// Build is the build of a program, which rigger version prints under its
// version.
type Build struct {
	// Commit is the git commit the program was built from.
	Commit string
	// Date is the time of that commit, RFC 3339.
	Date string
	// Modified reports a build of a tree with changes not committed.
	Modified bool
}

// programBuild returns the version and the build of the rigger program.
func programBuild() (string, Build) {
	info, _ := debug.ReadBuildInfo()

	return readBuild(buildVersion, Build{Commit: buildCommit, Date: buildDate}, info)
}

// readBuild returns the version and the build of a program: what the
// linker set, else what the go command recorded in info (go build, go
// install): the version of the module, vcs.revision, vcs.time and
// vcs.modified. A version neither has is "(devel)"; info is nil when the
// program has none.
func readBuild(version string, linked Build, info *debug.BuildInfo) (string, Build) {
	b := linked

	if info == nil {
		return orDevel(version), b
	}

	if version == "" {
		version = info.Main.Version
	}

	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			if b.Commit == "" {
				b.Commit = s.Value
			}
		case "vcs.time":
			if b.Date == "" {
				b.Date = s.Value
			}
		case "vcs.modified":
			b.Modified = s.Value == "true"
		}
	}

	return orDevel(version), b
}

// develVersion is the version of a program built from a tree, as the go
// command names it.
const develVersion = "(devel)"

// orDevel is version, or develVersion for none.
func orDevel(version string) string {
	if version == "" {
		return develVersion
	}

	return version
}

// shownVersion is what rigger version writes.
type shownVersion struct {
	Program   string `json:"program"`
	Version   string `json:"version"`
	Commit    string `json:"commit,omitempty"`
	Date      string `json:"date,omitempty"`
	Modified  bool   `json:"modified,omitempty"`
	GoVersion string `json:"go"`
	Platform  string `json:"platform"`
}

// writeVersion writes the version and the build of prog, for a person or
// as JSON.
func writeVersion(w io.Writer, prog Program, asJSON bool) error {
	v := shownVersion{
		Program: prog.Name, Version: prog.Version,
		Commit: prog.Build.Commit, Date: prog.Build.Date, Modified: prog.Build.Modified,
		GoVersion: runtime.Version(), Platform: runtime.GOOS + "/" + runtime.GOARCH,
	}

	if asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")

		return enc.Encode(v)
	}

	var out strings.Builder

	// The first line is the one rigger version has always written.
	fmt.Fprintf(&out, "rigger %s\n", v.Version)

	if v.Commit != "" {
		modified := ""
		if v.Modified {
			modified = " (modified)"
		}

		fmt.Fprintf(&out, "commit:   %s%s\n", v.Commit, modified)
	}

	if v.Date != "" {
		fmt.Fprintf(&out, "date:     %s\n", v.Date)
	}

	fmt.Fprintf(&out, "go:       %s\nplatform: %s\n", v.GoVersion, v.Platform)

	_, err := io.WriteString(w, out.String())

	return err
}

// newVersionCommand is rigger version.
func (a *app) newVersionCommand() *cobra.Command {
	var asJSON bool

	cmd := &cobra.Command{
		Use:   "version",
		Short: "Print the version and the build of the program",
		Long: `Print the version of the program and its build: the git commit and its
date, the Go version and the platform. The first line is "rigger <version>";
"rigger --version" prints the same. Built into norca, the version is the
one of norca.`,
		Example: `  rigger version
  rigger version --json`,
		RunE: func(*cobra.Command, []string) error {
			return writeVersion(a.stdout, a.prog, asJSON)
		},
	}

	cmd.Flags().BoolVar(&asJSON, jsonFlag, false, "write the version and the build as JSON")

	return cmd
}
