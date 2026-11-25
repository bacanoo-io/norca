// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package catalog_test

import (
	"errors"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/bacanoo-io/norca/rigger/catalog"
	"github.com/bacanoo-io/norca/rigger/index"
)

const (
	acme   = "acme"
	norca  = "norca"
	nats   = "nats"
	redis  = "redis"
	commit = "0123456789abcdef0123456789abcdef01234567"
	digest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
)

// entry is the file of an entry with the versions, as the global registry
// lists them.
func entry(t *testing.T, namespace, name string, versions ...string) *fstest.MapFile {
	t.Helper()

	e := index.Entry{Namespace: namespace, Name: name}
	for _, v := range versions {
		e.Versions = append(e.Versions, index.Version{
			Version: v, Repository: "github.com/" + namespace + "/rigger", Path: "modules/" + name, Tag: name + "/v" + v,
			Commit: commit, Digest: digest, Signatures: []index.Signature{{KeyID: digest, Signature: "MEUCIQ=="}}, Listed: "2026-11-22",
			Description: summaries[name].description + " " + v, Category: summaries[name].category,
			Keywords: []string{name}, License: "MPL-2.0",
		})
	}

	data, err := index.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}

	return &fstest.MapFile{Data: data}
}

// summaries are what the versions of the index say of each name.
var summaries = map[string]struct{ description, category string }{
	nats:  {"A NATS cluster", "messaging"},
	redis: {"A Redis cache", "database"},
	"web": {"A web shop", "application"},
}

// globalIndex is an index with two modules, a stack and a blocked version.
func globalIndex(t *testing.T) fstest.MapFS {
	t.Helper()

	advisories, err := index.Marshal(index.Advisories{Advisories: []index.Advisory{{
		Kind: index.KindModule, Namespace: norca, Name: nats, Version: "1.2.0",
		Level: index.LevelBlocked, Reason: "broken", Decision: "https://example.com/1",
	}}})
	if err != nil {
		t.Fatal(err)
	}

	return fstest.MapFS{
		"modules/n/norca/nats.json": entry(t, norca, nats, "1.0.0", "1.2.0", "1.9.0", "1.10.0"),
		"modules/a/acme/redis.json": entry(t, acme, redis, "0.1.0"),
		"stacks/n/norca/web.json":   entry(t, norca, "web", "2.0.0"),
		"namespaces/norca.json":     {Data: []byte("{}")},
		index.AdvisoriesFile:        {Data: advisories},
		"README.md":                 {Data: []byte("# the registry\n")},
		"modules/n/norca/notes.txt": {Data: []byte("not an entry\n")},
	}
}

// The index of the global registry: every entry, modules first, by
// namespace and name, the versions the newest first, a blocked one left
// out.
func TestGlobal(t *testing.T) {
	t.Parallel()

	items, err := catalog.Global(globalIndex(t))
	if err != nil {
		t.Fatal(err)
	}

	got := describe(items)
	want := "module acme/redis 0.1.0; module norca/nats 1.10.0 1.9.0 1.0.0; stack norca/web 2.0.0"

	if got != want {
		t.Errorf("Global() = %s, want %s", got, want)
	}
}

func TestGlobal_Errors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		file string
		data string
		err  string
	}{
		{name: "invalid entry", file: "modules/n/norca/nats.json", data: `{"namespace":"norca","name":"nats","versions":[{"version":"x"}]}`, err: "modules/n/norca/nats.json"},
		{name: "unknown field", file: "stacks/n/norca/web.json", data: `{"namespace":"norca","name":"web","versions":[],"extra":1}`, err: "stacks/n/norca/web.json"},
		{name: "wrong letter", file: "modules/x/norca/nats.json", data: `{}`, err: "not where an entry of norca/nats belongs"},
		{name: "invalid advisories", file: index.AdvisoriesFile, data: `{"advisories":[{"kind":"module"}]}`, err: index.AdvisoriesFile},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fsys := fstest.MapFS{tt.file: {Data: []byte(tt.data)}}

			if _, err := catalog.Global(fsys); err == nil || !strings.Contains(err.Error(), tt.err) {
				t.Errorf("Global() = %v, want an error naming %q", err, tt.err)
			}
		})
	}
}

// An item of the index says what its newest version says, not one that an
// advisory blocks.
func TestGlobal_Summary(t *testing.T) {
	t.Parallel()

	items, err := catalog.Global(globalIndex(t))
	if err != nil {
		t.Fatal(err)
	}

	var got []string
	for _, item := range items {
		got = append(got, item.Name+": "+item.Description+", "+item.Category+", "+strings.Join(item.Keywords, " ")+", "+item.License)
	}

	want := "redis: A Redis cache 0.1.0, database, redis, MPL-2.0; nats: A NATS cluster 1.10.0, messaging, nats, MPL-2.0; " +
		"web: A web shop 2.0.0, application, web, MPL-2.0"
	if strings.Join(got, "; ") != want {
		t.Errorf("Global() = %s, want %s", strings.Join(got, "; "), want)
	}
}

