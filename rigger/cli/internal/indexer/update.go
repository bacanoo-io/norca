// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package indexer

import (
	"context"
	"fmt"
	"slices"

	"github.com/bacanoo-io/norca/rigger/fetch"
	"github.com/bacanoo-io/norca/rigger/index"
	"github.com/bacanoo-io/norca/rigger/module"
	"github.com/bacanoo-io/norca/rigger/sign"
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

	u := &nsUpdate{ix: ix, namespace: name, advisories: advisories, entries: map[entryKey]*index.Entry{}, changed: map[entryKey]bool{}}

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

	// The stacks after the modules are written: a stack may use a module
	// listed in the same run.
	if err := u.write(index.KindModule); err != nil {
		return u.outcomes, err
	}

	for _, p := range u.stacks {
		u.list(ctx, p)
	}

	return u.outcomes, u.write(index.KindStack)
}

// entryKey names an entry of the namespace.
type entryKey struct {
	kind index.Kind
	name string
}

// pending is a tag to inspect.
type pending struct {
	url string
	vt  versionTag
	t   tree
}

// nsUpdate is the update of one namespace.
type nsUpdate struct {
	ix         *Indexer
	namespace  string
	keys       []sign.PublicKey
	advisories *index.Advisories
	entries    map[entryKey]*index.Entry
	changed    map[entryKey]bool
	stacks     []pending
	outcomes   []Outcome
}

// entry returns the entry of a module or stack, read once, new when there
// is none.
func (u *nsUpdate) entry(kind index.Kind, name string) (*index.Entry, error) {
	k := entryKey{kind: kind, name: name}
	if e, ok := u.entries[k]; ok {
		return e, nil
	}

	e, err := u.ix.readEntry(kind, u.namespace, name)
	if err != nil {
		return nil, err
	}

	if e == nil {
		e = &index.Entry{Namespace: u.namespace, Name: name}
	}

	u.entries[k] = e

	return e, nil
}

// listed returns the listed version of a name as a module or a stack.
func (u *nsUpdate) listed(name, v string) (*index.Version, error) {
	for _, kind := range []index.Kind{index.KindModule, index.KindStack} {
		e, err := u.entry(kind, name)
		if err != nil {
			return nil, err
		}

		if listed := e.Version(v); listed != nil {
			return listed, nil
		}
	}

	return nil, nil //nolint:nilnil // not listed
}

// tag lists the version of a tag, unless it is listed, blocked, or fails
// its checks; a stack waits for the modules.
func (u *nsUpdate) tag(ctx context.Context, repo index.Repository, tag fetch.Tag) error {
	vt, ok := parseTag(repo, tag.Name)
	if !ok {
		return nil
	}

	listed, err := u.listed(vt.name, vt.version)
	if err != nil {
		return err
	}

	o := Outcome{Namespace: u.namespace, Name: vt.name, Version: vt.version, Tag: tag.Name}

	if listed != nil {
		if listed.Repository == repo.URL && listed.Tag == tag.Name && listed.Commit != tag.Commit {
			o.Problem = fmt.Sprintf("the tag moved: listed at %s, now %s", listed.Commit, tag.Commit)
			u.outcomes = append(u.outcomes, o)
		}

		return nil
	}

	t, err := u.ix.readTree(ctx, repo.URL, vt)
	if err != nil {
		o.Problem = err.Error()
		u.outcomes = append(u.outcomes, o)

		return nil //nolint:nilerr // a tag that can't be read is an outcome, not a failure of the update
	}

	p := pending{url: repo.URL, vt: vt, t: t}
	if t.kind == index.KindStack {
		u.stacks = append(u.stacks, p)

		return nil
	}

	u.list(ctx, p)

	return nil
}

// list inspects a tag and adds its version to its entry.
func (u *nsUpdate) list(ctx context.Context, p pending) {
	o := Outcome{Namespace: u.namespace, Name: p.vt.name, Version: p.vt.version, Tag: p.vt.tag()}

	if adv, blocked := u.advisories.Blocked(p.t.kind, u.namespace, p.vt.name, p.vt.version); blocked {
		o.Problem = "blocked: " + adv.Reason
		u.outcomes = append(u.outcomes, o)

		return
	}

	v, err := u.ix.inspect(ctx, u.keys, p.url, p.vt, p.t)
	if err != nil {
		o.Problem = err.Error()
		u.outcomes = append(u.outcomes, o)

		return
	}

	entry, err := u.entry(p.t.kind, p.vt.name)
	if err != nil {
		o.Problem = err.Error()
		u.outcomes = append(u.outcomes, o)

		return
	}

	entry.Versions = append(entry.Versions, v)
	u.changed[entryKey{kind: p.t.kind, name: p.vt.name}], o.Listed, o.Kind = true, true, p.t.kind
	u.outcomes = append(u.outcomes, o)
}

// write writes the changed entries of a kind.
func (u *nsUpdate) write(kind index.Kind) error {
	var names []string

	for k := range u.changed {
		if k.kind == kind {
			names = append(names, k.name)
		}
	}

	slices.Sort(names)

	for _, n := range names {
		if err := u.ix.writeEntry(kind, u.entries[entryKey{kind: kind, name: n}]); err != nil {
			return err
		}
	}

	return nil
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
