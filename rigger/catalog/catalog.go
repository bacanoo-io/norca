// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Package catalog lists what a registry offers: the modules and stacks of
// the global registry (its index, design 7.3) or of a registry repository
// (design 7.4), with their versions, for a host that keeps registries
// synced and shows or searches them (design/rig-integration.md 4.5 of
// norca). It reads files and tags it is given; it fetches nothing.
package catalog

import (
	"cmp"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"strings"

	"github.com/bacanoo-io/norca/rigger/index"
	"github.com/bacanoo-io/norca/rigger/internal/lang"
	"github.com/bacanoo-io/norca/rigger/module"
	"github.com/bacanoo-io/norca/rigger/version"
)

// Ref names a module or a stack of a registry. The namespace is empty in a
// registry repository, which has none.
type Ref struct {
	Kind      index.Kind `json:"kind"`
	Namespace string     `json:"namespace,omitempty"`
	Name      string     `json:"name"`
}

// String is "module acme/nats", "stack web".
func (r Ref) String() string {
	if r.Namespace == "" {
		return string(r.Kind) + " " + r.Name
	}

	return string(r.Kind) + " " + r.Namespace + "/" + r.Name
}

// Item is a module or a stack of a registry and its versions, the newest
// first.
type Item struct {
	Ref

	Versions []string `json:"versions"`

	// Description, Category, Keywords and License are those of the newest
	// version, of the index of the global registry, or of the metadata.hcl
	// of a registry repository at its commit.
	Description string   `json:"description,omitempty"`
	Category    string   `json:"category,omitempty"`
	Keywords    []string `json:"keywords,omitempty"`
	License     string   `json:"license,omitempty"`
}

// Global lists the modules and stacks of the index of the global registry
// (a registry of its shape): every entry, checked as the CI of the registry
// checks it, without the versions its advisories block. An entry that does
// not pass is an error naming its file.
func Global(fsys fs.FS) ([]Item, error) {
	adv, err := index.ReadAdvisories(fsys)
	if err != nil {
		return nil, err
	}

	var out []Item

	for _, kind := range []index.Kind{index.KindModule, index.KindStack} {
		items, err := globalKind(fsys, kind, adv)
		if err != nil {
			return nil, err
		}

		out = append(out, items...)
	}

	sortItems(out)

	return out, nil
}

// globalKind lists the entries of a kind: <dir>/<letter>/<namespace>/<name>.json.
func globalKind(fsys fs.FS, kind index.Kind, adv *index.Advisories) ([]Item, error) {
	top := path.Dir(path.Dir(path.Dir(index.EntryPath(kind, "x", "x"))))

	files, err := fs.Glob(fsys, top+"/*/*/*.json")
	if err != nil {
		return nil, err
	}

	out := make([]Item, 0, len(files))

	for _, file := range files {
		namespace := path.Base(path.Dir(file))
		name := strings.TrimSuffix(path.Base(file), ".json")

		if index.EntryPath(kind, namespace, name) != file {
			return nil, fmt.Errorf("%s: %w: not where an entry of %s/%s belongs", file, index.ErrInvalid, namespace, name)
		}

		entry, err := index.ReadEntry(fsys, kind, namespace, name)
		if err != nil {
			return nil, err // it names the file
		}

		item := Item{Kind: kind, Namespace: namespace, Name: name}

		for _, v := range entry.Versions {
			if _, blocked := adv.Blocked(kind, namespace, name, v.Version); !blocked {
				item.Versions = append(item.Versions, v.Version)
			}
		}

		if len(item.Versions) > 0 {
			sortVersions(item.Versions)

			if newest := entry.Version(item.Versions[0]); newest != nil {
				item.Description, item.Category, item.License = newest.Description, newest.Category, newest.License
				item.Keywords = slices.Clone(newest.Keywords)
			}

			out = append(out, item)
		}
	}

	return out, nil
}

// Repository lists the modules and stacks of a registry repository at a
// commit: the directories modules/<name> and stacks/<name> with a
// metadata.hcl, and as versions the tags <name>/v<version> of that commit
// (fetch.Fetcher.TagsAt). A module or stack without a version is listed
// with none: it can't be installed yet.
func Repository(fsys fs.FS, tags []string) ([]Item, error) {
	versions := map[string][]string{}

	for _, tag := range tags {
		name, v, ok := strings.Cut(tag, "/v")
		if !ok {
			continue
		}

		if _, err := version.Parse(v); err == nil {
			versions[name] = append(versions[name], v)
		}
	}

	var out []Item

	for _, kind := range []index.Kind{index.KindModule, index.KindStack} {
		dirs, err := fs.Glob(fsys, path.Join(topDir(kind), "*", "metadata.hcl"))
		if err != nil {
			return nil, err
		}

		for _, file := range dirs {
			name := path.Base(path.Dir(file))
			item := Item{Kind: kind, Name: name, Versions: slices.Clone(versions[name])}
			sortVersions(item.Versions)

			// What it says of itself at the commit; one that does not read
			// is still listed, as a load names its error.
			if md, diags := lang.ReadMetadata(fsys, path.Dir(file)); !diags.HasErrors() {
				item.Description, item.Category, item.Keywords, item.License = md.Description, md.Category, md.Keywords, md.License
			}

			out = append(out, item)
		}
	}

	sortItems(out)

	return out, nil
}

