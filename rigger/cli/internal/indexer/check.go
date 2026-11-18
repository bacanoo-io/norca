// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package indexer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os/exec"
	"path"
	"slices"
	"strings"

	"github.com/bacanoo-io/norca-rig/index"
	"github.com/bacanoo-io/norca-rig/sign"
)

// listedEntry is an entry of the registry and its kind.
type listedEntry struct {
	kind  index.Kind
	entry *index.Entry
	path  string
}

// Check checks the files of the registry and returns the problems found:
// every file in its format, every key parses, every entry belongs to a
// namespace. With base, a git revision of the checkout (the target of a
// pull request), the change since base only adds: no listed version or
// advisory is changed or removed, and the versions added are checked
// against their sources. Without base every listed version is.
func (ix *Indexer) Check(ctx context.Context, base string) ([]string, error) {
	var problems []string

	report := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }

	namespaces, err := ix.Namespaces()
	if err != nil {
		return nil, err
	}

	nsFiles := map[string]*index.Namespace{}
	keys := map[string][]sign.PublicKey{}

	for _, name := range namespaces {
		ns, err := index.ReadNamespace(ix.fsys(), name)
		if err != nil {
			report("%v", err)

			continue
		}

		if keys[name], err = ix.keys(name); err != nil {
			report("%v", err)
		}

		nsFiles[name] = ns
	}

	ix.checkKeyDirs(nsFiles, report)

	entries := ix.readEntries(nsFiles, report)

	advisories, err := index.ReadAdvisories(ix.fsys())
	if err != nil {
		report("%v", err)

		advisories = &index.Advisories{}
	}

	added := map[string][]string{} // entry path → versions to check

	if base != "" {
		if err := ix.checkAppendOnly(ctx, base, entries, advisories, added, report); err != nil {
			return problems, err
		}
	} else {
		for _, le := range entries {
			for _, v := range le.entry.Versions {
				added[le.path] = append(added[le.path], v.Version)
			}
		}
	}

	for _, le := range entries {
		for _, v := range added[le.path] {
			ix.checkVersion(ctx, le, le.entry.Version(v), nsFiles[le.entry.Namespace], keys[le.entry.Namespace], advisories, report)
		}
	}

	return problems, nil
}

// checkKeyDirs reports a directory of keys of no namespace.
func (ix *Indexer) checkKeyDirs(namespaces map[string]*index.Namespace, report func(string, ...any)) {
	dirs, err := fs.ReadDir(ix.fsys(), "keys")
	if err != nil {
		return
	}

	for _, d := range dirs {
		if _, ok := namespaces[d.Name()]; !ok {
			report("keys/%s: no namespace %s", d.Name(), d.Name())
		}
	}
}

// readEntries reads the entries of modules and stacks, each in the place
// its namespace and name give it, of a namespace of the registry.
func (ix *Indexer) readEntries(namespaces map[string]*index.Namespace, report func(string, ...any)) []listedEntry {
	var out []listedEntry

	for _, kind := range []index.Kind{index.KindModule, index.KindStack} {
		top := path.Dir(path.Dir(path.Dir(index.EntryPath(kind, "x", "x"))))

		_ = fs.WalkDir(ix.fsys(), top, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil //nolint:nilerr // a missing top directory has no entries
			}

			parts := strings.Split(p, "/")
			name, ok := strings.CutSuffix(path.Base(p), ".json")

			if len(parts) != 4 || !ok || index.EntryPath(kind, parts[2], name) != p {
				report("%s: not the place of an entry", p)

				return nil
			}

			if _, listed := namespaces[parts[2]]; !listed {
				report("%s: no namespace %s", p, parts[2])

				return nil
			}

			e, err := index.ReadEntry(ix.fsys(), kind, parts[2], name)
			if err != nil {
				report("%v", err)

				return nil
			}

			out = append(out, listedEntry{kind: kind, entry: e, path: p})

			return nil
		})
	}

	return out
}

// checkAppendOnly compares the entries and advisories with those of base,
// and records the versions added since.
func (ix *Indexer) checkAppendOnly(ctx context.Context, base string, entries []listedEntry, advisories *index.Advisories, added map[string][]string, report func(string, ...any)) error {
	out, err := ix.git(ctx, "ls-tree", "-r", "--name-only", base, "--", "modules", "stacks", index.AdvisoriesFile)
	if err != nil {
		return err
	}

	current := map[string]*index.Entry{}
	for _, le := range entries {
		current[le.path] = le.entry
	}

	before := map[string]*index.Entry{}

	for p := range strings.Lines(string(out)) {
		p = strings.TrimSpace(p)

		data, err := ix.git(ctx, "show", base+":"+p)
		if err != nil {
			return err
		}

		if p == index.AdvisoriesFile {
			if err := compareAdvisories(data, advisories, report); err != nil {
				return fmt.Errorf("%s at %s: %w", p, base, err)
			}

			continue
		}

		var old index.Entry
		if err := json.Unmarshal(data, &old); err != nil {
			return fmt.Errorf("%s at %s: %w", p, base, err)
		}

		before[p] = &old

		if next, ok := current[p]; !ok {
			report("%s: %v (the entry is removed)", p, index.ErrChanged)
		} else if err := index.CheckAppendOnly(&old, next); err != nil {
			report("%s: %v", p, err)
		}
	}

	addedSince(before, current, added)

	return nil
}

