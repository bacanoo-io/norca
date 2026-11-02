// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package taskrunner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/client/allocrunner/interfaces"
	"github.com/bacanoo-io/norca/norca/client/secrets"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/shoenig/test/must"
	"github.com/shoenig/test/wait"
)

// leaseTestProvider serves dynamic secrets, and the static fallback at any
// path.
type leaseTestProvider struct {
	lock     sync.Mutex
	issueErr error
	issued   int
	renewed  *secrets.Lease
	renewErr error
	renews   int
}

func (p *leaseTestProvider) Name() string { return "norca" }
func (p *leaseTestProvider) Type() string { return "norca" }
func (p *leaseTestProvider) Capabilities() secrets.Capabilities {
	return secrets.Capabilities{Leases: true, AuditsReads: true}
}

func (p *leaseTestProvider) setIssueErr(err error) {
	p.lock.Lock()
	defer p.lock.Unlock()
	p.issueErr = err
}

func (p *leaseTestProvider) Fetch(_ context.Context, req *secrets.Request) (*secrets.Secret, error) {
	p.lock.Lock()
	defer p.lock.Unlock()
	if req.Engine == "" {
		return &secrets.Secret{Items: map[string]string{"username": "static"}, Version: "1"}, nil
	}
	if p.issueErr != nil {
		return nil, p.issueErr
	}
	p.issued++
	now := time.Now()
	return &secrets.Secret{
		Items:   map[string]string{"username": "dynamic", "cert": "CERT-1", "key": "KEY-1"},
		Version: "lease-1",
		Lease: &secrets.Lease{ID: "lease-1", Renewable: true,
			ExpireTime: now.Add(200 * time.Millisecond), MaxExpireTime: now.Add(time.Hour)},
	}, nil
}

func (p *leaseTestProvider) Renew(context.Context, *secrets.Lease, *secrets.Identity) (*secrets.Lease, error) {
	p.lock.Lock()
	defer p.lock.Unlock()
	p.renews++
	if p.renewErr != nil {
		return nil, p.renewErr
	}
	if p.renewed != nil {
		out := p.renewed
		p.renewed = nil
		return out, nil
	}
	now := time.Now()
	return &secrets.Lease{ID: "lease-1", Renewable: true,
		ExpireTime: now.Add(time.Hour), MaxExpireTime: now.Add(2 * time.Hour)}, nil
}

func (p *leaseTestProvider) Revoke(context.Context, *secrets.Lease, *secrets.Identity) error {
	return nil
}

func dynamicSecret() *structs.TaskSecret {
	return &structs.TaskSecret{
		Name: "db", Engine: "postgres", Role: "app",
		Fallback: "app/db-static",
		Files:    []*structs.TaskSecretFile{{Destination: "secrets/db.env"}},
		Timeout:  20 * time.Second,
	}
}

func secretFile(t *testing.T, ht *secretHookTest, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(ht.taskDir.Dir, "secrets", name))
	must.NoError(t, err)
	return string(data)
}

func TestSecretHook_FallbackAtStart(t *testing.T) {
	ci.Parallel(t)
	p := &leaseTestProvider{issueErr: secrets.NewError("norca", secrets.KindUnavailable, errors.New("operator down"))}
	spec := dynamicSecret()
	ht := newSecretHookTest(t, secrets.NewRegistry(p, nil, nil), spec)
	defer ht.hook.Shutdown()

	_, err := ht.prestart(context.Background())
	must.NoError(t, err)
	must.StrContains(t, secretFile(t, ht, "db.env"), `username="static"`)
	must.True(t, slicesContain(ht.events.messages(), "provider fallback"))
}

func slicesContain(messages []string, part string) bool {
	for _, m := range messages {
		if strings.Contains(m, part) {
			return true
		}
	}
	return false
}

// A task that is not allowed the credential does not get the fallback.
func TestSecretHook_FallbackNotOnDenied(t *testing.T) {
	ci.Parallel(t)
	p := &leaseTestProvider{issueErr: secrets.NewError("norca", secrets.KindDenied, errors.New("denied"))}
	ht := newSecretHookTest(t, secrets.NewRegistry(p, nil, nil), dynamicSecret())
	defer ht.hook.Shutdown()

	_, err := ht.prestart(context.Background())
	must.Error(t, err)
}

// With fallback_recover the task moves to a credential when it can be
// created, and the change mode applies.
func TestSecretHook_FallbackRecover(t *testing.T) {
	ci.Parallel(t)
	p := &leaseTestProvider{issueErr: secrets.NewError("norca", secrets.KindUnavailable, errors.New("operator down"))}
	spec := dynamicSecret()
	spec.FallbackRecover = structs.SecretFallbackRecoverSignal
	spec.ChangeMode = structs.SecretChangeModeNoop
	spec.FallbackRecoverInterval = time.Second
	ht := newSecretHookTest(t, secrets.NewRegistry(p, nil, nil), spec)
	defer ht.hook.Shutdown()

	_, err := ht.prestart(context.Background())
	must.NoError(t, err)
	must.StrContains(t, secretFile(t, ht, "db.env"), `username="static"`)

	p.setIssueErr(nil)
	must.Wait(t, wait.InitialSuccess(wait.BoolFunc(func() bool {
		return strings.Contains(secretFile(t, ht, "db.env"), `username="dynamic"`)
	}), wait.Timeout(5*time.Second), wait.Gap(20*time.Millisecond)))
	must.Wait(t, wait.InitialSuccess(wait.BoolFunc(func() bool {
		return slicesContain(ht.events.messages(), "leaving the fallback")
	}), wait.Timeout(5*time.Second), wait.Gap(20*time.Millisecond)))
}

