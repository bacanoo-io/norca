// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package secrets

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/hashicorp/nomad/client/config"
	"github.com/hashicorp/nomad/nomad/structs"
)

// Source is the resolution step that defined the provider of a lookup.
type Source string

const (
	SourceExplicit         Source = "explicit"
	SourceRoute            Source = "route"
	SourceGroupDefault     Source = "group default"
	SourceJobDefault       Source = "job default"
	SourceNamespaceDefault Source = "namespace default"
	SourceVaultIntegration Source = "vault integration"
	SourceFallback         Source = "fallback"
)

// Lookup describes a secret lookup for the resolution of its provider.
type Lookup struct {
	// Explicit is the provider named by the secret block or the
	// "provider:" prefix of a template call (step 1).
	Explicit string

	// Path is the path, placeholders replaced; client routes match on it
	// (step 2).
	Path string

	// GroupDefault, JobDefault and NamespaceDefault are the default
	// providers of the task's group, job and namespace (step 3).
	GroupDefault     string
	JobDefault       string
	NamespaceDefault string

	// VaultStyle marks a Vault-style lookup (a task vault block or a Vault
	// "secret" template call): with the agent's Vault integration enabled
	// it goes to Vault (step 4). Secret blocks and nomadSecret calls are
	// not Vault-style.
	VaultStyle bool
}

// LookupFor returns the lookup of a secret path of a task of the job: the
// defaults of its group, job and namespace.
func LookupFor(job *structs.Job, group, explicit, path string) *Lookup {
	l := &Lookup{Explicit: explicit, Path: path}
	if job == nil {
		return l
	}
	if tg := job.LookupTaskGroup(group); tg != nil && tg.Secrets != nil {
		l.GroupDefault = tg.Secrets.DefaultProvider
	}
	if job.Secrets != nil {
		l.JobDefault = job.Secrets.DefaultProvider
		l.NamespaceDefault = job.Secrets.NamespaceDefaultProvider
	}
	return l
}

// Resolution is the provider that answers a lookup and the step that
// defined it.
type Resolution struct {
	Provider Provider
	Source   Source
}

// String is the form shown in task events and the allocation status:
// "provider norca (fallback)".
func (r *Resolution) String() string {
	return fmt.Sprintf("provider %s (%s)", r.Provider.Name(), r.Source)
}

// Registry holds the providers of a client and resolves lookups to them.
type Registry struct {
	lock      sync.RWMutex
	providers map[string]Provider
	routes    []*config.SecretsRoute
	vault     Provider
}

// NewRegistry returns a registry with the given providers. The built-in
// store must be one of them: it is the fallback. vault is the provider of
// the agent's Vault integration, nil if it is not enabled.
func NewRegistry(norca Provider, vault Provider, routes []*config.SecretsRoute) *Registry {
	r := &Registry{providers: map[string]Provider{}}
	if norca != nil {
		r.providers[norca.Name()] = norca
	}
	if vault != nil {
		r.providers[vault.Name()] = vault
		r.vault = vault
	}
	r.routes = make([]*config.SecretsRoute, 0, len(routes))
	for _, rt := range routes {
		if rt != nil {
			r.routes = append(r.routes, rt)
		}
	}
	// Longest prefix first.
	sort.SliceStable(r.routes, func(i, j int) bool {
		return len(r.routes[i].Prefix) > len(r.routes[j].Prefix)
	})
	return r
}

// Register adds a provider (external providers, later phases).
func (r *Registry) Register(p Provider) {
	r.lock.Lock()
	defer r.lock.Unlock()
	r.providers[p.Name()] = p
}

// Provider returns the provider of the name.
func (r *Registry) Provider(name string) (Provider, bool) {
	r.lock.RLock()
	defer r.lock.RUnlock()
	p, ok := r.providers[name]
	return p, ok
}

// Known reports whether a provider of the name is configured.
func (r *Registry) Known(name string) bool {
	_, ok := r.Provider(name)
	return ok
}

// Resolve returns the provider of a lookup, in the order of the plan: the
// explicit provider, the client routes, the group, job and namespace
// defaults, the Vault integration for Vault-style lookups, and the built-in
// store as the fallback. The fallback applies only when no step defines a
// provider: a step that names a provider this client does not have fails
// the lookup, it never falls through. Errors of the provider itself are
// returned by the provider when the lookup is made, never resolved around.
func (r *Registry) Resolve(l *Lookup) (*Resolution, error) {
	named := func(name string, src Source) (*Resolution, error) {
		p, ok := r.Provider(name)
		if !ok {
			return nil, NewError(name, KindInvalid,
				fmt.Errorf("provider %q (%s) is not configured on this client", name, src))
		}
		return &Resolution{Provider: p, Source: src}, nil
	}

	if l.Explicit != "" {
		return named(l.Explicit, SourceExplicit)
	}
	for _, rt := range r.routes {
		if strings.HasPrefix(l.Path, rt.Prefix) {
			return named(rt.Provider, SourceRoute)
		}
	}
	switch {
	case l.GroupDefault != "":
		return named(l.GroupDefault, SourceGroupDefault)
	case l.JobDefault != "":
		return named(l.JobDefault, SourceJobDefault)
	case l.NamespaceDefault != "":
		return named(l.NamespaceDefault, SourceNamespaceDefault)
	}
	if l.VaultStyle && r.vault != nil {
		return &Resolution{Provider: r.vault, Source: SourceVaultIntegration}, nil
	}
	// Phase 4 hook point: Vault-style lookups without a Vault integration
	// reach this fallback and are served by the built-in store through the
	// Vault path mapping.
	return named(structs.SecretProviderNorca, SourceFallback)
}