// addedSince records the versions of the current entries that the entries
// before did not list.
func addedSince(before, current map[string]*index.Entry, added map[string][]string) {
	for p, e := range current {
		for _, v := range e.Versions {
			if old := before[p]; old == nil || old.Version(v.Version) == nil {
				added[p] = append(added[p], v.Version)
			}
		}
	}
}

// compareAdvisories reports an advisory of the file at base that is no
// longer listed.
func compareAdvisories(data []byte, advisories *index.Advisories, report func(string, ...any)) error {
	var old index.Advisories
	if err := json.Unmarshal(data, &old); err != nil {
		return err
	}

	if err := index.CheckAdvisoriesAppendOnly(&old, advisories); err != nil {
		report("%v", err)
	}

	return nil
}

// checkVersion checks a listed version against its source: a repository of
// its namespace, the tag of its layout pointing to the commit, the digest,
// path, requirements and icon as the source gives them, and every
// signature verifying with the key it names.
func (ix *Indexer) checkVersion(ctx context.Context, le listedEntry, v *index.Version, ns *index.Namespace, keys []sign.PublicKey, advisories *index.Advisories, report func(string, ...any)) {
	e := le.entry
	where := fmt.Sprintf("%s %s", le.path, v.Version)

	if _, blocked := advisories.Blocked(le.kind, e.Namespace, e.Name, v.Version); blocked {
		report("%s: the version is blocked", where)

		return
	}

	if le.kind == index.KindStack {
		report("%s: %v", where, errStack)

		return
	}

	i := slices.IndexFunc(ns.Repositories, func(r index.Repository) bool { return r.URL == v.Repository })
	if i < 0 {
		report("%s: %s is not a repository of the namespace", where, v.Repository)

		return
	}

	repo := ns.Repositories[i]

	tag, dir := expectedTag(repo, e.Name, v.Version)
	if v.Tag != tag || v.Path != dir {
		report("%s: the tag and path of the layout %s are %s and %s", where, repo.Layout, tag, dir)

		return
	}

	got, err := ix.inspect(ctx, keys, v.Repository, versionTag{name: e.Name, version: v.Version, dir: dir, kind: le.kind}, tag)
	if err != nil && !errors.Is(err, errNotSigned) {
		report("%s: %v", where, err)

		return
	}

	switch {
	case got.Commit != v.Commit:
		report("%s: the tag %s points to %s, the entry lists %s", where, tag, got.Commit, v.Commit)
	case got.Digest != v.Digest:
		report("%s: the digest of the source is %s, the entry lists %s", where, got.Digest, v.Digest)
	case got.Icon != v.Icon:
		report("%s: the icon of the source is %q, the entry lists %q", where, got.Icon, v.Icon)
	case !sameJSON(got.Requires, v.Requires):
		report("%s: the requirements are not those of the source", where)
	}

	ix.checkSignatures(where, e.Name, v, keys, report)
}

// checkSignatures checks that every listed signature verifies with the key
// of the namespace it names.
func (ix *Indexer) checkSignatures(where, name string, v *index.Version, keys []sign.PublicKey, report func(string, ...any)) {
	statement, err := sign.Statement(v.Digest, name, v.Version)
	if err != nil {
		report("%s: %v", where, err)

		return
	}

	for _, s := range v.Signatures {
		i := slices.IndexFunc(keys, func(k sign.PublicKey) bool { return k.ID == s.KeyID })
		if i < 0 {
			report("%s: %s is not a key of the namespace", where, s.KeyID)

			continue
		}

		if len(sign.Verify(v.Digest, []sign.Signature{{Payload: statement, Signature: s.Signature}}, keys[i:i+1])) == 0 {
			report("%s: the signature of %s does not verify", where, s.KeyID)
		}
	}
}

func sameJSON(a, b any) bool {
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)

	return bytes.Equal(ja, jb)
}

// git runs git in the checkout of the registry.
func (ix *Indexer) git(ctx context.Context, args ...string) ([]byte, error) {
	var stderr bytes.Buffer

	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", ix.Dir}, args...)...) //nolint:gosec // G204: git of the registry checkout
	cmd.Stderr = &stderr

	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}

	return out, nil
}
