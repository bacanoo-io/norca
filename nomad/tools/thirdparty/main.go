// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Command thirdparty generates the third-party license notices
// (THIRD_PARTY_NOTICES.md) and the CycloneDX software bills of materials of
// the shipped Norca binaries.
//
//	thirdparty notices -root <repo> [-out FILE | -check FILE]
//	thirdparty sbom -root <repo> -binary <path> [-name norca] -out FILE
//
// The notices cover the Go modules linked into the shipped binaries on every
// release platform (see binaries and platforms below), found with
// `go list -deps`, and the production npm dependencies of the web UI, found
// in nomad/ui/package.json and nomad/ui/yarn.lock. Third-party Go source
// copied into the repository (sourceCopies) is listed like a module. The
// license files and NOTICE files come from the Go module cache (`go list`
// downloads missing modules), from the directory of a source copy, and from
// nomad/ui/node_modules, or, for packages missing there, from the registry
// tarballs named in yarn.lock, verified against their integrity hashes and
// cached in the user cache directory.
//
// Every license must be on the allow-list; anything else fails the run,
// unless overrides.go records a decision for the component. A decision
// marked pending (awaiting legal review) lets the notices be generated, but
// fails the run with -strict, which the release workflow uses.
//
// The SBOM is built from the Go build information embedded in a binary
// (`go version -m`), with the licenses found the same way as for the
// notices; the norca SBOM also lists the npm packages of the embedded UI.
//
// The output is deterministic: it contains no timestamps or random
// identifiers, so two runs on the same inputs produce identical files on
// every host operating system.
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
)

// binary describes a shipped executable.
type binary struct {
	// Name is the name of the executable (without .exe).
	Name string
	// ModuleDir is the directory of its Go module, relative to the
	// repository root.
	ModuleDir string
	// Package is the main package, relative to ModuleDir.
	Package string
	// Tags are the build tags of a release build.
	Tags []string
	// CGO reports whether release builds enable cgo where the platform
	// allows it (see platform.CGO).
	CGO bool
	// UI reports whether the binary embeds the web UI.
	UI bool
}

// platform is a release target. Keep this list, the build tags and the cgo
// settings in sync with .github/workflows/release.yml.
type platform struct {
	GOOS, GOARCH string
	// CGO reports whether cgo is enabled for binaries that use it. The
	// Linux builds need cgo for the exec driver (libcontainer's nsenter).
	CGO bool
	// Tags are additional build tags for this platform.
	Tags []string
}

var binaries = []binary{
	{
		Name:      "norca",
		ModuleDir: "nomad",
		Package:   ".",
		Tags:      []string{"ui", "codegen_generated", "release"},
		CGO:       true,
		UI:        true,
	},
}

var platforms = []platform{
	{GOOS: "linux", GOARCH: "amd64", CGO: true},
	{GOOS: "linux", GOARCH: "arm64", CGO: true},
	{GOOS: "darwin", GOARCH: "arm64", CGO: true, Tags: []string{"netcgo"}},
	{GOOS: "windows", GOARCH: "amd64", Tags: []string{"timetzdata"}},
}

// Allowed licenses (norca-doc content/docs/introduction/license.md, "Dependencies"; the UI list matches
// the license check in .github/workflows/compliance.yml).
var (
	allowedGo  = setOf("MPL-2.0", "Apache-2.0", "MIT", "BSD-2-Clause", "BSD-3-Clause", "ISC")
	allowedNPM = setOf("MPL-2.0", "Apache-2.0", "MIT", "BSD-2-Clause", "BSD-3-Clause", "ISC", "0BSD", "CC0-1.0")
)

// sourceCopy is third-party Go source copied into a Norca module (a git
// subtree). `go list` reports its packages as Norca's own; they are listed
// as a component of their own, with the license files of Dir.
type sourceCopy struct {
	// Path and Version identify the upstream module and release.
	Path, Version string
	// Dir is the directory of the copy, relative to the repository root.
	Dir string
	// ImportPath is the import path of Dir in the Norca module.
	ImportPath string
}

// sourceCopies are the third-party source copies. Update the version
// together with the `git subtree pull`.
var sourceCopies = []sourceCopy{
	{
		Path:       "github.com/cwolsen7905/ubixvault",
		Version:    "v1.5.0",
		Dir:        "nomad/secretstore/ubix",
		ImportPath: "github.com/hashicorp/nomad/secretstore/ubix",
	},
}

// findSourceCopy returns the source copy a package belongs to, or nil.
func findSourceCopy(importPath string) *sourceCopy {
	for i := range sourceCopies {
		c := &sourceCopies[i]
		if importPath == c.ImportPath || strings.HasPrefix(importPath, c.ImportPath+"/") {
			return c
		}
	}
	return nil
}

// forbiddenNPM lists packages that must not be shipped regardless of their
// license (norca-doc content/docs/introduction/license.md, "Prohibited").
var forbiddenNPM = setOf("apexcharts")

func setOf(s ...string) map[string]bool {
	m := make(map[string]bool, len(s))
	for _, v := range s {
		m[v] = true
	}
	return m
}

func usage() {
	fmt.Fprint(os.Stderr, `usage:
  thirdparty notices -root <repo> [-out FILE | -check FILE] [flags]
  thirdparty sbom -root <repo> -binary <path> [-name NAME] -out FILE [flags]
`)
	os.Exit(2)
}

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	var err error
	switch os.Args[1] {
	case "notices":
		err = runNotices(os.Args[2:])
	case "sbom":
		err = runSBOM(os.Args[2:])
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "thirdparty:", err)
		os.Exit(1)
	}
}

// commonFlags are shared by the subcommands.
type commonFlags struct {
	root     string
	cacheDir string
	offline  bool
	noUI     bool
	strict   bool
}

func (c *commonFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&c.root, "root", "..", "repository root (the directory that contains nomad/)")
	fs.StringVar(&c.cacheDir, "cache", "", "cache directory for npm tarballs (default: <user cache dir>/norca-thirdparty)")
	fs.BoolVar(&c.offline, "offline", false, "do not download npm tarballs; fail if a package is neither in node_modules nor in the cache")
	fs.BoolVar(&c.noUI, "no-ui", false, "skip the npm dependencies of the web UI")
	fs.BoolVar(&c.strict, "strict", false, "also fail on license decisions pending legal review (for releases)")
	fs.BoolVar(&verbose, "v", false, "list the license files in which no license was recognized")
}

var verbose bool

// kinds returns the component kinds covered by a run.
func (c *commonFlags) kinds() []string {
	if c.noUI {
		return []string{"go"}
	}
	return []string{"go", "npm"}
}
