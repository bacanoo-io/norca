// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"encoding/json"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/bacanoo-io/norca/rigger/index"
)

// The module and the stack of the registry of searchedRegistry, and the
// versions of the greeter it lists.
const (
	acmeGreeter  = "acme/greeter"
	acmeGreeting = "acme/greeting"
	greeterV1    = "1.0.0"
	greeterV12   = "1.2.0"
	saysHello    = "Says hello"
	optVersion   = "-version"
	optKind      = "-kind"
	dash         = "-"
)

// kindStack is the kind of a stack as rigger show takes and writes it.
var kindStack = string(index.KindStack)

// shownJSON is what rigger show -json writes.
type shownJSON struct {
	Kind     string `json:"kind"`
	Source   string `json:"source"`
	Version  string `json:"version"`
	Metadata struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	} `json:"metadata"`
	Versions  []string `json:"versions"`
	Digest    string   `json:"digest"`
	Signers   []string `json:"signers"`
	Warnings  []string `json:"warnings"`
	Variables []struct {
		Name    string `json:"name"`
		Default string `json:"default"`
	} `json:"variables"`
	Outputs []struct {
		Name string `json:"name"`
	} `json:"outputs"`
	Modules []struct {
		Release    string `json:"release"`
		Source     string `json:"source"`
		Constraint string `json:"constraint"`
		Version    string `json:"version"`
	} `json:"modules"`
	Readme string `json:"readme"`
}

func showJSON(t *testing.T, args ...string) shownJSON {
	t.Helper()

	out := riggerRun(t, exitOK, nil, append(append([]string{cmdShow}, args...), "-json")...)

	var got shownJSON
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("rigger show -json: %v\n%s", err, out)
	}

	return got
}

// matchAll checks that out has a line each pattern matches.
func matchAll(t *testing.T, what, out string, rows ...*regexp.Regexp) {
	t.Helper()

	for _, row := range rows {
		if !row.MatchString(out) {
			t.Errorf("%s: no line %s:\n%s", what, row, out)
		}
	}
}

// A module of the global registry is shown without a cluster, fetched and
// verified as an install verifies it: the newest version, or the newest
// that meets -version, the versions of the index, the key IDs of the
// signatures, the advisories as warnings.
func TestRun_Show_Global(t *testing.T) {
	reg := searchedRegistry(t)
	t.Setenv("NORCA_ADDR", "http://127.0.0.1:1") // a cluster that is not there is never asked

	code, out, stderr := riggerRunAll(t, cmdShow, acmeGreeter)
	if code != exitOK || stderr != "" {
		t.Fatalf("rigger show acme/greeter = %d\nstdout:\n%s\nstderr:\n%s", code, out, stderr)
	}

	matchAll(t, "rigger show acme/greeter", out,
		regexp.MustCompile(`(?m)^module acme/greeter 1\.2\.0$`),
		tableRow("versions", greeterV12, greeterV1),
		regexp.MustCompile(`(?m)^  signed by +sha256:[0-9a-f]+$`),
		tableRow("tag", "greeter/v"+greeterV12),
		tableRow("description", dash),
		tableRow("NAME", "TYPE", "DEFAULT", "REQUIRED", "SENSITIVE", "DESCRIPTION"),
		tableRow("text", "any", `"hello"`, "no", "no", dash),
	)

	// The newest of 1.0: what it says of itself.
	got := showJSON(t, acmeGreeter, optVersion, "~> 1.0.0")
	if got.Kind != "module" || got.Version != greeterV1 || got.Metadata.Description != saysHello || len(got.Signers) != 1 ||
		!strings.HasPrefix(got.Signers[0], "sha256:") || !strings.HasPrefix(got.Digest, "sha256:") ||
		strings.Join(got.Versions, " ") != greeterV12+" "+greeterV1 || len(got.Variables) != 1 || got.Variables[0].Default != `"hello"` {
		t.Errorf("rigger show acme/greeter -version ~> 1.0.0 -json = %+v", got)
	}

	matchAll(t, "rigger show acme/greeter -version 1.0.0", riggerRun(t, exitOK, nil, cmdShow, acmeGreeter, optVersion, greeterV1),
		tableRow("description", saysHello), tableRow("category", "application"), tableRow("keywords", "greeting"),
		tableRow("license", "MPL-2.0"), tableRow("signed by", got.Signers[0]),
	)

	showAdvisory(t, reg)

	riggerRunErr(t, `no version meets the constraint ">= 2.0.0"`, cmdShow, acmeGreeter, optVersion, ">= 2.0.0")
	riggerRunErr(t, "acme/nothing", cmdShow, "acme/nothing")
	riggerRunErr(t, "show needs one module or stack", cmdShow)
	riggerRunErr(t, `-kind is module or stack, not "thing"`, cmdShow, acmeGreeter, optKind, "thing")
}

