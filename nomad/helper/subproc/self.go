// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package subproc

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

var (
	// executable is the executable of this process
	executable string
)

func init() {
	s, err := os.Executable()
	if err != nil {
		panic(fmt.Sprintf("failed to detect executable: %v", err))
	}

	// when running tests, we need to use the real norca binary (or a binary
	// named nomad, the compatibility alias), and make sure you recompile
	// between changes!
	if strings.HasSuffix(s, ".test") {
		if s, err = lookPathBinary(); err != nil {
			panic(fmt.Sprintf("failed to find norca binary: %v", err))
		}
	}
	executable = s
}

// binaryNames are the names of the agent binary searched on the PATH by
// tests, in order of preference.
var binaryNames = []string{"norca", "nomad"}

// lookPathBinary returns the first binary of binaryNames found on the PATH.
func lookPathBinary() (string, error) {
	var firstErr error
	for _, name := range binaryNames {
		s, err := exec.LookPath(name)
		if err == nil {
			return s, nil
		}
		if firstErr == nil {
			firstErr = err
		}
	}
	return "", firstErr
}

// Self returns the path to the executable of this process.
func Self() string {
	return executable
}
