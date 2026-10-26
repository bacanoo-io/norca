// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package secretstore

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/nomad/secretstore/ubix/export/audit"
	"github.com/shoenig/test/must"
)

func TestAuditFile_Records(t *testing.T) {
	ci.Parallel(t)
	path := filepath.Join(t.TempDir(), "audit.log")
	f, err := NewAuditFile(AuditFileConfig{Path: path})
	must.NoError(t, err)
	broker := audit.NewBroker(f)

	rec := &structs.SecretAccessEvent{
		Region: "global", Server: "s1", Namespace: "prod", Path: "db", Version: 3,
		Operation: structs.SecretOpRead, Result: structs.SecretAccessResultOK,
		Accessor: &structs.SecretAccessor{JobID: "web", AllocationID: "a1", NodeID: "n1"},
	}
	ctx := WithAuditRecord(context.Background(), rec)
	must.NoError(t, broker.LogRequest(ctx, &audit.Entry{Operation: "read", Path: "prod/db"}))
	must.NoError(t, broker.LogResponse(ctx, &audit.Entry{Operation: "read", Path: "prod/db"}))
	// Without a Norca record the uBixVault entry is written.
	must.NoError(t, broker.LogResponse(context.Background(), &audit.Entry{Operation: "list", Path: "x"}))
	must.NoError(t, broker.Close())

	data, err := os.ReadFile(path)
	must.NoError(t, err)
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	must.Len(t, 3, lines)
	var req, resp, plain map[string]any
	must.NoError(t, json.Unmarshal([]byte(lines[0]), &req))
	must.NoError(t, json.Unmarshal([]byte(lines[1]), &resp))
	must.NoError(t, json.Unmarshal([]byte(lines[2]), &plain))
	must.Eq(t, "request", req["type"])
	must.Nil(t, req["result"])
	must.Eq(t, "response", resp["type"])
	must.Eq(t, "ok", resp["result"])
	must.Eq(t, "prod", resp["namespace"])
	must.Eq(t, any(float64(3)), resp["version"])
	must.Eq(t, "web", resp["accessor"].(map[string]any)["JobID"])
	must.Eq(t, "x", plain["path"])

	info, err := os.Stat(path)
	must.NoError(t, err)
	if os.PathSeparator == '/' {
		must.Eq(t, os.FileMode(0o600), info.Mode().Perm())
	}

	// Closed devices refuse records: the broker is fail closed.
	must.Error(t, f.Log(ctx, &audit.Entry{Type: "response"}))
}

func TestAuditFile_Rotation(t *testing.T) {
	ci.Parallel(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.log")
	f, err := NewAuditFile(AuditFileConfig{Path: path, RotateBytes: 300, RotateMaxFiles: 2})
	must.NoError(t, err)
	defer f.Close()
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	f.now = func() time.Time { now = now.Add(time.Second); return now }

	rec := &structs.SecretAccessEvent{Namespace: "default", Path: strings.Repeat("p", 100), Operation: "read"}
	ctx := WithAuditRecord(context.Background(), rec)
	for i := 0; i < 10; i++ {
		must.NoError(t, f.Log(ctx, &audit.Entry{Type: "response", Time: now}))
	}
	rotated := f.Rotated()
	must.Len(t, 2, rotated)
	for _, p := range append(rotated, path) {
		info, err := os.Stat(p)
		must.NoError(t, err)
		must.LessEq(t, 300, info.Size())
	}

	// Time based rotation.
	path2 := filepath.Join(dir, "timed.log")
	g, err := NewAuditFile(AuditFileConfig{Path: path2, RotateDuration: time.Hour})
	must.NoError(t, err)
	defer g.Close()
	clock := now
	g.now = func() time.Time { return clock }
	g.opened = clock
	must.NoError(t, g.Log(ctx, &audit.Entry{Type: "response", Time: clock}))
	clock = clock.Add(2 * time.Hour)
	must.NoError(t, g.Log(ctx, &audit.Entry{Type: "response", Time: clock}))
	must.Len(t, 1, g.Rotated())
}
