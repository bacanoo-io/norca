// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package plugins

import (
	"errors"
	"os"
	"os/exec"
	"testing"

	log "github.com/hashicorp/go-hclog"
	"github.com/hashicorp/nomad/ci"
	"github.com/shoenig/test/must"
)

// TestServe_UnsupportedExitCode asserts that serving an unsupported plugin
// type ends the process with a non-zero exit code. serve calls os.Exit, so
// the test runs itself in a child process.
func TestServe_UnsupportedExitCode(t *testing.T) {
	if os.Getenv("NOMAD_TEST_SERVE_UNSUPPORTED") == "1" {
		serve(struct{}{}, log.NewNullLogger())
		return
	}
	ci.Parallel(t)

	cmd := exec.Command(os.Args[0], "-test.run=^TestServe_UnsupportedExitCode$")
	cmd.Env = append(os.Environ(), "NOMAD_TEST_SERVE_UNSUPPORTED=1")
	out, err := cmd.Output()

	var exitErr *exec.ExitError
	must.True(t, errors.As(err, &exitErr), must.Sprintf("err: %v", err))
	must.Eq(t, 1, exitErr.ExitCode())
	must.StrContains(t, string(out), "Unsupported plugin type")
}
