// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/api"
)

// The registries of the fake and the words of its objects.
const (
	fakeGlobal = "global"
	fakeAcme   = "acme"
	fakeKind   = "kind"
	fakeModule = "module"
	fakeName   = "name"
)

// fakeRegistries is the part of the HTTP API of a cluster the registry
// commands use: two registries, a sync, a search, and object writes with a
// check-and-set.
type fakeRegistries struct {
	mu      sync.Mutex
	objects map[string]*api.CustomObject
	applied []string // the cas of each write
}

func newFakeRegistries() *fakeRegistries {
	return &fakeRegistries{objects: map[string]*api.CustomObject{
		fakeGlobal: {
			Group: api.RiggerObjectGroup, Kind: api.RiggerRegistryKind, Name: fakeGlobal, ModifyIndex: 7,
			Spec: map[string]any{
				"type": "global", "source": "github.com/bacanoo-io/norca-global-registry", "display": "selected",
				"selected": []any{map[string]any{fakeKind: fakeModule, "namespace": "norca", fakeName: "nats"}},
			},
			Status: map[string]any{
				"phase": "Ready", "commit": "0123456789abcdef0123456789abcdef01234567", "source": "github.com/bacanoo-io/norca-global-registry",
				"modules": 2, "stacks": 1, "lastSyncedAt": "2026-11-22T10:00:00Z", "lastGoodSyncAt": "2026-11-22T10:00:00Z",
				"shown":   []any{map[string]any{fakeKind: fakeModule, "namespace": "norca", fakeName: "nats", "versions": []any{"1.1.0", "1.0.0"}}},
				"missing": []any{map[string]any{fakeKind: fakeModule, "namespace": "acme", fakeName: "gone"}},
			},
		},
		fakeAcme: {
			Group: api.RiggerObjectGroup, Kind: api.RiggerRegistryKind, Name: fakeAcme, ModifyIndex: 9,
			Spec: map[string]any{"type": "git", "source": "https://git.example.com/acme/modules"},
		},
	}}
}

func (f *fakeRegistries) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	const objects, object = "/v1/objects/rigger.norca.io/Registry", "/v1/object/rigger.norca.io/Registry/"

	switch {
	case r.URL.Path == objects:
		writeJSON(w, []*api.CustomObject{f.objects[fakeAcme], f.objects[fakeGlobal]})
	case strings.HasPrefix(r.URL.Path, object):
		f.object(w, r, strings.TrimPrefix(r.URL.Path, object))
	case r.URL.Path == "/v1/rigger/registry/acme/sync":
		writeJSON(w, map[string]any{fakeName: fakeAcme, "spec": map[string]any{"type": "git"}, "status": map[string]any{
			"phase": "Ready", "commit": "fedcba9876543210fedcba9876543210fedcba98", "modules": 3, "stacks": 1,
		}})
	case r.URL.Path == "/v1/rigger/registry/global/sync":
		http.Error(w, "sync of the registry \"global\": git fetch: no network", http.StatusInternalServerError)
	case r.URL.Path == "/v1/rigger/registry/acme/search":
		q := r.URL.Query()
		if q.Get(fakeKind) != fakeModule || q.Get("text") != "web app" || q.Get("offset") != "1" || q.Get("limit") != "2" {
			http.Error(w, "unexpected query "+r.URL.RawQuery, http.StatusBadRequest)

			return
		}

		writeJSON(w, map[string]any{
			"Items": []any{
				map[string]any{fakeKind: fakeModule, fakeName: "web-app", "versions": []any{"2.0.0", "1.0.0"}},
				map[string]any{fakeKind: fakeModule, fakeName: "web-app-draft", "versions": []any{}},
			},
			"Total": 5, "Commit": "fedcba9876543210fedcba9876543210fedcba98",
		})
	default:
		http.Error(w, "unknown path "+r.URL.Path, http.StatusNotFound)
	}
}

// object reads or writes one registry.
func (f *fakeRegistries) object(w http.ResponseWriter, r *http.Request, name string) {
	if r.Method == http.MethodGet {
		obj, ok := f.objects[name]
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)

			return
		}

		writeJSON(w, obj)

		return
	}

	var obj api.CustomObject
	if err := json.NewDecoder(r.Body).Decode(&obj); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)

		return
	}

	f.applied = append(f.applied, r.URL.Query().Get("cas"))
	f.objects[obj.Name].Spec = obj.Spec
	writeJSON(w, obj)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// The registry commands against the HTTP API: a table of the registries,
