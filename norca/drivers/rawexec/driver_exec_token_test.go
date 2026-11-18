// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

//go:build !windows

package rawexec

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/helper/testtask"
	"github.com/bacanoo-io/norca/norca/helper/uuid"
	"github.com/bacanoo-io/norca/norca/plugins/drivers"
	dproto "github.com/bacanoo-io/norca/norca/plugins/drivers/proto"
	dtestutil "github.com/bacanoo-io/norca/norca/plugins/drivers/testutils"
	"github.com/shoenig/test/must"
)

// stdoutStream is an exec stream without a terminal that sends no input and
// collects what the command writes to its standard output.
type stdoutStream struct {
	mu     sync.Mutex
	sent   bool
	stdout string
}

func (s *stdoutStream) Recv() (*drivers.ExecTaskStreamingRequestMsg, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sent {
		return nil, io.EOF
	}
	s.sent = true
	return &drivers.ExecTaskStreamingRequestMsg{
		Stdin: &dproto.ExecTaskStreamingIOOperation{Close: true},
	}, nil
}

func (s *stdoutStream) Send(m *drivers.ExecTaskStreamingResponseMsg) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m.Stdout != nil {
		s.stdout += string(m.Stdout.Data)
	}
	return nil
}

func (s *stdoutStream) output() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stdout
}

// A command run in a task by `alloc exec` gets the task's environment and
// sees its secrets directory: with `identity { env = true }` or `file = true`
// it reads the task's workload identity token, and with it the ACL policies
// bound to the task's job (review finding JOG-8, question 3, checked
// 2026-11-18). CURRENT BEHAVIOUR: the exec permission (alloc-exec, and
// alloc-node-exec for a driver without file system isolation such as this
// one) is the only check; the exec and docker drivers pass the same task
// environment to an exec session (executor_linux.go, the container's env).
func TestRawExec_ExecTaskStreaming_SeesWorkloadIdentityToken(t *testing.T) {
	ci.Parallel(t)

	d := newEnabledRawExecDriver(t)
	harness := dtestutil.NewDriverHarness(t, d)
	defer harness.Kill()

	task := &drivers.TaskConfig{
		AllocID: uuid.Generate(),
		ID:      uuid.Generate(),
		Name:    "sleep",
		Env:     defaultEnv(),
	}
	cleanup := harness.MkAllocDir(task, false)
	defer cleanup()

	// What the identity hook gives the task: the variable with env = true,
	// the file in the secrets directory with file = true.
	secrets := task.TaskDir().SecretsDir
	must.NoError(t, os.WriteFile(filepath.Join(secrets, "norca_token"), []byte("token-from-file"), 0o600))
	task.Env["NORCA_TOKEN"] = "token-from-env"
	task.Env["NORCA_SECRETS_DIR"] = secrets

	tc := &TaskConfig{
		Command: testtask.Path(),
		Args:    []string{"sleep", "9000s"},
	}
	must.NoError(t, task.EncodeConcreteDriverConfig(&tc))
	testtask.SetTaskConfigEnv(task)

	_, _, err := harness.StartTask(task)
	must.NoError(t, err)
	defer func() { _ = d.DestroyTask(task.ID, true) }()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	stream := &stdoutStream{}
	cmd := []string{"/bin/sh", "-c", `printf '%s|' "$NORCA_TOKEN"; cat "$NORCA_SECRETS_DIR/norca_token"`}
	must.NoError(t, d.ExecTaskStreamingRaw(ctx, task.ID, cmd, false, stream))

	// CURRENT BEHAVIOUR: the exec session reads both.
	must.Eq(t, "token-from-env|token-from-file", stream.output())
}
