// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package taskrunner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bacanoo-io/norca/nomad/ci"
	"github.com/bacanoo-io/norca/nomad/client/allocrunner/interfaces"
	"github.com/bacanoo-io/norca/nomad/client/secrets"
	"github.com/bacanoo-io/norca/nomad/nomad/structs"
	"github.com/shoenig/test/must"
	"github.com/shoenig/test/wait"
)

// testLeaseProvider is an in-memory provider of dynamic secrets: every
// fetch creates a credential with a lease of ttl, up to maxTTL.
type testLeaseProvider struct {
	ttl, maxTTL time.Duration

	lock     sync.Mutex
	issued   int
	requests []*secrets.Request
	leases   map[string]*secrets.Lease
	ended    map[string]bool
	renewals map[string]int
	revoked  []string
}

func newTestLeaseProvider(ttl, maxTTL time.Duration) *testLeaseProvider {
	return &testLeaseProvider{ttl: ttl, maxTTL: maxTTL,
		leases: map[string]*secrets.Lease{}, ended: map[string]bool{}, renewals: map[string]int{}}
}

func (p *testLeaseProvider) Name() string { return "norca" }
func (p *testLeaseProvider) Type() string { return "norca" }
func (p *testLeaseProvider) Capabilities() secrets.Capabilities {
	return secrets.Capabilities{Leases: true, AuditsReads: true}
}

func (p *testLeaseProvider) Fetch(ctx context.Context, req *secrets.Request) (*secrets.Secret, error) {
	p.lock.Lock()
	defer p.lock.Unlock()
	p.issued++
	p.requests = append(p.requests, req)
	now := time.Now()
	l := &secrets.Lease{
		ID:            fmt.Sprintf("lease-%d", p.issued),
		ExpireTime:    now.Add(p.ttl),
		MaxExpireTime: now.Add(p.maxTTL),
		Renewable:     true,
	}
	p.leases[l.ID] = l
	cp := *l
	return &secrets.Secret{
		Items:   map[string]string{"creds": fmt.Sprintf("creds-%d-%s", p.issued, secretTestCanary)},
		Version: l.ID,
		Lease:   &cp,
	}, nil
}

func (p *testLeaseProvider) Renew(ctx context.Context, lease *secrets.Lease, id *secrets.Identity) (*secrets.Lease, error) {
	p.lock.Lock()
	defer p.lock.Unlock()
	l, ok := p.leases[lease.ID]
	if !ok || p.ended[lease.ID] {
		return nil, secrets.NewError("norca", secrets.KindNotFound, errors.New("secret lease is revoked"))
	}
	p.renewals[lease.ID]++
	l.ExpireTime = time.Now().Add(p.ttl)
	if l.ExpireTime.After(l.MaxExpireTime) {
		l.ExpireTime = l.MaxExpireTime
	}
	cp := *l
	return &cp, nil
}

func (p *testLeaseProvider) Revoke(ctx context.Context, lease *secrets.Lease, id *secrets.Identity) error {
	p.lock.Lock()
	defer p.lock.Unlock()
	p.revoked = append(p.revoked, lease.ID)
	p.ended[lease.ID] = true
	return nil
}

func (p *testLeaseProvider) end(id string) {
	p.lock.Lock()
	defer p.lock.Unlock()
	p.ended[id] = true
}

func (p *testLeaseProvider) counts() (issued int, renewals map[string]int, revoked []string) {
	p.lock.Lock()
	defer p.lock.Unlock()
	r := map[string]int{}
	for k, v := range p.renewals {
		r[k] = v
	}
	return p.issued, r, append([]string(nil), p.revoked...)
}

func natsSecret(mode string) *structs.TaskSecret {
	return &structs.TaskSecret{Name: "nats", Engine: "nats", Role: "worker",
		Files: []*structs.TaskSecretFile{{Destination: "secrets/nats.creds", Format: "raw:creds"}}, ChangeMode: mode, Timeout: 5 * time.Second}
}

func readCreds(t *testing.T, ht *secretHookTest) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(ht.taskDir.Dir, "secrets", "nats.creds"))
	must.NoError(t, err)
	return string(data)
}

func leaseWaitFor(t *testing.T, f func() bool) {
	t.Helper()
	must.Wait(t, wait.InitialSuccess(wait.BoolFunc(f), wait.Timeout(15*time.Second), wait.Gap(20*time.Millisecond)))
}