// the status of one, a sync and its failure, a page of a search, a
// selection written with a check-and-set on the registry as it was read.
func TestRun_Registry(t *testing.T) {
	fake := newFakeRegistries()
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	t.Setenv("NORCA_ADDR", srv.URL)
	t.Setenv("NORCA_TOKEN", "")

	tests := []struct {
		name   string
		args   []string
		code   int
		stdout []string
		stderr string
	}{
		{
			name: "list", args: []string{"list"},
			stdout: []string{"NAME    TYPE    DISPLAY   PHASE  COMMIT", "acme    git     -", "global  selected  Ready  0123456789ab  2        1       github.com/bacanoo-io/norca-global-registry"},
		},
		{
			name: "status", args: []string{cmdStatus, fakeGlobal},
			stdout: []string{
				"registry global (global) github.com/bacanoo-io/norca-global-registry", "display: selected, 1 selected",
				"commit 0123456789abcdef0123456789abcdef01234567 of github.com/bacanoo-io/norca-global-registry: 2 modules, 1 stacks",
				"shown module norca/nats 1.1.0 1.0.0", "missing module acme/gone: selected, not in the registry",
			},
		},
		{name: "status not synced", args: []string{cmdStatus, fakeAcme}, stdout: []string{"not synced: rigger registry sync acme"}},
		{name: "sync", args: []string{cmdRegistrySync, fakeAcme}, stdout: []string{"synced acme: commit fedcba987654, 3 modules, 1 stacks"}},
		{name: "failed sync", args: []string{cmdRegistrySync, fakeGlobal}, code: exitError, stderr: "git fetch: no network"},
		{
			name: "search", args: []string{cmdRegistrySearch, fakeAcme, "web", "app", "-kind", fakeModule, "-offset", "1", "-limit", "2"},
			stdout: []string{"module  web-app        2.0.0   2", "module  web-app-draft  -       0", "2-3 of 5, at commit fedcba987654"},
		},
		{name: "no name", args: []string{cmdRegistrySync}, code: exitError, stderr: "the command needs the name of a registry"},
		{name: "bad ref", args: []string{cmdRegistrySelect, fakeGlobal, "policy", "x"}, code: exitError, stderr: "select and unselect need module|stack"},
		{name: "no command", args: nil, code: exitError, stderr: "registry needs a command"},
	}

	//nolint:paralleltest // the subtests share the environment of the cluster address (t.Setenv)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer

			code := Run(t.Context(), append([]string{"-log-level", "error", "registry"}, tt.args...), &stdout, &stderr)
			if code != tt.code {
				t.Fatalf("Run() = %d, want %d (stderr %q)", code, tt.code, stderr.String())
			}

			for _, want := range tt.stdout {
				if !strings.Contains(stdout.String(), want) {
					t.Errorf("stdout does not contain %q:\n%s", want, stdout.String())
				}
			}

			if !strings.Contains(stderr.String(), tt.stderr) {
				t.Errorf("stderr = %q, want %q", stderr.String(), tt.stderr)
			}
		})
	}

	// Select a stack, then unselect the module: each write has the
	// check-and-set of the registry as it was read.
	var stdout, stderr bytes.Buffer

	if code := Run(t.Context(), []string{cmdRegistry, cmdRegistrySelect, fakeGlobal, "stack", "norca/site"}, &stdout, &stderr); code != exitOK {
		t.Fatalf("select = %d: %s", code, stderr.String())
	}

	if !strings.Contains(stdout.String(), "selected stack norca/site in global; the UI changes with the next sync (rigger registry sync global)") {
		t.Errorf("select wrote %q", stdout.String())
	}

	if code := Run(t.Context(), []string{cmdRegistry, cmdRegistryUnselect, fakeGlobal, fakeModule, "norca/nats"}, &stdout, &stderr); code != exitOK {
		t.Fatalf("unselect = %d: %s", code, stderr.String())
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()

	if strings.Join(fake.applied, ",") != "7,7" {
		t.Errorf("the writes had the check-and-set %v, want 7 and 7", fake.applied)
	}

	selected, _ := json.Marshal(fake.objects[fakeGlobal].Spec["selected"])
	if string(selected) != `[{"kind":"stack","name":"site","namespace":"norca"}]` {
		t.Errorf("selected = %s, want the stack norca/site only", selected)
	}
}

