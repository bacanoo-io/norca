// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package taskrunner

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/norca/mock"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/shoenig/test/must"
	"github.com/shoenig/test/wait"
)

// A template reads the workload certificate of native mTLS with
// {{ file (env "NORCA_MTLS_CERT_FILE") }}: the mTLS hook runs before the
// templates, so the task starts with the certificate in the rendered file,
// and the template is rendered again when the client renews it.
func TestTaskRunner_MTLSTemplate(t *testing.T) {
	ci.Parallel(t)

	alloc := mock.BatchAlloc()
	tg := alloc.Job.TaskGroups[0]
	tg.Services = []*structs.Service{{
		Name: "web", Provider: structs.ServiceProviderNomad,
		MTLS: &structs.ServiceMTLS{Mode: structs.MTLSModeApp},
	}}
	task := tg.Tasks[0]
	task.Config = map[string]any{"run_for": "30s"}
	task.Templates = []*structs.Template{{
		EmbeddedTmpl: `{{ file (env "NORCA_MTLS_CERT_FILE") }}`,
		DestPath:     "local/cert-copy.pem",
		ChangeMode:   structs.TemplateChangeModeNoop,
	}}
	alloc.SignedIdentities = map[string]string{task.Name: "workload-identity"}

	// Renewed after 8 s; the client renders a changed template only after 5 s
	// without a change (the default template wait), so the renewals must be
	// further apart than that.
	rpc := newFakeMTLSServer(t, 12*time.Second)
	conf, cleanup := testTaskRunnerConfig(t, alloc, task.Name)
	defer cleanup()
	conf.RPCClient = rpc
	conf.ClientConfig.Node.SecretID = "node-secret"

	tr, err := NewTaskRunner(conf)
	must.NoError(t, err)
	defer tr.Kill(context.Background(), structs.NewTaskEvent("cleanup"))
	go tr.Run()

	cert := filepath.Join(conf.TaskDir.SecretsDir, "mtls", "cert.pem")
	copied := filepath.Join(conf.TaskDir.LocalDir, "cert-copy.pem")
	same := func() bool {
		a, errA := os.ReadFile(cert)
		b, errB := os.ReadFile(copied)
		return errA == nil && errB == nil && len(a) > 0 && string(a) == string(b)
	}

	// The task starts with the certificate rendered.
	must.Wait(t, wait.InitialSuccess(
		wait.BoolFunc(func() bool { return tr.TaskState().State == structs.TaskStateRunning && same() }),
		wait.Timeout(15*time.Second), wait.Gap(100*time.Millisecond)))
	first, err := os.ReadFile(copied)
	must.NoError(t, err)

	// The renewal is rendered too.
	must.Wait(t, wait.InitialSuccess(
		wait.BoolFunc(func() bool {
			b, err := os.ReadFile(copied)
			return rpc.signCount() >= 2 && err == nil && string(b) != string(first) && same()
		}),
		wait.Timeout(30*time.Second), wait.Gap(200*time.Millisecond)))
}
