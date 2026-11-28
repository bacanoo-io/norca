// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/bacanoo-io/norca/norca/api"

	rigger "github.com/bacanoo-io/norca/rigger"
)

// The flags and modules of the tests of install.
const (
	flagName       = "-name"
	flagServer     = "-server"
	flagVersion    = "-version"
	flagRegistry   = "-registry"
	globalModule   = "acme/web"
	builtinRedis   = "builtin/redis"
	gitModule      = "github.com/acme/modules//web"
	ociModule      = "oci://registry.example.com/acme/web"
	riggerRelease  = "web"
	someConstraint = "~> 1.2"
)

// parseInstallFlags parses the arguments of rigger install as the program
// does: the flags, one dash or two, then the argument.
func parseInstallFlags(_ io.Writer, args []string) (*installFlags, error) {
	cmd := &cobra.Command{Use: cmdInstall}
	f := addInstallFlags(cmd)

	if err := cmd.ParseFlags(normalizeArgs(cmd, args)); err != nil {
		return nil, err
	}

	return f, f.setArg(cmd.Flags().Args())
}

// The first argument of install is a directory when there is one, else the
// address of a module, of which -version picks the version; with
// -registry it is the name of a module of that registry of the cluster.
func TestParseInstallFlags(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	tests := []struct {
		name                   string
		args                   []string
		dir, address, registry string
		version                string
		err                    string
	}{
		{name: "no argument", args: []string{flagName, "x"}, dir: "."},
		{name: "a directory", args: []string{dir, flagName, "x"}, dir: dir},
		{name: "a missing path", args: []string{"./missing", flagName, "x"}, dir: "./missing"},
		{name: "the global registry", args: []string{globalModule, flagName, "x"}, address: globalModule},
		{name: "a constraint", args: []string{globalModule, flagName, "x", flagVersion, someConstraint}, address: globalModule, version: someConstraint},
		{name: "builtin", args: []string{builtinRedis, flagName, "x", flagVersion, ">= 0.4"}, address: builtinRedis, version: ">= 0.4"},
		{name: "a git address", args: []string{gitModule, flagName, "x"}, address: gitModule},
		{name: "an oci address", args: []string{ociModule, flagName, "x"}, address: ociModule},
		{name: "a registry of the cluster", args: []string{riggerRelease, flagName, "x", flagRegistry, fakeAcme, flagServer}, address: riggerRelease, registry: fakeAcme},
		{name: "a version of a directory", args: []string{dir, flagName, "x", flagVersion, "1.4.0"}, err: errInstallVersionDir.Error()},
		{name: "a registry without -server", args: []string{riggerRelease, flagName, "x", flagRegistry, fakeAcme, "-server=false"}, err: errInstallRegistryServer.Error()},
		{name: "a registry without a module", args: []string{flagName, "x", flagRegistry, fakeAcme, flagServer}, err: errInstallRegistryModule.Error()},
		{name: "no name", args: []string{globalModule}, err: errInstallName.Error()},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			f, err := parseInstallFlags(io.Discard, tt.args)
			if tt.err != "" {
				if err == nil || !strings.Contains(err.Error(), tt.err) {
					t.Fatalf("parseInstallFlags() = %v, want %q", err, tt.err)
				}

				return
			}

			if err != nil {
				t.Fatal(err)
			}

			if f.dir != tt.dir || f.address != tt.address || f.registry != tt.registry || f.version != tt.version {
				t.Errorf("dir %q, address %q, registry %q, version %q; want %q, %q, %q, %q",
					f.dir, f.address, f.registry, f.version, tt.dir, tt.address, tt.registry, tt.version)
			}
		})
	}
}

// The servers install a module of a registry they synced or one built into
// them: a module of the global registry names the registry global, one of
// another registry of the cluster names it (-registry), a built-in module
// none. Git and OCI addresses and directories are not sent.
func TestModuleInstallRequest(t *testing.T) {
	t.Parallel()

	base := installFlags{name: riggerRelease, version: someConstraint, adopt: true, timeout: time.Minute}
	want := api.RiggerInstallRequest{Release: riggerRelease, Version: someConstraint, Adopt: true, Timeout: time.Minute}

	of := func(registry, module string) api.RiggerInstallRequest {
		r := want
		r.Registry, r.Module = registry, module

		return r
	}

	tests := []struct {
		name, address, registry string
		want                    api.RiggerInstallRequest
		err                     string
	}{
		{name: "the global registry", address: globalModule, want: of(api.RiggerGlobalRegistry, globalModule)},
		{name: "a registry of the cluster", address: riggerRelease, registry: fakeAcme, want: of(fakeAcme, riggerRelease)},
		{name: "builtin", address: builtinRedis, want: of("", builtinRedis)},
		{name: "a git address", address: gitModule, err: "rigger registry sync"},
		{name: "an oci address", address: ociModule, err: "rigger registry sync"},
		{name: "a directory", address: "dir:///srv/web", err: rigger.ErrDirectorySource.Error()},
		{name: "no address", address: "not/an/address", err: "invalid source"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			f := base
			f.address, f.registry = tt.address, tt.registry

			got, err := moduleInstallRequest(&f)
			if tt.err != "" {
				if err == nil || !strings.Contains(err.Error(), tt.err) {
					t.Fatalf("moduleInstallRequest() = %v, want %q", err, tt.err)
				}

				return
			}

			if err != nil {
				t.Fatal(err)
			}

			if !equalJSON(t, *got, tt.want) {
				t.Errorf("moduleInstallRequest() = %+v, want %+v", *got, tt.want)
			}
		})
	}
}