// requestWait is how long a test waits for a request of its command to
// reach its server. The requests before it may each take long on a machine
// that scans local connections (20 seconds were seen on Windows).
const requestWait = 2 * time.Minute

// hangingServer is a server of the HTTP API that never answers the requests
// hangs picks: they wait until their client gives up, or the test ends. The
// other requests go to next. A request that hangs is told on the channel.
func hangingServer(t *testing.T, next http.Handler, hangs func(*http.Request) bool) (*httptest.Server, <-chan struct{}) {
	t.Helper()

	release := make(chan struct{})
	hung := make(chan struct{}, 1)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !hangs(r) {
			next.ServeHTTP(w, r)

			return
		}

		select {
		case hung <- struct{}{}:
		default:
		}

		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))

	// Close waits for the requests: those of a client that never gave up
	// are let go first.
	t.Cleanup(func() {
		close(release)
		srv.Close()
	})

	return srv, hung
}

// cancelWhenHung cancels the context of a command when its request hangs on
// the server, and returns how the command ended; the test fails when it
// goes on waiting for the server.
func cancelWhenHung(t *testing.T, hung <-chan struct{}, cancel context.CancelFunc, done <-chan int) int {
	t.Helper()

	select {
	case <-hung:
	case code := <-done:
		t.Fatalf("the command ended with %d before a request hung", code)
	case <-time.After(requestWait):
		t.Fatal("no request reached the server")
	}

	cancel()

	code, ended := endsWithin(done, 10*time.Second)
	if !ended {
		t.Fatal("the command still waits for the server after its context was cancelled")
	}

	return code
}

// endsWithin waits for the end of a command, for d at most. A machine that
// stood still for longer than d has both the end and the time when it goes
// on: the end is waited for a little longer, so that it wins.
func endsWithin[T any](done <-chan T, d time.Duration) (T, bool) {
	select {
	case v := <-done:
		return v, true
	case <-time.After(d):
	}

	select {
	case v := <-done:
		return v, true
	case <-time.After(5 * time.Second):
		var none T

		return none, false
	}
}

// A registry command ends when its context is done (a Ctrl-C), whichever
// request it waits for: the servers may be slow, or never answer.
func TestRun_Registry_Cancelled(t *testing.T) {
	always := func(*http.Request) bool { return true }

	tests := []struct {
		name  string
		args  []string
		hangs func(*http.Request) bool
	}{
		{name: "the registries", args: []string{"list"}, hangs: always},
		{name: "the status of a registry", args: []string{cmdStatus, fakeGlobal}, hangs: always},
		{name: "the sync of a registry", args: []string{cmdRegistrySync, fakeAcme}, hangs: always},
		{name: "a search", args: []string{cmdRegistrySearch, fakeAcme, "web"}, hangs: always},
		{name: "select, reading the registry", args: []string{cmdRegistrySelect, fakeGlobal, "stack", "norca/site"}, hangs: always},
		{
			name: "select, writing the selection", args: []string{cmdRegistrySelect, fakeGlobal, "stack", "norca/site"},
			hangs: func(r *http.Request) bool { return r.Method != http.MethodGet },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, hung := hangingServer(t, newFakeRegistries(), tt.hangs)
			t.Setenv("NORCA_ADDR", srv.URL)
			t.Setenv("NORCA_TOKEN", "")

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()

			var stdout, stderr bytes.Buffer

			done := make(chan int, 1)

			go func() {
				done <- Run(ctx, append([]string{flagLogLevel, "error", cmdRegistry}, tt.args...), &stdout, &stderr)
			}()

			if code := cancelWhenHung(t, hung, cancel, done); code != exitError || !strings.Contains(stderr.String(), context.Canceled.Error()) {
				t.Errorf("Run() = %d, stderr %q, want the error of the cancelled context", code, stderr.String())
			}
		})
	}
}
