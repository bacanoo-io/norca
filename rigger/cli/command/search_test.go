// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"encoding/json"
	"io/fs"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/bacanoo-io/norca/rigger/builtin"
)

// searchedRegistry is a registry of the shape of the global one, a
// directory, that lists the module acme/greeter 1.0.0 (with a description,
// a category and a keyword) and 1.2.0 (without), both signed, and the stack
// acme/greeting 1.0.0 over greeter 1.0.0, committed; the program reads it
// (RIGGER_GLOBAL_REGISTRY). It returns the directory of the registry.
func searchedRegistry(t *testing.T) string {
	t.Helper()

	repo, reg, _, keys := publishedRegistry(t)
	t.Setenv("RIGGER_GLOBAL_REGISTRY", reg)
	publishedStack(t, repo, reg, keys)

	setVersion(t, repo, greeterV12)
	riggerRun(t, exitOK, []string{"as greeter/v1.2.0"}, "module", "publish", filepath.Join(repo, "modules", "greeter"), "-key", filepath.Join(keys, "acme.key"))
	riggerRun(t, exitOK, []string{"listed module acme/greeter 1.2.0"}, "index", "update", "-dir", reg, "-today", "2026-11-25")
	gitIn(t, reg, "add", "-A")
	gitIn(t, reg, "commit", "--quiet", "-m", "acme/greeter 1.2.0")

	return reg
}

// builtinVersion is the version of the built-in module redis, as its
// metadata says.
func builtinVersion(t *testing.T) string {
	t.Helper()

	data, err := fs.ReadFile(builtin.FS(), "modules/redis/metadata.hcl")
	if err != nil {
		t.Fatal(err)
	}

	m := regexp.MustCompile(`version\s*=\s*"([^"]+)"`).FindSubmatch(data)
	if m == nil {
		t.Fatalf("the metadata of redis names no version:\n%s", data)
	}

	return string(m[1])
}

// searchOutput is what rigger search -json writes.
type searchOutput struct {
	Registry string `json:"registry"`
	Commit   string `json:"commit"`
	Total    int    `json:"total"`
	Offset   int    `json:"offset"`
	Items    []struct {
		Kind      string   `json:"kind"`
		Namespace string   `json:"namespace"`
		Name      string   `json:"name"`
		Source    string   `json:"source"`
		Versions  []string `json:"versions"`
	} `json:"items"`
}

// searchJSON runs rigger search -json with args and decodes what it writes.
func searchJSON(t *testing.T, args ...string) searchOutput {
	t.Helper()

	out := riggerRun(t, exitOK, nil, append(append([]string{cmdSearch}, args...), "-json")...)

	var got searchOutput
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("rigger search -json: %v\n%s", err, out)
	}

	return got
}

// tableRow matches a row of a table rigger search or rigger show writes:
// the words, with the spaces of the table between them, after the
// indentation of the row.
func tableRow(words ...string) *regexp.Regexp {
	quoted := make([]string, 0, len(words))
	for _, w := range words {
		quoted = append(quoted, regexp.QuoteMeta(w))
	}

	return regexp.MustCompile(`(?m)^ *` + strings.Join(quoted, ` +`) + `\s*$`)
}

