// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package indexer

import (
	"context"
	"fmt"
	"slices"

	"github.com/bacanoo-io/norca-rig/cli/internal/fetch"
	"github.com/bacanoo-io/norca-rig/index"
	"github.com/bacanoo-io/norca-rig/module"
	"github.com/bacanoo-io/norca-rig/sign"
)

// Update adds to the entries of the namespaces the versions their
// repositories have tagged since: each checked as Check checks it, signed
// by a key of the namespace, not blocked. A listed version is never
// touched; a tag that moved is reported. No namespaces means all.
func (ix *Indexer) Update(ctx context.Context, namespaces []string) ([]Outcome, error) {
	if len(namespaces) == 0 {
		all, err := ix.Namespaces()
		if err != nil {
			return nil, err
		}

		namespaces = all
	}

	advisories, err := index.ReadAdvisories(ix.fsys())
	if err != nil {
		return nil, err
	}

	var outcomes []Outcome

	for _, name := range namespaces {
		out, err := ix.updateNamespace(ctx, name, advisories)
		if err != nil {
			return outcomes, fmt.Errorf("namespace %s: %w", name, err)
		}

		outcomes = append(outcomes, out...)
	}

	return outcomes, nil
}

func (ix *Indexer) updateNamespace(ctx context.Context, name string, advisories *index.Advisories) ([]Outcome, error) {
	ns, err := index.ReadNamespace(ix.fsys(), name)
	if err != nil {
		return nil, err
	}

	u := &nsUpdate{ix: ix, namespace: name, advisories: advisories, entries: map[string]*index.Entry{}, changed: map[string]bool{}}

	if u.keys, err = ix.keys(name); err != nil {
		return nil, err
	}

	for _, repo := range ns.Repositories {
		tags, err := ix.Fetcher.Tags(ctx, ix.location(repo.URL))
		if err != nil {
			return u.outcomes, fmt.Errorf("%s: %w", repo.URL, err)
		}

		for _, tag := range tags {
			if err := u.tag(ctx, repo, tag); err != nil {
				return u.outcomes, err
			}
		}
	}

	for _, n := range sortedKeys(u.changed) {
		if err := ix.writeEntry(index.KindModule, u.entries[n]); err != nil {
			return u.outcomes, err
		}
	}

	return u.outcomes, nil
}

// nsUpdate is the update of one namespace.
type nsUpdate struct {
	ix         *Indexer
	namespace  string
	keys       []sign.PublicKey
	advisories *index.Advisories
	entries    map[string]*index.Entry
	changed    map[string]bool
	outcomes   []Outcome
}

// entry returns the entry of a module, read once, new when there is none.
func (u *nsUpdate) entry(name string) (*index.Entry, error) {
	if e, ok := u.entries[name]; ok {
		return e, nil
	}

	e, err := u.ix.readEntry(index.KindModule, u.namespace, name)
	if err != nil {
		return nil, err
	}

	if e == nil {
		e = &index.Entry{Namespace: u.namespace, Name: name}
	}

	u.entries[name] = e

	return e, nil
}

// tag lists the version of a tag, unless it is listed, blocked, or fails
// its checks.
func (u *nsUpdate) tag(ctx context.Context, repo index.Repository, tag fetch.Tag) error {
	vt, ok := parseTag(repo, tag.Name)
	if !ok {
		return nil
	}

	entry, err := u.entry(vt.name)
	if err != nil {
		return err
	}

	o := Outcome{Namespace: u.namespace, Name: vt.name, Version: vt.version, Tag: tag.Name}

	if listed := entry.Version(vt.version); listed != nil {
		if listed.Repository == repo.URL && listed.Tag == tag.Name && listed.Commit != tag.Commit {
			o.Problem = fmt.Sprintf("the tag moved: listed at %s, now %s", listed.Commit, tag.Commit)
			u.outcomes = append(u.outcomes, o)
		}

		return nil
	}

	if adv, blocked := u.advisories.Blocked(index.KindModule, u.namespace, vt.name, vt.version); blocked {
		o.Problem = "blocked: " + adv.Reason
		u.outcomes = append(u.outcomes, o)

		return nil
	}

	v, err := u.ix.inspect(ctx, u.keys, repo.URL, vt, tag.Name)
	if err != nil {
		o.Problem = err.Error()
	} else {
		entry.Versions = append(entry.Versions, v)
		u.changed[vt.name], o.Listed = true, true
	}

	u.outcomes = append(u.outcomes, o)

	return nil
}

func sortedKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}

	slices.Sort(keys)

	return keys
}

// requires is the requirements of a version in the index.
func requires(norca string, features []string, modules []module.RequiredModule) *index.Requires {
	if norca == "" && len(features) == 0 && len(modules) == 0 {
		return nil
	}

	r := &index.Requires{Norca: norca, Features: features}

	for _, m := range modules {
		r.Modules = append(r.Modules, index.RequiredModule{Name: m.Name, Source: m.Source, Version: m.Version})
	}

	return r
}