// showAdvisory checks that an advisory of the registry on the version is a
// warning on standard error and in the JSON.
func showAdvisory(t *testing.T, reg string) {
	t.Helper()

	writeFile(t, filepath.Join(reg, "advisories.json"), `{"advisories": [{"kind": "module", "namespace": "acme", "name": "greeter", "version": "`+
		greeterV1+`", "level": "warning", "reason": "greets too loudly"}]}`+"\n")

	if code, _, stderr := riggerRunAll(t, cmdShow, acmeGreeter, optVersion, greeterV1); code != exitOK || !strings.Contains(stderr, "greets too loudly") {
		t.Errorf("rigger show of a version with an advisory = %d:\n%s", code, stderr)
	}

	if got := showJSON(t, acmeGreeter, optVersion, greeterV1); !strings.Contains(strings.Join(got.Warnings, "; "), "greets too loudly") {
		t.Errorf("rigger show -json of a version with an advisory: warnings %q", got.Warnings)
	}
}

// A stack of the global registry is told from a module by the index; it is
// shown with its modules, their sources, constraints and the versions its
// lock file pins.
//
//nolint:paralleltest // the registry is the environment of the program (t.Setenv)
func TestRun_Show_Stack(t *testing.T) {
	searchedRegistry(t)

	out := riggerRun(t, exitOK, []string{"stack acme/greeting " + greeterV1, "RELEASE"}, cmdShow, acmeGreeting)
	if !tableRow("hello", acmeGreeter, "~> 1.0", greeterV1).MatchString(out) {
		t.Errorf("rigger show acme/greeting: no row of its module:\n%s", out)
	}

	got := showJSON(t, acmeGreeting, optKind, kindStack)
	if got.Kind != kindStack || got.Version != greeterV1 || len(got.Modules) != 1 || got.Modules[0].Source != acmeGreeter ||
		got.Modules[0].Version != greeterV1 || len(got.Signers) != 1 || strings.Join(got.Versions, " ") != greeterV1 {
		t.Errorf("rigger show acme/greeting -json = %+v", got)
	}

	riggerRunErr(t, acmeGreeting, cmdShow, acmeGreeting, optKind, "module")
}

// A built-in module has one version, the one the program ships, and no
// signature: the program is its source. -readme prints its README.
func TestRun_Show_Builtin(t *testing.T) {
	t.Setenv("RIGGER_CACHE_DIR", t.TempDir())

	v := builtinVersion(t)

	code, out, stderr := riggerRunAll(t, cmdShow, "builtin/redis")
	if code != exitOK || stderr != "" {
		t.Fatalf("rigger show builtin/redis = %d\nstdout:\n%s\nstderr:\n%s", code, out, stderr)
	}

	matchAll(t, "rigger show builtin/redis", out,
		regexp.MustCompile(`(?m)^module builtin/redis `+regexp.QuoteMeta(v)+`$`),
		tableRow("versions", v),
		tableRow("signed by", "not signed: built into the program"),
		tableRow("requires", "norca >= 0.1.0; features releases, secret-store"),
		tableRow("port", "no", "Port Redis listens on in the network namespace of the..."),
	)

	// The descriptions of the variables and outputs are cut as rigger search
	// cuts them; -max-width 0 keeps them whole.
	if whole := riggerRun(t, exitOK, nil, cmdShow, "builtin/redis", "-max-width", "0"); !tableRow("port", "no",
		"Port Redis listens on in the network namespace of the group (bridge mode).").MatchString(whole) {
		t.Errorf("rigger show builtin/redis -max-width 0:\n%s", whole)
	}

	if strings.Contains(out, "# redis") {
		t.Errorf("rigger show without -readme prints the README:\n%s", out)
	}

	riggerRun(t, exitOK, []string{"README.md:", "# redis"}, cmdShow, "builtin/redis", "-readme")

	got := showJSON(t, "builtin/redis", "-readme")
	if strings.Join(got.Versions, " ") != v || len(got.Signers) != 0 || len(got.Warnings) != 0 || !strings.HasPrefix(got.Readme, "# redis") || len(got.Outputs) != 3 {
		t.Errorf("rigger show builtin/redis -json = %+v", got)
	}

	riggerRunErr(t, "builtin/redis is version "+v+" in this program", cmdShow, "builtin/redis", optVersion, ">= 99.0.0")
	riggerRunErr(t, "no built-in module nothing", cmdShow, "builtin/nothing")
	riggerRunErr(t, "a published stack comes from the global registry", cmdShow, "builtin/redis", optKind, kindStack)
}

// A module of a git repository by its address: the versions of its tags,
// not signed.
func TestRun_Show_Git(t *testing.T) {
	t.Setenv("RIGGER_CACHE_DIR", t.TempDir())

	repo := registryRepo(t)

	code, out, stderr := riggerRunAll(t, cmdShow, repo+"//modules/greeter")
	if code != exitOK || !strings.Contains(stderr, "is not signed") || !tableRow("versions", "1.1.0", greeterV1).MatchString(out) ||
		!tableRow("path", "no", dash).MatchString(out) {
		t.Errorf("rigger show of a git address = %d\nstdout:\n%s\nstderr:\n%s", code, out, stderr)
	}
}