// topDir is the directory of the modules or stacks of a registry repository.
func topDir(kind index.Kind) string {
	if kind == index.KindStack {
		return "stacks"
	}

	return "modules"
}

// Query is a search of the items of a registry.
type Query struct {
	// Kind keeps one kind; empty keeps both.
	Kind index.Kind
	// Text is found in "namespace/name" or in the description,
	// case-insensitively; empty finds every item.
	Text string
	// Category keeps the items of one category (module.Categories);
	// Keyword those with the keyword.
	Category, Keyword string
	// Offset and Limit give one page: Limit 0 is DefaultLimit, and at most
	// MaxLimit.
	Offset, Limit int
}

// The size of a page of a search.
const (
	DefaultLimit = 50
	MaxLimit     = 500
)

// ErrQuery is the error of a query with a negative offset or limit, or a
// kind that is not one.
var ErrQuery = errors.New("invalid query")

// Search returns a page of the items that match the query, in the order of
// the list, and how many match in all.
func Search(items []Item, q Query) ([]Item, int, error) {
	if q.Offset < 0 || q.Limit < 0 || (q.Kind != "" && q.Kind != index.KindModule && q.Kind != index.KindStack) {
		return nil, 0, fmt.Errorf("%w: offset %d, limit %d, kind %q", ErrQuery, q.Offset, q.Limit, q.Kind)
	}

	if q.Category != "" && !slices.Contains(module.Categories, q.Category) {
		return nil, 0, fmt.Errorf("%w: category %q is none of %s", ErrQuery, q.Category, strings.Join(module.Categories, ", "))
	}

	limit := cmp.Or(q.Limit, DefaultLimit)
	limit = min(limit, MaxLimit)
	text := strings.ToLower(q.Text)

	var (
		page  []Item
		total int
	)

	for _, item := range items {
		if !q.matches(item, text) {
			continue
		}

		if total >= q.Offset && len(page) < limit {
			page = append(page, item)
		}

		total++
	}

	return page, total, nil
}

// matches reports whether an item is one the query asks for; text is the
// text of the query in lower case.
func (q Query) matches(item Item, text string) bool {
	switch {
	case q.Kind != "" && item.Kind != q.Kind:
		return false
	case q.Category != "" && item.Category != q.Category:
		return false
	case q.Keyword != "" && !slices.Contains(item.Keywords, q.Keyword):
		return false
	case text == "":
		return true
	default:
		return strings.Contains(strings.ToLower(item.Namespace+"/"+item.Name), text) ||
			strings.Contains(strings.ToLower(item.Description), text)
	}
}

// Select returns the items of the list that are selected, in the order of
// the list, and the selected refs that the list lacks, in the order given:
// what a registry shows in the UI after a sync.
func Select(items []Item, selected []Ref) (shown []Item, missing []Ref) {
	want := make(map[Ref]bool, len(selected))
	for _, ref := range selected {
		want[ref] = true
	}

	found := map[Ref]bool{}

	for _, item := range items {
		if want[item.Ref] {
			shown = append(shown, item)
			found[item.Ref] = true
		}
	}

	for _, ref := range selected {
		if !found[ref] {
			missing = append(missing, ref)
		}
	}

	return shown, missing
}

// sortItems orders items by kind (modules first), namespace and name.
func sortItems(items []Item) {
	slices.SortFunc(items, func(a, b Item) int {
		return cmp.Or(
			cmp.Compare(kindOrder(a.Kind), kindOrder(b.Kind)),
			cmp.Compare(a.Namespace, b.Namespace),
			cmp.Compare(a.Name, b.Name),
		)
	})
}

func kindOrder(k index.Kind) int {
	if k == index.KindStack {
		return 1
	}

	return 0
}

// sortVersions orders versions the newest first; one that does not parse
// goes last.
func sortVersions(vs []string) {
	slices.SortStableFunc(vs, func(a, b string) int {
		va, errA := version.Parse(a)
		vb, errB := version.Parse(b)

		switch {
		case errA != nil || errB != nil:
			return cmp.Compare(boolInt(errA != nil), boolInt(errB != nil))
		default:
			return version.Compare(vb, va)
		}
	})
}

func boolInt(b bool) int {
	if b {
		return 1
	}

	return 0
}
