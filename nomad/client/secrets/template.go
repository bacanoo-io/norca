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
// The existing Vault "secret" template function is not served here: it
// keeps going to Vault through consul-template. Its fallback to the
// built-in store (phase 4) resolves with a Vault-style Lookup and maps the
// Vault KV paths onto the built-in store.
type TemplateSource struct {
	registry *Registry
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
	items       map[string]string
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

// Func is the nomadSecret template function: it returns the items of the
// secret. Errors never contain values.
func (s *TemplateSource) Func(ref string) (map[string]string, error) {
	s.lock.Lock()
	e, ok := s.entries[ref]
	if !ok {
		e = &templateEntry{ready: make(chan struct{})}
		s.entries[ref] = e
		go s.run(ref, e)
	}
	s.lock.Unlock()

	select {
	case <-e.ready:
	case <-s.ctx.Done():
		return nil, fmt.Errorf("nomadSecret %q: stopped", ref)
	}
	if e.err != nil {
		return nil, fmt.Errorf("nomadSecret %q: %v", ref, e.err)
	}
	e.lock.Lock()
	defer e.lock.Unlock()
	out := make(map[string]string, len(e.items))
	for k, v := range e.items {
		out[k] = v
	}
	return out, nil
}

// run fetches a path for the first time and then watches it.
func (s *TemplateSource) run(ref string, e *templateEntry) {
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

	fetch := func() (*Secret, error) {
		sec, err, aerr := FetchAudited(s.ctx, p, &Request{Path: path, Identity: s.identity()}, s.auditor)
		if aerr != nil {
			s.logger.Warn("failed to record the audit record of a secret fetch", "provider", p.Name(), "error", aerr)
		}
		return sec, err
	}

	deadline := time.Now().Add(s.timeout)
	var attempts uint64
	var sec *Secret
	for {
		sec, err = fetch()
		if err == nil {
			break
		}
		if s.ctx.Err() != nil || !IsRecoverable(err) || time.Now().After(deadline) {
			e.err = fmt.Errorf("%s: %v", res, err)
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
	e.items, e.fingerprint = sec.Items, Fingerprint(sec.Items)
	version := sec.Version
	close(e.ready)

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
			v, err := watcher.Watch(s.ctx, &Request{Path: path, Identity: s.identity()}, version)
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
		sec, err := fetch()
		if err != nil {
			s.logger.Warn("failed to fetch secret of a template, keeping the rendered value", "provider", p.Name(), "error", err)
			if !wait() {
				return
			}
			continue
		}
		attempts = 0
		version = sec.Version
		fp := Fingerprint(sec.Items)
		e.lock.Lock()
		changed := fp != e.fingerprint
		e.items, e.fingerprint = sec.Items, fp
		e.lock.Unlock()
		if changed {
			select {
			case s.changes <- struct{}{}:
			default:
			}
		}
	}
}
