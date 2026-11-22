// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/hashicorp/go-hclog"
	"github.com/shoenig/test/must"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/norca/mock"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/bacanoo-io/norca/norca/testutil"
	"github.com/bacanoo-io/norca/rigger/host"
)

// The hosts of Rigger that need no release request (design/
// rig-integration.md 4.3): the cluster, the secret store and the local store,
// through the endpoints of the server with the token given. The install of a
// module through the hosts is tested in command/agent, where the release
// requests are converted as on a real server.
func TestRiggerHosts_ClusterSecretsStore(t *testing.T) {
	ci.Parallel(t)

	s, root, cleanup := TestACLServer(t, nil)
	defer cleanup()
	testutil.WaitForLeader(t, s.RPC)

	h := &riggerHosts{srv: s, token: root.SecretID, namespace: structs.DefaultNamespace}
	ctx := context.Background()

	info, err := riggerCluster{h}.Info(ctx)
	must.NoError(t, err)
	must.Eq(t, s.Region(), info.Region)
	must.SliceContains(t, info.Features, "releases")

	names, err := riggerCluster{h}.Namespaces(ctx)
	must.NoError(t, err)
	must.SliceContains(t, names, structs.DefaultNamespace)

	// A secret is made once: a second Ensure without overwrite keeps it.
	secrets := riggerSecrets{h}
	written, err := secrets.Ensure(ctx, host.Secret{Path: "rigger/test", Items: map[string]host.Sensitive{"token": host.NewSensitive("one")}})
	must.NoError(t, err)
	must.True(t, written)

	written, err = secrets.Ensure(ctx, host.Secret{Path: "rigger/test", Items: map[string]host.Sensitive{"token": host.NewSensitive("two")}})
	must.NoError(t, err)
	must.False(t, written)

	items, err := secrets.Read(ctx, "rigger/test")
	must.NoError(t, err)
	must.Eq(t, "one", items["token"].Reveal())

	written, err = secrets.Ensure(ctx, host.Secret{Path: "rigger/test", Overwrite: true, Items: map[string]host.Sensitive{"token": host.NewSensitive("three")}})
	must.NoError(t, err)
	must.True(t, written)

	_, err = secrets.Read(ctx, "rigger/missing")
	must.True(t, errors.Is(err, host.ErrNotFound), must.Sprintf("err %v", err))

	// A token without rights on the secret store is refused.
	weak := mock.CreatePolicyAndToken(t, s.State(), 1100, "jobs-only", `namespace "default" { capabilities = ["read-job"] }`)
	_, err = riggerSecrets{&riggerHosts{srv: s, token: weak.SecretID, namespace: structs.DefaultNamespace}}.Read(ctx, "rigger/test")
	must.ErrorContains(t, err, "Permission denied")

	// The local store of the engine: keys under its directory only.
	store := riggerStore{dir: t.TempDir()}
	_, err = store.Get(ctx, "plans/a")
	must.True(t, errors.Is(err, host.ErrNotFound))
	must.NoError(t, store.Put(ctx, "plans/a", []byte("x")))
	data, err := store.Get(ctx, "plans/a")
	must.NoError(t, err)
	must.Eq(t, "x", string(data))
	must.NoError(t, store.Delete(ctx, "plans/a"))
	must.NoError(t, store.Delete(ctx, "plans/a"))
	must.Error(t, store.Put(ctx, "../outside", []byte("x")))

	// Without the conversion of release requests (set by the agent) there
	// is no engine.
	_, err = s.RiggerEngine(root.SecretID, structs.DefaultNamespace)
	must.ErrorContains(t, err, "can't be converted")
}

// The log of Rigger goes into the hclog logger of the server.
func TestRiggerHosts_LogHandler(t *testing.T) {
	ci.Parallel(t)

	var lines []string
	logger := hclog.NewInterceptLogger(&hclog.LoggerOptions{Level: hclog.Info, Output: hclogDiscard{}})
	logger.RegisterSink(hclog.NewSinkAdapter(&hclog.LoggerOptions{
		Level:  hclog.Info,
		Output: lineWriter{&lines},
	}))

	l := slog.New(&hclogHandler{logger: logger}).With("module", "web").WithGroup("op")
	l.Debug("hidden")
	l.Info("started", "release", "web")

	must.Len(t, 1, lines)
	must.StrContains(t, lines[0], "started")
	must.StrContains(t, lines[0], "module=web")
	must.StrContains(t, lines[0], "op.release=web")
}

type hclogDiscard struct{}

func (hclogDiscard) Write(p []byte) (int, error) { return len(p), nil }

type lineWriter struct{ lines *[]string }

func (w lineWriter) Write(p []byte) (int, error) {
	*w.lines = append(*w.lines, string(p))
	return len(p), nil
}
