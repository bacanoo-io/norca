// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

//go:build ui

// Command legacyui writes the legacy web UI (served at /ui/legacy/) embedded
// in command/agent/bindata_assetfs.go back into a directory, so that the
// embedded UI can be regenerated with a rebuilt new UI while keeping the
// legacy one. windows.bat uses it, because the legacy UI's build tools fail on
// Windows; on Linux, make ember-dist-legacy builds it from source.
//
//	go run -tags ui,codegen_generated ./scripts/legacyui <dir>
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/hashicorp/nomad/command/agent"
)

const prefix = "dist/legacy/"

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: legacyui <dir>")
		os.Exit(2)
	}
	dir := os.Args[1]

	var n int
	for _, name := range agent.AssetNames() {
		rel, ok := strings.CutPrefix(name, prefix)
		if !ok {
			continue
		}
		data, err := agent.Asset(name)
		if err != nil {
			fail(err)
		}
		path := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			fail(err)
		}
		if err := os.WriteFile(path, data, 0o644); err != nil {
			fail(err)
		}
		n++
	}
	if n == 0 {
		fail(fmt.Errorf("the embedded UI holds no legacy UI (%s*)", prefix))
	}
	fmt.Printf("wrote %d files of the legacy UI to %s\n", n, dir)
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "legacyui:", err)
	os.Exit(1)
}
