// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package template

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/client/secrets"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/shoenig/test/must"
	"github.com/shoenig/test/wait"
)

// tmplSecretProvider is an in-memory provider with change notification.
type tmplSecretProvider struct {
	lock    sync.Mutex
	items   map[string]string
	version int
	changed chan struct{}
	paths   []string
}

func (p *tmplSecretProvider) Name() string { return structs.SecretProviderNorca }
func (p *tmplSecretProvider) Type() string { return structs.SecretProviderNorca }
func (p *tmplSecretProvider) Capabilities() secrets.Capabilities {
	return secrets.Capabilities{Watch: true, AuditsReads: true}
}

func (p *tmplSecretProvider) Fetch(ctx context.Context, req *secrets.Request) (*secrets.Secret, error) {
	p.lock.Lock()
	defer p.lock.Unlock()
	p.paths = append(p.paths, req.Path)
	items := map[string]string{}
	for k, v := range p.items {
		items[k] = v
	}
	return &secrets.Secret{Items: items, Version: strconv.Itoa(p.version)}, nil
}

func (p *tmplSecretProvider) Watch(ctx context.Context, req *secrets.Request, version string) (string, error) {
	for {
		p.lock.Lock()
		v, ch := strconv.Itoa(p.version), p.changed
		p.lock.Unlock()
		if v != version {
			return v, nil
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-ch:
		}
	}
}

func (p *tmplSecretProvider) set(items map[string]string) {
	p.lock.Lock()
	defer p.lock.Unlock()
	p.items = items
	p.version++
	close(p.changed)
	p.changed = make(chan struct{})
}

// TestTaskTemplateManager_NomadSecret renders a template with nomadSecret,
// re-renders it within 10 seconds after the secret changed and applies the
// template's change mode.
func TestTaskTemplateManager_NomadSecret(t *testing.T) {
	ci.Parallel(t)

	p := &tmplSecretProvider{items: map[string]string{"password": "one"}, version: 1, changed: make(chan struct{})}
	src := secrets.NewTemplateSource(&secrets.TemplateSourceConfig{
		Registry: secrets.NewRegistry(p, nil, nil),
		Lookup: func(explicit, path string) *secrets.Lookup {
			return &secrets.Lookup{Explicit: explicit, Path: path}
		},
		Identity:    func() *secrets.Identity { return &secrets.Identity{Token: "wi"} },
		Interpolate: func(s string) string { return structs.InterpolateSecretPath(s, "web", "g", "t") },
	})
	defer src.Stop()

	tmpl := &structs.Template{
		EmbeddedTmpl: `{{ with nomadSecret "jobs/${job}/db" }}pw={{ .password }}{{ end }}`,
		DestPath:     "local/app.conf",
		ChangeMode:   structs.TemplateChangeModeRestart,
	}
	harness := newTestHarness(t, []*structs.Template{tmpl}, false, false)
	must.NoError(t, os.MkdirAll(filepath.Join(harness.taskDir, "local"), 0o755))
	manager, err := NewTaskTemplateManager(&TaskTemplateManagerConfig{
		UnblockCh:            harness.mockHooks.UnblockCh,
		Lifecycle:            harness.mockHooks,
		Events:               harness.mockHooks,
		Templates:            harness.templates,
		ClientConfig:         harness.config,
		TaskDir:              harness.taskDir,
		EnvBuilder:           harness.envBuilder,
		MaxTemplateEventRate: DefaultMaxTemplateEventRate,
		Secrets:              src,
	})
	must.NoError(t, err)
	harness.manager = manager
	defer harness.stop()

	select {
	case <-harness.mockHooks.UnblockCh:
	case <-time.After(10 * time.Second):
		t.Fatal("task unblock should have been called")
	}
	dest := filepath.Join(harness.taskDir, "local", "app.conf")
	data, err := os.ReadFile(dest)
	must.NoError(t, err)
	must.Eq(t, "pw=one", string(data))
	p.lock.Lock()
	must.Eq(t, []string{"jobs/web/db"}, p.paths)
	p.lock.Unlock()

	start := time.Now()
	p.set(map[string]string{"password": "two"})
	must.Wait(t, wait.InitialSuccess(wait.BoolFunc(func() bool {
		data, _ := os.ReadFile(dest)
		return string(data) == "pw=two"
	}), wait.Timeout(10*time.Second), wait.Gap(100*time.Millisecond)))
	must.Less(t, 10*time.Second, time.Since(start))

	select {
	case <-harness.mockHooks.RestartCh:
	case <-time.After(10 * time.Second):
		t.Fatal("the change mode restart was not applied")
	}
}