// A running task whose renewal fails keeps its credential: it never moves to
// the fallback.
func TestSecretHook_NoFallbackOnRenew(t *testing.T) {
	ci.Parallel(t)
	p := &leaseTestProvider{renewErr: secrets.NewError("norca", secrets.KindUnavailable, errors.New("operator down"))}
	spec := dynamicSecret()
	spec.ChangeMode = structs.SecretChangeModeNoop
	ht := newSecretHookTest(t, secrets.NewRegistry(p, nil, nil), spec)
	defer ht.hook.Shutdown()

	_, err := ht.prestart(context.Background())
	must.NoError(t, err)
	must.Wait(t, wait.InitialSuccess(wait.BoolFunc(func() bool {
		p.lock.Lock()
		defer p.lock.Unlock()
		return p.renews > 0
	}), wait.Timeout(5*time.Second), wait.Gap(20*time.Millisecond)))
	must.StrContains(t, secretFile(t, ht, "db.env"), `username="dynamic"`)
	must.False(t, slicesContain(ht.events.messages(), "provider fallback"))
}

// A certificate: every item has its file, and a renewal that replaces the
// credential rewrites them.
func TestSecretHook_RenewReplacesCredential(t *testing.T) {
	ci.Parallel(t)
	now := time.Now()
	p := &leaseTestProvider{renewed: &secrets.Lease{ID: "lease-1", Renewable: true,
		ExpireTime: now.Add(time.Hour), MaxExpireTime: now.Add(2 * time.Hour),
		NewItems: map[string]string{"cert": "CERT-2", "key": "KEY-2"}}}
	spec := &structs.TaskSecret{
		Name: "tls", Engine: "pki", Role: "client",
		Files: []*structs.TaskSecretFile{
			{Destination: "secrets/tls.crt", Format: "raw:cert"},
			{Destination: "secrets/tls.key", Format: "raw:key"},
		},
		ChangeMode: structs.SecretChangeModeNoop,
		Timeout:    5 * time.Second,
	}
	ht := newSecretHookTest(t, secrets.NewRegistry(p, nil, nil), spec)
	defer ht.hook.Shutdown()

	_, err := ht.prestart(context.Background())
	must.NoError(t, err)
	must.Eq(t, "CERT-1", secretFile(t, ht, "tls.crt"))
	must.Eq(t, "KEY-1", secretFile(t, ht, "tls.key"))

	must.Wait(t, wait.InitialSuccess(wait.BoolFunc(func() bool {
		return secretFile(t, ht, "tls.crt") == "CERT-2" && secretFile(t, ht, "tls.key") == "KEY-2"
	}), wait.Timeout(5*time.Second), wait.Gap(20*time.Millisecond)))
	must.True(t, slicesContain(ht.events.messages(), "renewed credential delivered"))
}

// A restart of the task asks for a credential again.
func TestSecretHook_FallbackRetriedOnRestart(t *testing.T) {
	ci.Parallel(t)
	p := &leaseTestProvider{issueErr: secrets.NewError("norca", secrets.KindUnavailable, errors.New("operator down"))}
	spec := dynamicSecret()
	ht := newSecretHookTest(t, secrets.NewRegistry(p, nil, nil), spec)
	defer ht.hook.Shutdown()

	resp, err := ht.prestart(context.Background())
	must.NoError(t, err)
	must.Eq(t, "1", resp.State[secretFallbackStatePrefix+"db"])

	// Still down: the task restarts on the fallback.
	_, err = ht.prestart(context.Background())
	must.NoError(t, err)
	must.StrContains(t, secretFile(t, ht, "db.env"), `username="static"`)

	p.setIssueErr(nil)
	resp, err = ht.prestart(context.Background())
	must.NoError(t, err)
	must.StrContains(t, secretFile(t, ht, "db.env"), `username="dynamic"`)
	must.MapNotContainsKey(t, resp.State, secretFallbackStatePrefix+"db")
}

// fallback_keep_on_restart keeps the fallback when the task restarts, and when
// the client restarts.
func TestSecretHook_FallbackKeptOnRestart(t *testing.T) {
	ci.Parallel(t)
	p := &leaseTestProvider{issueErr: secrets.NewError("norca", secrets.KindUnavailable, errors.New("operator down"))}
	spec := dynamicSecret()
	spec.FallbackKeepOnRestart = true
	ht := newSecretHookTest(t, secrets.NewRegistry(p, nil, nil), spec)
	defer ht.hook.Shutdown()

	resp, err := ht.prestart(context.Background())
	must.NoError(t, err)

	p.setIssueErr(nil)
	_, err = ht.prestart(context.Background())
	must.NoError(t, err)
	must.StrContains(t, secretFile(t, ht, "db.env"), `username="static"`)
	must.Eq(t, 0, p.issuedCount())

	// The client restarts with the task running.
	ht.lifecycle.running = true
	hook := ht.newHook(secrets.NewRegistry(p, nil, nil))
	defer hook.Shutdown()
	req := &interfaces.TaskPrestartRequest{Task: ht.task, TaskDir: ht.taskDir, PreviousState: resp.State}
	must.NoError(t, hook.Prestart(context.Background(), req, &interfaces.TaskPrestartResponse{}))
	time.Sleep(100 * time.Millisecond)
	must.Eq(t, 0, p.issuedCount())
}

func (p *leaseTestProvider) issuedCount() int {
	p.lock.Lock()
	defer p.lock.Unlock()
	return p.issued
}
