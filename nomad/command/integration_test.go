// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/nomad/api"
	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/client/testutil"
	"github.com/stretchr/testify/assert"
)

func TestIntegration_Command_NomadInit(t *testing.T) {
	ci.Parallel(t)
	tmpDir := t.TempDir()

	{
		cmd := exec.Command("nomad", "job", "init")
		cmd.Dir = tmpDir
		if err := cmd.Run(); err != nil {
			t.Fatalf("error running init: %v", err)
		}
	}

	{
		cmd := exec.Command("nomad", "job", "validate", "example.nomad.hcl")
		cmd.Dir = tmpDir
		cmd.Env = []string{`NOMAD_ADDR=http://127.0.0.1:0`}
		if err := cmd.Run(); err != nil {
			t.Fatalf("error validating example.nomad.hcl: %v", err)
		}
	}
}

func TestIntegration_Command_RoundTripJob(t *testing.T) {
	ci.Parallel(t)
	// The example job runs a Docker container, so the client must be able to
	// run allocations: that needs root as well as Docker.
	testutil.RequireRoot(t)
	testutil.DockerCompatible(t)

	// "nomad job run" monitors the deployment and never returns when the
	// allocation can not be started, so bound every command.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	assert := assert.New(t)
	tmpDir := t.TempDir()

	// Start in dev mode so we get a node registration
	srv, client, url := testServer(t, true, nil)
	defer srv.Shutdown()

	{
		cmd := exec.CommandContext(ctx, "nomad", "job", "init", "-short")
		cmd.Dir = tmpDir
		assert.Nil(cmd.Run())
	}

	{
		cmd := exec.CommandContext(ctx, "nomad", "job", "run", "example.nomad.hcl")
		cmd.Dir = tmpDir
		cmd.Env = []string{fmt.Sprintf("NOMAD_ADDR=%s", url)}
		err := cmd.Run()
		if err != nil && !strings.Contains(err.Error(), "exit status 2") {
			t.Fatalf("error running example.nomad.hcl: %v", err)
		}
	}

	{
		cmd := exec.CommandContext(ctx, "nomad", "job", "inspect", "example")
		cmd.Dir = tmpDir
		cmd.Env = []string{fmt.Sprintf("NOMAD_ADDR=%s", url)}
		out, err := cmd.Output()
		assert.Nil(err)

		var req api.JobRegisterRequest
		dec := json.NewDecoder(bytes.NewReader(out))
		assert.Nil(dec.Decode(&req))

		var resp api.JobRegisterResponse
		_, err = client.Raw().Write("/v1/jobs", req, &resp, nil)
		assert.Nil(err)
		assert.NotZero(resp.EvalID)
	}

	{
		cmd := exec.CommandContext(ctx, "nomad", "job", "stop", "example")
		cmd.Dir = tmpDir
		cmd.Env = []string{fmt.Sprintf("NOMAD_ADDR=%s", url)}
		_, err := cmd.Output()
		assert.Nil(err)
	}
}