// A search of the global registry needs no cluster: the registry of the
// program (RIGGER_GLOBAL_REGISTRY, here a directory read as it is) is
// searched as the servers search a registry they synced.
//
//nolint:paralleltest // the registry is the environment of the program (t.Setenv)
func TestRun_Search_Global(t *testing.T) {
	searchedRegistry(t)
	t.Setenv("NORCA_ADDR", "http://127.0.0.1:1") // a cluster that is not there is never asked

	out := riggerRun(t, exitOK, []string{"KIND", "NAME", "LATEST", "DESCRIPTION"}, cmdSearch)
	matchAll(t, "rigger search", out, tableRow("module", acmeGreeter, greeterV12, dash), tableRow(kindStack, acmeGreeting, greeterV1, dash))

	if strings.Contains(out, " of ") {
		t.Errorf("rigger search of one page has a footer:\n%s", out)
	}

	tests := []struct {
		name string
		args []string
		// expected are the sources found, in order; total how many match.
		expected string
		total    int
	}{
		{name: "all", expected: acmeGreeter + " " + acmeGreeting, total: 2},
		{name: "text", args: []string{"GREETING"}, expected: acmeGreeting, total: 1},
		{name: "text of words", args: []string{"acme/", "greet"}, expected: "", total: 0},
		{name: "kind", args: []string{optKind, "module"}, expected: acmeGreeter, total: 1},
		// What the newest version says: 1.2.0 says no category and no keyword.
		{name: "category", args: []string{"-category", "application"}, expected: "", total: 0},
		{name: "keyword", args: []string{"-keyword", "greeting"}, expected: "", total: 0},
		{name: "page", args: []string{"-offset", "1", "-limit", "1"}, expected: acmeGreeting, total: 2},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := searchJSON(t, tt.args...)

			sources := make([]string, 0, len(got.Items))
			for _, item := range got.Items {
				sources = append(sources, item.Source)
			}

			if strings.Join(sources, " ") != tt.expected || got.Total != tt.total {
				t.Errorf("rigger search %s = %v of %d, want %s of %d", strings.Join(tt.args, " "), sources, got.Total, tt.expected, tt.total)
			}
		})
	}

	// The versions, the newest first; a directory is read as it is, at no
	// commit.
	got := searchJSON(t, optKind, "module")
	if v := got.Items[0].Versions; strings.Join(v, " ") != greeterV12+" "+greeterV1 || got.Commit != "" || got.Items[0].Namespace != "acme" {
		t.Errorf("rigger search -json = %+v", got)
	}

	// A page of more says which.
	out = riggerRun(t, exitOK, []string{"2-2 of 2"}, cmdSearch, "-offset", "1", "-limit", "1")
	if strings.Contains(out, acmeGreeter) {
		t.Errorf("the second page shows the first:\n%s", out)
	}

	riggerRun(t, exitOK, []string{"nothing matches"}, cmdSearch, "no-such-thing")
	riggerRunErr(t, "invalid query", cmdSearch, optKind, "thing")
	riggerRunErr(t, `category "nope" is none of`, cmdSearch, "-category", "nope")
}

// A global registry at a git address is read at the commit of its default
// branch, fetched into the cache.
func TestRun_Search_GlobalRepository(t *testing.T) {
	reg := searchedRegistry(t)
	t.Setenv("RIGGER_GLOBAL_REGISTRY", "file://"+filepath.ToSlash(reg))

	got := searchJSON(t)
	if got.Total != 2 || len(got.Commit) != 40 || got.Items[0].Source != acmeGreeter {
		t.Errorf("rigger search -json of a repository = %+v", got)
	}

	// The commit of the search is that of the default branch.
	code, _, stderr := riggerRunAll(t, "-log-level", "info", cmdSearch)
	if code != exitOK || !strings.Contains(stderr, got.Commit) {
		t.Errorf("rigger search = %d, its log does not name the commit %s:\n%s", code, got.Commit, stderr)
	}
}

// The modules built into the program, and those of a registry repository
// by their tags; the source of each is the address rigger show and rigger
// install take.
func TestRun_Search_BuiltinAndRepository(t *testing.T) {
	t.Setenv("RIGGER_CACHE_DIR", t.TempDir())

	v := builtinVersion(t)
	out := riggerRun(t, exitOK, nil, cmdSearch, "-registry", "builtin")

	// The description is cut to -max-width (60) at a word, so a row stays
	// on one line of a terminal; -max-width 0 keeps it whole.
	if !tableRow("module", "builtin/redis", v, "One Redis server: a service in the catalog, a password...").MatchString(out) {
		t.Errorf("rigger search -registry builtin:\n%s", out)
	}

	whole := riggerRun(t, exitOK, nil, cmdSearch, "-registry", "builtin", "-max-width", "0")
	if !tableRow("module", "builtin/redis", v, "One Redis server: a service in the catalog, a password from a Norca Variable that the server requires, "+
		"optional persistence on a host volume.").MatchString(whole) {
		t.Errorf("rigger search -registry builtin -max-width 0:\n%s", whole)
	}

	riggerRunErr(t, "-max-width", cmdSearch, "-registry", "builtin", "-max-width", "-1")

	if got := searchJSON(t, "-registry", "builtin", "-category", "database"); got.Total != 1 || got.Items[0].Source != "builtin/redis" {
		t.Errorf("rigger search -registry builtin -category database = %+v", got)
	}

	repo := registryRepo(t)

	got := searchJSON(t, "-registry", repo)
	if got.Total != 1 || got.Items[0].Name != "greeter" || got.Items[0].Source != repo+"//modules/greeter" ||
		strings.Join(got.Items[0].Versions, " ") != "1.1.0 "+greeterV1 || len(got.Commit) != 40 {
		t.Errorf("rigger search -registry %s = %+v", repo, got)
	}

	if !tableRow("module", "greeter", "1.1.0", dash).MatchString(riggerRun(t, exitOK, nil, cmdSearch, "-registry", repo)) {
		t.Error("rigger search of a repository has no row of the greeter")
	}

	riggerRunErr(t, "git repository", cmdSearch, "-registry", filepath.Join(t.TempDir(), "none"))
}
