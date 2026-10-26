// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package secrets

import (
	"context"
	"fmt"
	"sync"
	"time"

	log "github.com/hashicorp/go-hclog"

	"github.com/hashicorp/nomad/helper"
	"github.com/hashicorp/nomad/helper/uuid"
	"github.com/hashicorp/nomad/nomad/structs"
)

// TemplateSource serves the nomadSecret template function of a task:
// {{ with nomadSecret "prod/db" }}{{ .password }}{{ end }}, optionally with a
// "provider:" prefix. Lookups follow the same resolution order as secret
// blocks. The first call of a path fetches the secret (waiting up to the
// timeout while the provider is unavailable or the secret is missing);
// afterwards the secret is watched and Changes signals new values, upon
// which the template manager makes the templates render again.
//
// When the agent has no Vault integration (phase 4, "Vault compatibility"),
// the source also serves the Vault "secret" template function in place of
// consul-template's: {{ with secret "kv/data/app/db" }}{{ .Data.data.password
// }}{{ end }} resolves as a Vault-style lookup and, from the built-in store,
// returns the Vault KV response shape (VaultReader). With a Vault integration
// the Vault function stays consul-template's and goes to Vault.
type TemplateSource struct {
	registry *Registry
	vault    *VaultReader
	auditor  Auditor
	logger   log.Logger
	timeout  time.Duration

	// lookup returns the lookup of a path (the job, group and namespace
	// defaults), identity the task's identity, interpolate replaces the
	// placeholders of a path.
	lookup      func(explicit, path string) *Lookup
	identity    func() *Identity
	interpolate func(string) string

	ctx    context.Context
	cancel context.CancelFunc

	lock    sync.Mutex
	entries map[string]*templateEntry
	changes chan struct{}
}

type templateEntry struct {
	ready chan struct{}
	err   error

	lock        sync.Mutex
	value       any // map[string]string (nomadSecret) or *VaultKV (secret)
	fingerprint string
}

// TemplateSourceConfig configures a TemplateSource.
type TemplateSourceConfig struct {
	Registry    *Registry
	Auditor     Auditor
	Logger      log.Logger
	Lookup      func(explicit, path string) *Lookup
	Identity    func() *Identity
	Interpolate func(string) string

	// Vault serves the Vault "secret" template function; nil leaves it to
	// consul-template (the agent has a Vault integration).
	Vault *VaultReader

	// Timeout bounds the wait for the first fetch of a path; 0 is
	// structs.DefaultSecretTimeout.
	Timeout time.Duration
}

// NewTemplateSource returns the source of a task's nomadSecret calls.
func NewTemplateSource(c *TemplateSourceConfig) *TemplateSource {
	ctx, cancel := context.WithCancel(context.Background())
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = structs.DefaultSecretTimeout
	}
	logger := c.Logger
	if logger == nil {
		logger = log.NewNullLogger()
	}
	interpolate := c.Interpolate
	if interpolate == nil {
		interpolate = func(s string) string { return s }
	}
	return &TemplateSource{
		registry:    c.Registry,
		vault:       c.Vault,
		auditor:     c.Auditor,
		logger:      logger,
		timeout:     timeout,
		lookup:      c.Lookup,
		identity:    c.Identity,
		interpolate: interpolate,
		ctx:         ctx,
		cancel:      cancel,
		entries:     map[string]*templateEntry{},
		changes:     make(chan struct{}, 1),
	}
}

// Changes is signaled when a secret used by the templates changed.
func (s *TemplateSource) Changes() <-chan struct{} { return s.changes }

// Stop stops the watches.
func (s *TemplateSource) Stop() { s.cancel() }

// ServesVault reports whether the source serves the Vault "secret" template
// function.
func (s *TemplateSource) ServesVault() bool { return s != nil && s.vault != nil }

// entry returns the entry of a key, starting its fetch on the first call,
// and waits for the first fetch.
func (s *TemplateSource) entry(key string, start func(*templateEntry)) (*templateEntry, error) {
	s.lock.Lock()
	e, ok := s.entries[key]
	if !ok {
		e = &templateEntry{ready: make(chan struct{})}
		s.entries[key] = e
		go start(e)
	}
	s.lock.Unlock()

	select {
	case <-e.ready:
	case <-s.ctx.Done():
		return nil, fmt.Errorf("stopped")
	}
	if e.err != nil {
		return nil, e.err
	}
	return e, nil
}

// Func is the nomadSecret template function: it returns the items of the
// secret. Errors never contain values.
func (s *TemplateSource) Func(ref string) (map[string]string, error) {
	e, err := s.entry(ref, func(e *templateEntry) { s.runNomadSecret(ref, e) })
	if err != nil {
		return nil, fmt.Errorf("nomadSecret %q: %v", ref, err)
	}
	e.lock.Lock()
	defer e.lock.Unlock()
	items := e.value.(map[string]string)
	out := make(map[string]string, len(items))
	for k, v := range items {
		out[k] = v
	}
	return out, nil
}

// VaultFunc is the Vault "secret" template function served by the source:
// it returns the Vault response of the path. Additional arguments would
// write to Vault, which the built-in store doesn't support. Errors never
// contain values.
func (s *TemplateSource) VaultFunc(args ...string) (interface{}, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("secret: missing path")
	}
	ref := args[0]
	if len(args) > 1 {
		return nil, fmt.Errorf("secret %q: writes (secret with data arguments) are not supported by the "+
			"built-in secret store; they require a Vault/OpenBao provider", ref)
	}
	e, err := s.entry(vaultRefKey(ref), func(e *templateEntry) { s.runVault(ref, e) })
	if err != nil {
		return nil, fmt.Errorf("secret %q: %v", ref, err)
	}
	e.lock.Lock()
	kv := e.value.(*VaultKV).Copy()
	e.lock.Unlock()
	return &VaultTemplateSecret{
		RequestID:     uuid.Generate(),
		LeaseDuration: kv.LeaseDuration,
		Data:          kv.Data,
	}, nil
}

