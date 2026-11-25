// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/zclconf/go-cty/cty"

	"github.com/bacanoo-io/norca/norca/api"

	rigger "github.com/bacanoo-io/norca/rigger"
)

// The values of the flags reach the servers as a file of values that reads
// back to the same values.
func TestValuesHCL(t *testing.T) {
	t.Parallel()

	values := rigger.Values{
		"count": {Value: cty.NumberIntVal(3)},
		"name":  {Value: cty.StringVal("web \"one\"")},
		"tags":  {Value: cty.TupleVal([]cty.Value{cty.StringVal("a"), cty.StringVal("b")})},
		"on":    {Value: cty.True},
	}

	src := valuesHCL(values)

	got, diags := rigger.ParseValues("values.vars.hcl", []byte(src))
	if diags.HasErrors() {
		t.Fatalf("%s: %v", src, diags)
	}

	if len(got) != len(values) {
		t.Fatalf("read %d values of %s", len(got), src)
	}

	for name, v := range values {
		if !got[name].Value.RawEquals(v.Value) {
			t.Errorf("%s = %#v, want %#v", name, got[name].Value, v.Value)
		}
	}

	if strings.Index(src, "count") > strings.Index(src, "tags") {
		t.Errorf("the values are not in the order of their names:\n%s", src)
	}
}

// A record of the log of an operation is written through the logger of the
// command, at its level, with its time, message and attributes.
func TestLogLine(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	ctx := context.Background()

	logLine(ctx, logger, map[string]any{"time": "2026-11-22T10:00:00Z", "level": "DEBUG", "msg": "hidden"})
	logLine(ctx, logger, map[string]any{
		"time": "2026-11-22T10:00:01.5Z", "level": "WARN", "msg": "release.apply", "release": "front", "revision": float64(2),
	})

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("wrote %q, want the warning only", buf.String())
	}

	var got map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &got); err != nil {
		t.Fatal(err)
	}

	if got["msg"] != "release.apply" || got["level"] != "WARN" || got["release"] != "front" || got["time"] != "2026-11-22T10:00:01.5Z" {
		t.Errorf("wrote %v", got)
	}

	// The tree a person reads takes the depth of an operation as an integer.
	var tree bytes.Buffer

	logger, err := newLogger(&tree, "text", "info")
	if err != nil {
		t.Fatal(err)
	}

	logLine(ctx, logger, map[string]any{
		"time": "2026-11-22T10:00:01Z", "level": "INFO", "msg": "release.apply started",
		"op": "a1", "depth": float64(1), "duration": "1.5ms", "revision": float64(2),
	})

	if !strings.Contains(tree.String(), "release.apply started") || !strings.Contains(tree.String(), "revision=2") {
		t.Errorf("wrote %q", tree.String())
	}
}