// TestSecretHook_DynamicSecret requests a credential, delivers it to the
// engine's default file, renews the lease, gets a new credential when
// max_ttl is near (with the change mode) and revokes the lease at the stop.
func TestSecretHook_DynamicSecret(t *testing.T) {
	ci.Parallel(t)
	p := newTestLeaseProvider(400*time.Millisecond, 2*time.Second)
	ht := newSecretHookTest(t, secrets.NewRegistry(p, nil, nil), natsSecret(structs.SecretChangeModeRestart))
	defer ht.hook.Shutdown()

	resp, err := ht.prestart(context.Background())
	must.NoError(t, err)
	ht.lifecycle.lock.Lock()
	ht.lifecycle.running = true
	ht.lifecycle.lock.Unlock()

	must.Eq(t, "creds-1-"+secretTestCanary, readCreds(t, ht))
	p.lock.Lock()
	req := p.requests[0]
	p.lock.Unlock()
	must.Eq(t, "nats", req.Engine)
	must.Eq(t, "worker", req.Role)
	must.Eq(t, "nats", req.Name)
	must.Eq(t, "", req.Path)
	must.Eq(t, "wi-token", req.Identity.Token)

	// The lease ID (never the credential) is kept in the hook state.
	must.StrHasPrefix(t, "lease-1 ", resp.State["lease.nats"])

	// Renewals while the lease can be extended.
	leaseWaitFor(t, func() bool { _, r, _ := p.counts(); return r["lease-1"] >= 2 })

	// Close to max_ttl a new credential replaces it, with the change mode.
	leaseWaitFor(t, func() bool {
		issued, _, _ := p.counts()
		restarts, _ := ht.lifecycle.counts()
		return issued >= 2 && restarts >= 1
	})
	must.Eq(t, "creds-2-"+secretTestCanary, readCreds(t, ht))

	// The stop revokes the current lease.
	must.NoError(t, ht.hook.Stop(context.Background(), &interfaces.TaskStopRequest{}, &interfaces.TaskStopResponse{}))
	issued, _, revoked := p.counts()
	must.Eq(t, []string{fmt.Sprintf("lease-%d", issued)}, revoked)

	msgs := strings.Join(ht.events.messages(), "\n")
	must.StrContains(t, msgs, "secret nats: delivered (lease lease-1")
	must.StrContains(t, msgs, "secret nats: new credential delivered (lease lease-2")
	must.StrNotContains(t, msgs, secretTestCanary)
	must.SliceEmpty(t, ht.auditor.records)
}

// TestSecretHook_DynamicSecretRevoked replaces a credential whose lease was
// revoked by the servers.
func TestSecretHook_DynamicSecretRevoked(t *testing.T) {
	ci.Parallel(t)
	p := newTestLeaseProvider(300*time.Millisecond, time.Hour)
	spec := natsSecret(structs.SecretChangeModeSignal)
	spec.ChangeSignal = "SIGHUP"
	ht := newSecretHookTest(t, secrets.NewRegistry(p, nil, nil), spec)
	defer ht.hook.Shutdown()

	_, err := ht.prestart(context.Background())
	must.NoError(t, err)
	ht.lifecycle.lock.Lock()
	ht.lifecycle.running = true
	ht.lifecycle.lock.Unlock()

	p.end("lease-1")
	leaseWaitFor(t, func() bool {
		_, signals := ht.lifecycle.counts()
		return len(signals) == 1
	})
	must.Eq(t, "creds-2-"+secretTestCanary, readCreds(t, ht))
	must.SliceContains(t, ht.events.messages(),
		"secret nats: lease lease-1 ended (provider norca: secret lease is revoked); requesting a new credential")
}

// TestSecretHook_DynamicSecretResume resumes the persisted lease of a
// restored task: no new credential, the delivered file stays, the lease is
// renewed.
func TestSecretHook_DynamicSecretResume(t *testing.T) {
	ci.Parallel(t)
	p := newTestLeaseProvider(300*time.Millisecond, time.Hour)
	ht := newSecretHookTest(t, secrets.NewRegistry(p, nil, nil), natsSecret(""))
	resp, err := ht.prestart(context.Background())
	must.NoError(t, err)
	ht.hook.Shutdown()
	_, _, revoked := p.counts()
	must.SliceEmpty(t, revoked, must.Sprint("the agent's shutdown does not revoke"))

	ht.lifecycle.running = true
	ht.hook = ht.newHook(secrets.NewRegistry(p, nil, nil))
	defer ht.hook.Shutdown()
	req := &interfaces.TaskPrestartRequest{Task: ht.task, TaskDir: ht.taskDir, PreviousState: resp.State}
	resp2 := &interfaces.TaskPrestartResponse{}
	must.NoError(t, ht.hook.Prestart(context.Background(), req, resp2))
	must.Eq(t, resp.State, resp2.State)

	renewals := func() int { _, r, _ := p.counts(); return r["lease-1"] }
	before := renewals()
	leaseWaitFor(t, func() bool { return renewals() >= before+2 })
	issued, _, _ := p.counts()
	must.Eq(t, 1, issued)
	must.Eq(t, "creds-1-"+secretTestCanary, readCreds(t, ht))
	restarts, _ := ht.lifecycle.counts()
	must.Eq(t, 0, restarts)
}

// TestSecretHook_DynamicSecretUnsupported fails a dynamic secret resolved to
// a provider without leases.
func TestSecretHook_DynamicSecretUnsupported(t *testing.T) {
	ci.Parallel(t)
	p := newTestSecretProvider("norca", map[string]string{"creds": "x"})
	ht := newSecretHookTest(t, secrets.NewRegistry(p, nil, nil), natsSecret(""))
	defer ht.hook.Shutdown()
	_, err := ht.prestart(context.Background())
	must.ErrorContains(t, err, "does not support dynamic secrets")
}
