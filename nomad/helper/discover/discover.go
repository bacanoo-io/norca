// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package discover

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// NomadExecutable checks the current executable, then the $PATH, $GOPATH/bin,
// and finally the CWD, in that order, for a norca or nomad binary. If it can't
// be found, an error is returned.
func NomadExecutable() (string, error) {
	// The norca binary is preferred; nomad is its compatibility alias and
	// the name "go install" gives the binary of this module.
	names := []string{"norca", "nomad"}
	if runtime.GOOS == "windows" {
		names = []string{"norca.exe", "nomad.exe"}
	}

	// Check the current executable.
	bin, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("Failed to determine the norca executable: %v", err)
	}

	if _, err := os.Stat(bin); err == nil && isNomad(bin, names[0]) {
		return bin, nil
	}

	// Check the $PATH
	for _, name := range names {
		if bin, err := exec.LookPath(name); err == nil {
			return bin, nil
		}
	}

	// Check the $GOPATH.
	for _, name := range names {
		bin = filepath.Join(os.Getenv("GOPATH"), "bin", name)
		if _, err := os.Stat(bin); err == nil {
			return bin, nil
		}
	}

	// Check the CWD.
	pwd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("Could not find Norca executable (%v): %v", names, err)
	}

	for _, name := range names {
		bin = filepath.Join(pwd, name)
		if _, err := os.Stat(bin); err == nil {
			return bin, nil
		}

		// Check CWD/bin
		bin = filepath.Join(pwd, "bin", name)
		if _, err := os.Stat(bin); err == nil {
			return bin, nil
		}
	}

	return "", fmt.Errorf("Could not find Norca executable (%v)", names)
}

func isNomad(path, nomadExe string) bool {
	if strings.HasSuffix(path, ".test") || strings.HasSuffix(path, ".test.exe") {
		return false
	}
	return true
}