// VaultListFunc is the Vault "secrets" template function served by the
// source: listing is not supported by the built-in store.
func (s *TemplateSource) VaultListFunc(args ...string) ([]string, error) {
	ref := ""
	if len(args) > 0 {
		ref = args[0]
	}
	return nil, fmt.Errorf("secrets %q: listing Vault paths is not supported by the built-in secret store; "+
		"it requires a Vault/OpenBao provider", ref)
}

// templateFetch is one way of fetching the value of an entry.
type templateFetch struct {
	res *Resolution

	// request is the provider request of the watch.
	request func() *Request

	// fetch fetches the value with its fingerprint and version.
	fetch func() (value any, fingerprint, version string, err error)

	// pinned values never change.
	pinned bool
}

// runNomadSecret fetches and watches the secret of a nomadSecret call.
func (s *TemplateSource) runNomadSecret(ref string, e *templateEntry) {
	explicit, path := structs.ParseSecretRef(ref)
	path = s.interpolate(path)
	res, err := s.registry.Resolve(s.lookup(explicit, path))
	if err != nil {
		e.err = err
		close(e.ready)
		return
	}
	p := res.Provider
	s.logger.Debug("nomadSecret provider resolved", "path", path, "provider", p.Name(), "source", res.Source)
	s.run(e, &templateFetch{
		res:     res,
		request: func() *Request { return &Request{Path: path, Identity: s.identity()} },
		fetch: func() (any, string, string, error) {
			sec, err, aerr := FetchAudited(s.ctx, p, &Request{Path: path, Identity: s.identity()}, s.auditor)
			if aerr != nil {
				s.logger.Warn("failed to record the audit record of a secret fetch", "provider", p.Name(), "error", aerr)
			}
			if err != nil {
				return nil, "", "", err
			}
			return sec.Items, Fingerprint(sec.Items), sec.Version, nil
		},
	})
}

// runVault fetches and watches the path of a Vault "secret" call.
func (s *TemplateSource) runVault(ref string, e *templateEntry) {
	path, version, err := ParseVaultRef(s.interpolate(ref))
	if err == nil {
		var vl *VaultLookup
		vl, err = s.vault.Resolve(s.lookup("", path), version)
		if err == nil {
			s.logger.Debug("Vault secret provider resolved", "path", path,
				"provider", vl.Resolution.Provider.Name(), "source", vl.Resolution.Source)
			s.run(e, &templateFetch{
				res:     vl.Resolution,
				request: func() *Request { return vl.Request(s.identity()) },
				fetch: func() (any, string, string, error) {
					kv, err := s.vault.Read(s.ctx, vl, s.identity())
					if err != nil {
						return nil, "", "", err
					}
					return kv, kv.Fingerprint(), kv.Version, nil
				},
				pinned: version != 0,
			})
			return
		}
	}
	e.err = err
	close(e.ready)
}

// run fetches an entry for the first time and then watches it.
func (s *TemplateSource) run(e *templateEntry, f *templateFetch) {
	p := f.res.Provider
	deadline := time.Now().Add(s.timeout)
	var attempts uint64
	var value any
	var fingerprint, version string
	var err error
	for {
		value, fingerprint, version, err = f.fetch()
		if err == nil {
			break
		}
		if s.ctx.Err() != nil || !IsRecoverable(err) || time.Now().After(deadline) {
			e.err = fmt.Errorf("%s: %v", f.res, err)
			close(e.ready)
			return
		}
		backoff := helper.Backoff(time.Second, 30*time.Second, attempts)
		attempts++
		select {
		case <-s.ctx.Done():
			e.err = s.ctx.Err()
			close(e.ready)
			return
		case <-time.After(backoff):
		}
	}
	e.value, e.fingerprint = value, fingerprint
	close(e.ready)
	if f.pinned {
		return
	}

	watcher, canWatch := p.(Watcher)
	canWatch = canWatch && p.Capabilities().Watch
	interval := p.Capabilities().PollInterval
	if interval <= 0 {
		interval = 10 * time.Second
	}
	attempts = 0
	wait := func() bool {
		backoff := helper.Backoff(time.Second, 30*time.Second, attempts)
		attempts++
		select {
		case <-s.ctx.Done():
			return false
		case <-time.After(backoff):
			return true
		}
	}
	for s.ctx.Err() == nil {
		if canWatch {
			v, err := watcher.Watch(s.ctx, f.request(), version)
			if err != nil {
				s.logger.Warn("failed to watch secret of a template", "provider", p.Name(), "error", err)
				if !wait() {
					return
				}
				continue
			}
			if v == version {
				continue
			}
		} else {
			select {
			case <-s.ctx.Done():
				return
			case <-time.After(interval):
			}
		}
		value, fp, v, err := f.fetch()
		if err != nil {
			s.logger.Warn("failed to fetch secret of a template, keeping the rendered value", "provider", p.Name(), "error", err)
			if !wait() {
				return
			}
			continue
		}
		attempts = 0
		version = v
		e.lock.Lock()
		changed := fp != e.fingerprint
		e.value, e.fingerprint = value, fp
		e.lock.Unlock()
		if changed {
			select {
			case s.changes <- struct{}{}:
			default:
			}
		}
	}
}
