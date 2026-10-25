// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/nomad/secretstore/ubix/export/barrier"
	"github.com/hashicorp/nomad/secretstore/ubix/export/core"
	"github.com/hashicorp/nomad/secretstore/ubix/export/kv"
	"github.com/hashicorp/nomad/secretstore/ubix/export/storage"
	"github.com/hashicorp/nomad/testutil"
	"github.com/shoenig/test/must"
	"github.com/shoenig/test/wait"
)

// testSecretStoreCore returns a uBixVault core with HA over the server's
// secret store backend, as the seal of a later phase creates it.
func testSecretStoreCore(s *Server) *core.Core {
	return core.New(s.secretStore, core.WithHA(core.HAConfig{
		Backend:  s.secretStore,
		HolderID: s.config.NodeID,
	}))
}

func waitForSecretStore(t *testing.T, what string, cond func() bool) {
	t.Helper()
	must.Wait(t, wait.InitialSuccess(
		wait.BoolFunc(cond),
		wait.Timeout(20*time.Second),
		wait.Gap(20*time.Millisecond),
	), must.Sprintf("waiting for %s", what))
}

// assertSecretStoreCiphertext fails if a blob in the server's state store
// contains the canary in the clear.
func assertSecretStoreCiphertext(t *testing.T, s *Server, canary string) {
	t.Helper()
	iter, err := s.State().SecretStoreBlobsByPrefix(nil, "")
	must.NoError(t, err)
	n := 0
	for raw := iter.Next(); raw != nil; raw = iter.Next() {
		blob := raw.(*structs.SecretStoreBlob)
		must.False(t, bytes.Contains(blob.Value, []byte(canary)), must.Sprintf("plaintext in %s", blob.Path))
		n++
	}
	must.Positive(t, n)
}

// TestSecretStore_BarrierRoundTrip initializes, unseals, writes, seals and
// unseals the uBixVault core on the Raft backend of a test server.
func TestSecretStore_BarrierRoundTrip(t *testing.T) {
	ci.Parallel(t)
	s1, cleanup := TestServer(t, nil)
	defer cleanup()
	testutil.WaitForLeader(t, s1.RPC)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := testSecretStoreCore(s1)

	// Initialize takes the HA lock, which is granted once the server has
	// established its leadership.
	init, err := c.Initialize(ctx, core.InitConfig{SecretShares: 5, SecretThreshold: 3})
	must.NoError(t, err)
	must.Len(t, 5, init.Keys)

	done := make(chan error, 1)
	go func() { done <- c.RunHA(ctx, t.Logf) }()

	// Fewer shares than the threshold leave it sealed.
	for _, share := range init.Keys[:2] {
		status, err := c.Unseal(ctx, share)
		must.NoError(t, err)
		must.True(t, status.Sealed)
	}
	status, err := c.Unseal(ctx, init.Keys[4])
	must.NoError(t, err)
	must.False(t, status.Sealed)
	waitForSecretStore(t, "active core", c.Active)

	secrets := kv.New(c.Barrier(), "secret")
	_, err = secrets.Write(ctx, "app/db", map[string]any{"password": "canary-s3cr3t"})
	must.NoError(t, err)
	data, _, err := secrets.Read(ctx, "app/db", 0)
	must.NoError(t, err)
	must.Eq(t, "canary-s3cr3t", data["password"])
	assertSecretStoreCiphertext(t, s1, "canary-s3cr3t")

	// Sealed, nothing can be read.
	c.Seal()
	_, _, err = secrets.Read(ctx, "app/db", 0)
	must.ErrorIs(t, err, barrier.ErrSealed)
	waitForSecretStore(t, "inactive core", func() bool { return !c.Active() })

	for _, share := range init.Keys[1:4] {
		_, err := c.Unseal(ctx, share)
		must.NoError(t, err)
	}
	waitForSecretStore(t, "active core after unseal", c.Active)
	data, meta, err := secrets.Read(ctx, "app/db", 0)
	must.NoError(t, err)
	must.Eq(t, "canary-s3cr3t", data["password"])
	must.Eq(t, 1, meta.Version)

	// The blobs survive a snapshot and restore of the FSM unchanged.
	fsm2 := testSnapshotRestore(t, s1.fsm)
	iter, err := s1.State().SecretStoreBlobsByPrefix(nil, "")
	must.NoError(t, err)
	for raw := iter.Next(); raw != nil; raw = iter.Next() {
		want := raw.(*structs.SecretStoreBlob)
		got, err := fsm2.State().SecretStoreBlobByPath(nil, want.Path)
		must.NoError(t, err)
		must.Eq(t, want, got)
	}

	cancel()
	select {
	case err := <-done:
		must.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("RunHA did not return")
	}
}