// A registry repository: the modules and stacks with a metadata.hcl, the
// versions of their tags; a tag of another name or not a version is no
// version, a module without a tag has none.
func TestRepository(t *testing.T) {
	t.Parallel()

	fsys := fstest.MapFS{
		"modules/nats/metadata.hcl": {Data: []byte("module {\n  name        = \"nats\"\n  description = \"A NATS cluster\"\n" +
			"  category    = \"messaging\"\n  keywords    = [\"queue\"]\n  license     = \"Apache-2.0\"\n}\n")},
		"modules/redis/metadata.hcl": {Data: []byte("module {}\n")},
		"modules/draft/README.md":    {Data: []byte("no metadata\n")},
		"stacks/web/metadata.hcl":    {Data: []byte("stack {}\n")},
	}
	tags := []string{"nats/v1.0.0", "nats/v1.1.0", "web/v0.1.0", "nats/vnext", "v9.9.9", "other/v1.0.0"}

	items, err := catalog.Repository(fsys, tags)
	if err != nil {
		t.Fatal(err)
	}

	if got, want := describe(items), "module nats 1.1.0 1.0.0; module redis; stack web 0.1.0"; got != want {
		t.Errorf("Repository() = %s, want %s", got, want)
	}

	// What metadata.hcl says at the commit; one that does not read says
	// nothing.
	nats := items[0]
	if got := nats.Description + ", " + nats.Category + ", " + strings.Join(nats.Keywords, " ") + ", " + nats.License; got != "A NATS cluster, messaging, queue, Apache-2.0" {
		t.Errorf("the nats of Repository() = %s", got)
	}

	if items[1].Description != "" || items[1].Category != "" {
		t.Errorf("redis = %+v", items[1])
	}
}

// The modules built into a program: each of the one version its
// metadata.hcl says, with what it says of itself; a directory without
// metadata.hcl is no module, one without a version has none.
func TestBuiltin(t *testing.T) {
	t.Parallel()

	fsys := fstest.MapFS{
		"modules/redis/metadata.hcl": {Data: []byte("module {\n  name        = \"redis\"\n  version     = \"0.4.2\"\n" +
			"  description = \"A Redis cache\"\n  category    = \"database\"\n}\n")},
		"modules/audit/metadata.hcl": {Data: []byte("module {\n  name    = \"audit\"\n  version = \"1.0.0\"\n}\n")},
		"modules/draft/metadata.hcl": {Data: []byte("module {\n  name = \"draft\"\n}\n")},
		"modules/notes/README.md":    {Data: []byte("no metadata\n")},
	}

	items, err := catalog.Builtin(fsys)
	if err != nil {
		t.Fatal(err)
	}

	if got, want := describe(items), "module audit 1.0.0; module draft; module redis 0.4.2"; got != want {
		t.Errorf("Builtin() = %s, want %s", got, want)
	}

	if redis := items[2]; redis.Description != "A Redis cache" || redis.Category != "database" {
		t.Errorf("the redis of Builtin() = %+v", redis)
	}
}

func TestSearch(t *testing.T) {
	t.Parallel()

	items, err := catalog.Global(globalIndex(t))
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name  string
		query catalog.Query
		want  string
		total int
	}{
		{name: "all", query: catalog.Query{}, want: "module acme/redis; module norca/nats; stack norca/web", total: 3},
		{name: "text in the namespace", query: catalog.Query{Text: "NORCA/"}, want: "module norca/nats; stack norca/web", total: 2},
		{name: "kind", query: catalog.Query{Kind: index.KindStack}, want: "stack norca/web", total: 1},
		{name: "page", query: catalog.Query{Offset: 1, Limit: 1}, want: "module norca/nats", total: 3},
		{name: "past the end", query: catalog.Query{Offset: 5}, want: "", total: 3},
		{name: "no match", query: catalog.Query{Text: "postgres"}, want: "", total: 0},
		{name: "text in the description", query: catalog.Query{Text: "CACHE"}, want: "module acme/redis", total: 1},
		{name: "category", query: catalog.Query{Category: "messaging"}, want: "module norca/nats", total: 1},
		{name: "keyword", query: catalog.Query{Keyword: "web"}, want: "stack norca/web", total: 1},
		{name: "category and text", query: catalog.Query{Category: "database", Text: "nats"}, want: "", total: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			page, total, err := catalog.Search(items, tt.query)
			if err != nil {
				t.Fatal(err)
			}

			refs := make([]string, 0, len(page))
			for _, item := range page {
				refs = append(refs, item.String())
			}

			if got := strings.Join(refs, "; "); got != tt.want || total != tt.total {
				t.Errorf("Search() = %q of %d, want %q of %d", got, total, tt.want, tt.total)
			}
		})
	}

	for _, q := range []catalog.Query{{Offset: -1}, {Limit: -1}, {Kind: "policy"}, {Category: "databases"}} {
		if _, _, err := catalog.Search(items, q); !errors.Is(err, catalog.ErrQuery) {
			t.Errorf("Search(%+v) = %v, want ErrQuery", q, err)
		}
	}
}

func TestSelect(t *testing.T) {
	t.Parallel()

	items, err := catalog.Global(globalIndex(t))
	if err != nil {
		t.Fatal(err)
	}

	gone := catalog.Ref{Kind: index.KindModule, Namespace: acme, Name: "gone"}
	selected := []catalog.Ref{
		{Kind: index.KindStack, Namespace: norca, Name: "web"},
		gone,
		{Kind: index.KindModule, Namespace: norca, Name: nats},
	}

	shown, missing := catalog.Select(items, selected)

	if got, want := describe(shown), "module norca/nats 1.10.0 1.9.0 1.0.0; stack norca/web 2.0.0"; got != want {
		t.Errorf("shown = %s, want %s", got, want)
	}

	if len(missing) != 1 || missing[0] != gone {
		t.Errorf("missing = %v, want %v", missing, gone)
	}
}

// describe writes items as "module ns/name v1 v2; ...".
func describe(items []catalog.Item) string {
	parts := make([]string, 0, len(items))
	for _, item := range items {
		parts = append(parts, strings.TrimSpace(item.String()+" "+strings.Join(item.Versions, " ")))
	}

	return strings.Join(parts, "; ")
}