// What an operation did is written; one that did not succeed is an error
// with its phase and error.
func TestWriteOperation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		phase   string
		errText string
		wantErr string
	}{
		{name: "succeeded", phase: "Succeeded"},
		{name: "failed", phase: "Failed", errText: "release web: timeout", wantErr: "is Failed: release web: timeout"},
		{name: "cancelled", phase: "Cancelled", wantErr: "was cancelled"},
		{name: "interrupted", phase: "Interrupted", errText: "the leader changed", wantErr: "is Interrupted"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			const ns = "shop"

			op := &api.RiggerOperation{Name: "shop-apply-1", Status: api.RiggerOperationStatus{
				Phase: tt.phase, Error: tt.errText,
				Outcomes: []api.RiggerOperationRelease{
					{Namespace: ns, Release: "db", Action: "install", Revision: 1},
					{Namespace: ns, Release: "cache", Action: "unchanged", Revision: 4},
					{Namespace: ns, Release: "api", Action: "install", Failed: true},
					{Namespace: ns, Release: "web", Action: "upgrade", Skipped: true},
				},
			}}

			var buf bytes.Buffer

			err := writeOperation(&buf, op)

			want := "install shop/db: done (revision 1)\ninstall shop/api: failed\nupgrade shop/web: skipped\n"
			if buf.String() != want {
				t.Errorf("wrote %q, want %q", buf.String(), want)
			}

			switch {
			case tt.wantErr == "" && err != nil:
				t.Errorf("writeOperation() = %v", err)
			case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
				t.Errorf("writeOperation() = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

// -server is refused on the commands the servers don't run, and
// RIGGER_SERVER turns it on.
func TestServerFlag(t *testing.T) {
	for _, args := range [][]string{
		{cmdStack, "validate", "-server"},
		{cmdStack, "sync", "-server"},
		{"bundle", "create", "-server", "-o", "x.riggerbundle"},
	} {
		var stderr bytes.Buffer
		if code := Run(context.Background(), args, &bytes.Buffer{}, &stderr); code != exitError || !strings.Contains(stderr.String(), "-server works with") {
			t.Errorf("%v: exit %d, %q", args, code, stderr.String())
		}
	}

	if serverDefault() {
		t.Fatal("the server mode is on without RIGGER_SERVER")
	}

	t.Setenv(serverEnv, "1")

	if !serverDefault() {
		t.Error("RIGGER_SERVER=1 does not turn the server mode on")
	}

	// The variable turns it on where it works: the local commands stay.
	for _, args := range [][]string{
		{cmdStack, "validate", t.TempDir()},
		{cmdStack, "sync", t.TempDir()},
		{cmdStack, "publish", t.TempDir()},
		{"bundle", "create", t.TempDir()},
		{"bundle", "inspect", t.TempDir()},
	} {
		var stderr bytes.Buffer

		Run(t.Context(), args, &bytes.Buffer{}, &stderr)

		if strings.Contains(stderr.String(), "-server works with") {
			t.Errorf("%v with %s=1: %q", args, serverEnv, stderr.String())
		}
	}
}

// A done context while the command follows an operation (a Ctrl-C) asks
// the servers to cancel it, and the command follows it to its end.
func TestFollowOperation_Cancel(t *testing.T) {
	t.Parallel()

	var cancelled atomic.Bool

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch {
		case r.Method == http.MethodPut && r.URL.Path == "/v1/rigger/operation/op1/cancel":
			cancelled.Store(true)

			_, _ = w.Write([]byte(`{"name":"op1","status":{"phase":"Running","cancelRequested":true}}`))
		case r.URL.Path == "/v1/rigger/operation/op1/log":
			if !cancelled.Load() {
				_, _ = w.Write([]byte(`{"Lines":[],"Phase":"Running","Finished":false}`))

				return
			}

			_, _ = w.Write([]byte(`{"Lines":[{"Seq":1,"Line":{"level":"INFO","msg":"stack.apply stopped"}}],"Phase":"Cancelled","Finished":true}`))
		case r.URL.Path == "/v1/object/rigger.norca.io/Operation/op1":
			_, _ = w.Write([]byte(`{"Namespace":"shop","Name":"op1","Spec":{"stack":"shop","action":"apply"},` +
				`"Status":{"phase":"Cancelled","outcomes":[{"namespace":"shop","release":"db","action":"install","revision":1}]}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	cfg := api.DefaultConfig()
	cfg.Address = srv.URL

	client, err := api.NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var stdout, stderr, logs bytes.Buffer

	logger := slog.New(slog.NewTextHandler(&logs, nil))

	err = followOperation(ctx, logger, &stdout, &stderr, client, "shop", "op1")
	if err == nil || !strings.Contains(err.Error(), "was cancelled") {
		t.Errorf("followOperation() = %v", err)
	}

	if !cancelled.Load() {
		t.Error("the operation was not cancelled")
	}

	if !strings.Contains(logs.String(), "stack.apply stopped") || stdout.String() != "install shop/db: done (revision 1)\n" {
		t.Errorf("log %q, out %q", logs.String(), stdout.String())
	}

	if !strings.Contains(stderr.String(), "cancelling the operation op1") {
		t.Errorf("stderr %q", stderr.String())
	}
}

// Where the log can't be read the command follows the phase, says so once
// and has the result of the operation.
func TestFollowOperation_WithoutLog(t *testing.T) {
	t.Parallel()

	var reads atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch r.URL.Path {
		case "/v1/rigger/operation/op1/log":
			phase, finished := "Running", "false"
			if reads.Add(1) > 2 {
				phase, finished = "Succeeded", "true"
			}

			_, _ = w.Write([]byte(`{"Lines":[],"Phase":"` + phase + `","Finished":` + finished + `,"LogUnavailable":"the bus is not ready"}`))
		case "/v1/object/rigger.norca.io/Operation/op1":
			_, _ = w.Write([]byte(`{"Namespace":"shop","Name":"op1","Spec":{"stack":"shop","action":"apply"},"Status":{"phase":"Succeeded"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	cfg := api.DefaultConfig()
	cfg.Address = srv.URL

	client, err := api.NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer

	if err := followOperation(t.Context(), slog.New(slog.DiscardHandler), &stdout, &stderr, client, "shop", "op1"); err != nil {
		t.Errorf("followOperation() = %v", err)
	}

	if strings.Count(stderr.String(), "the bus is not ready") != 1 {
		t.Errorf("stderr %q, want the reason once", stderr.String())
	}
}