// TestSecretStore_LeadershipLoss checks that the core is active only on the
// Nomad leader: after a leadership transfer the old leader's core steps
// down and its writes are fenced, and the new leader's core takes over with
// the replicated data.
func TestSecretStore_LeadershipLoss(t *testing.T) {
	ci.Parallel(t)
	var servers []*Server
	for i := 0; i < 3; i++ {
		s, cleanup := TestServer(t, func(c *Config) { c.BootstrapExpect = 3 })
		defer cleanup()
		servers = append(servers, s)
	}
	TestJoin(t, servers...)
	for _, s := range servers {
		testutil.WaitForLeader(t, s.RPC)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	leaderOf := func() *Server {
		for _, s := range servers {
			if s.IsLeader() {
				return s
			}
		}
		return nil
	}
	cores := map[*Server]*core.Core{}
	for _, s := range servers {
		cores[s] = testSecretStoreCore(s)
	}

	var leader *Server
	waitForSecretStore(t, "leader", func() bool { leader = leaderOf(); return leader != nil })
	init, err := cores[leader].Initialize(ctx, core.InitConfig{SecretShares: 3, SecretThreshold: 2})
	must.NoError(t, err)

	// Every server unseals its own core, once the seal configuration (the
	// last write of Initialize) has been replicated to it.
	for _, s := range servers {
		s := s
		waitForSecretStore(t, "replicated seal configuration", func() bool {
			blob, err := s.State().SecretStoreBlobByPath(nil, "core/seal-config")
			return err == nil && blob != nil
		})
		for _, share := range init.Keys[:2] {
			_, err := cores[s].Unseal(ctx, share)
			must.NoError(t, err)
		}
		go cores[s].RunHA(ctx, t.Logf)
	}

	waitForSecretStore(t, "active leader core", cores[leader].Active)
	for _, s := range servers {
		if s != leader {
			waitForSecretStore(t, "standby follower core", cores[s].Standby)
			info, err := cores[s].Leader(ctx)
			must.NoError(t, err)
			must.Eq(t, leader.config.NodeID, info.HolderID)
			must.False(t, info.IsSelf)
		}
	}
	must.NoError(t, cores[leader].Barrier().Put(ctx, &storage.Entry{Key: "secret/app", Value: []byte("canary-1")}))

	// Hand leadership to another server.
	old := leader
	must.NoError(t, old.raft.LeadershipTransfer().Error())
	waitForSecretStore(t, "new leader", func() bool {
		leader = leaderOf()
		return leader != nil && leader != old
	})

	waitForSecretStore(t, "old leader core standby", cores[old].Standby)
	must.ErrorIs(t, cores[old].Barrier().Put(ctx, &storage.Entry{Key: "secret/app", Value: []byte("stale")}), storage.ErrFenced)

	waitForSecretStore(t, "new leader core active", cores[leader].Active)
	e, err := cores[leader].Barrier().Get(ctx, "secret/app")
	must.NoError(t, err)
	must.Eq(t, []byte("canary-1"), e.Value)
	must.NoError(t, cores[leader].Barrier().Put(ctx, &storage.Entry{Key: "secret/app", Value: []byte("canary-2")}))

	// The old leader reads the new value once it is replicated.
	waitForSecretStore(t, "replicated write", func() bool {
		e, err := cores[old].Barrier().Get(ctx, "secret/app")
		return err == nil && e != nil && string(e.Value) == "canary-2"
	})
	for _, s := range servers {
		assertSecretStoreCiphertext(t, s, "canary-")
	}
}