func equalJSON(t *testing.T, a, b any) bool {
	t.Helper()

	ja, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}

	jb, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}

	return bytes.Equal(ja, jb)
}

// notSynced is the answer of servers that have not synced the global
// registry.
const notSynced = `the registry "global" has not been synced: sync it first (rigger registry sync global)`

// installServer is a cluster that answers installs: the module
// acme/unsynced as servers that have not synced the global registry,
// another one with a revision and an advisory. It records the requests.
type installServer struct {
	mu       sync.Mutex
	requests []string
	last     api.RiggerInstallRequest
}

func (s *installServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.requests = append(s.requests, r.Method+" "+r.URL.Path)

	if r.Method != http.MethodPut || r.URL.Path != "/v1/rigger/install" {
		http.NotFound(w, r)

		return
	}

	if err := json.NewDecoder(r.Body).Decode(&s.last); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)

		return
	}

	if s.last.Module == "acme/unsynced" {
		http.Error(w, notSynced, http.StatusConflict)

		return
	}

	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"Action":"install","Revision":3,"Warnings":["acme/web 1.2.0: an advisory"],` +
		`"NextSteps":"The service web runs.\n  norca service info web\n"}`))
}

// runInstallCommand runs rigger install with args: its exit code, its
// standard output and its standard error.
func runInstallCommand(t *testing.T, args ...string) (int, string, string) {
	t.Helper()

	var stdout, stderr bytes.Buffer

	code := Run(t.Context(), append([]string{cmdInstall}, args...), &stdout, &stderr)

	return code, stdout.String(), stderr.String()
}

// install -server of a module by its address sends the request of the
// address with the values of the flags, and writes the warnings, the
// revision and the next steps of the answer; the answer of servers that have not synced the
// registry reaches the user as they wrote it, and nothing syncs it. A git
// address is refused before anything is sent.
func TestServerInstall_ByAddress(t *testing.T) {
	fake := &installServer{}

	srv := httptest.NewServer(fake)
	defer srv.Close()

	t.Setenv("NORCA_ADDR", srv.URL)
	t.Setenv("NORCA_NAMESPACE", "")
	t.Setenv(serverEnv, "")

	wantStdout := "install store/web: revision 3\n\nThe service web runs.\n  norca service info web\n"

	code, stdout, stderr := runInstallCommand(t, globalModule, flagName, riggerRelease, "-namespace", "store", flagServer, flagVersion, someConstraint, "-var", "count=2")
	if code != exitOK || stdout != wantStdout || !strings.Contains(stderr, "an advisory") {
		t.Errorf("install = %d, stdout %q, stderr %q", code, stdout, stderr)
	}

	got := fake.last
	want := api.RiggerInstallRequest{Release: riggerRelease, Registry: api.RiggerGlobalRegistry, Module: globalModule, Version: someConstraint, Values: "count = 2\n"}

	if !equalJSON(t, got, want) {
		t.Errorf("the request %+v, want %+v", got, want)
	}

	code, _, stderr = runInstallCommand(t, "acme/unsynced", flagName, riggerRelease, flagServer)
	if code != exitError || !strings.Contains(stderr, notSynced) {
		t.Errorf("install of a registry not synced = %d, stderr %q", code, stderr)
	}

	code, _, stderr = runInstallCommand(t, gitModule, flagName, riggerRelease, flagServer)
	if code != exitError || !strings.Contains(stderr, "rigger registry sync") {
		t.Errorf("install of a git address = %d, stderr %q", code, stderr)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()

	if want := "PUT /v1/rigger/install, PUT /v1/rigger/install"; strings.Join(fake.requests, ", ") != want {
		t.Errorf("requests %v, want %s", fake.requests, want)
	}
}

// Without -server an address is fetched here: a built-in module whose
// version does not meet -version is refused before the cluster is asked
// anything.
func TestInstall_BuiltinVersion(t *testing.T) {
	t.Setenv("RIGGER_CACHE_DIR", t.TempDir())
	t.Setenv("NORCA_ADDR", "http://127.0.0.1:1")
	t.Setenv(serverEnv, "")

	code, _, stderr := runInstallCommand(t, builtinRedis, flagName, "kv", flagVersion, ">= 99.0.0")
	if code != exitError || !strings.Contains(stderr, "builtin/redis is version") || !strings.Contains(stderr, `>= 99.0.0`) {
		t.Errorf("install = %d, stderr %q", code, stderr)
	}
}

// The next steps of a module follow the line of the install below an empty
// line, with one line end; a module without them adds nothing.
func TestWriteNextSteps(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name, text, expected string
	}{
		{name: "none", text: "", expected: ""},
		{name: "only white space", text: "\n  \n", expected: ""},
		{name: "a heredoc", text: "Run:\n  norca service info web\n", expected: "\nRun:\n  norca service info web\n"},
		{name: "no line end", text: "Done.", expected: "\nDone.\n"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var b bytes.Buffer
			if err := writeNextSteps(&b, tt.text); err != nil {
				t.Fatal(err)
			}

			if b.String() != tt.expected {
				t.Errorf("writeNextSteps(%q) wrote %q, want %q", tt.text, b.String(), tt.expected)
			}
		})
	}
}
