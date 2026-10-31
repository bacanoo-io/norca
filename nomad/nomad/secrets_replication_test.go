// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/nomad/secretsrepl"
	"github.com/hashicorp/nomad/nomad/stream"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/nomad/testutil"
	"github.com/shoenig/test/must"
	"github.com/shoenig/test/wait"
)

// secretReplicationServers starts a server of the authoritative region
// "region1" and one of "region2" that replicates from it, joined over WAN.
// With acl, the second region uses the root token of the first as its
// replication token. cb2 configures the second server.
func secretReplicationServers(t *testing.T, acl bool, cb2 func(*Config)) (s1, s2 *Server, root1, root2 *structs.ACLToken, cleanup func()) {
	t.Helper()
	s1, root1, cleanup1 := secretTestServer(t, acl, func(c *Config) {
		c.Region = "region1"
		c.AuthoritativeRegion = "region1"
	})
	s2, root2, cleanup2 := secretTestServer(t, acl, func(c *Config) {
		c.Region = "region2"
		c.AuthoritativeRegion = "region1"
		c.ReplicationBackoff = 20 * time.Millisecond
		c.SecretsReplicationInterval = 500 * time.Millisecond
		if root1 != nil {
			c.ReplicationToken = root1.SecretID
		}
		if cb2 != nil {
			cb2(c)
		}
	})
	TestJoin(t, s1, s2)
	if acl {
		// The bootstrap tokens are global: region2 replaces its own with
		// the tokens of region1.
		testutil.WaitForResult(func() (bool, error) {
			token, err := s2.State().ACLTokenBySecretID(nil, root1.SecretID)
			return token != nil, err
		}, func(err error) { t.Fatalf("token not replicated: %v", err) })
		root2 = root1
	}
	return s1, s2, root1, root2, func() { cleanup2(); cleanup1() }
}

func secretToken(token *structs.ACLToken) string {
	if token == nil {
		return ""
	}
	return token.SecretID
}

// replWrite writes a secret in a region.
func replWrite(t *testing.T, s *Server, token *structs.ACLToken, path string, items structs.SecretItems, meta *structs.SecretMeta) (*structs.Secret, error) {
	t.Helper()
	req := secretWriteReq(path, secretToken(token), items)
	req.Region = s.Region()
	req.Meta = meta
	var w structs.SecretWriteResponse
	err := s.RPC(structs.SecretUpsertRPCMethod, req, &w)
	return w.Secret, err
}

// replVersionsOp deletes, undeletes or destroys versions of a secret.
func replVersionsOp(t *testing.T, s *Server, token *structs.ACLToken, method, path string, versions ...uint64) error {
	t.Helper()
	req := secretVersionsReq(path, secretToken(token), versions...)
	req.Region = s.Region()
	var w structs.SecretWriteResponse
	return s.RPC(method, req, &w)
}

// replRead reads the value of a version of a secret in a region.
func replRead(t *testing.T, s *Server, token *structs.ACLToken, path string, version uint64) (*structs.SecretReadResponse, error) {
	t.Helper()
	q := secretQuery(secretToken(token))
	q.Region = s.Region()
	var r structs.SecretReadResponse
	err := s.RPC(structs.SecretReadRPCMethod, &structs.SecretReadRequest{Path: path, Version: version, QueryOptions: q}, &r)
	return &r, err
}

// replicaOf returns the row of a path in the server's state.
func replicaOf(t *testing.T, s *Server, path string) *structs.Secret {
	t.Helper()
	row, err := s.State().SecretByPath(nil, structs.DefaultNamespace, path)
	must.NoError(t, err)
	return row
}

// waitReplica waits until the replica of path on s2 matches the row of s1
// (or until it is gone, if the secret is not replicated in s1).
func waitReplica(t *testing.T, s1, s2 *Server, path string) {
	t.Helper()
	must.Wait(t, wait.InitialSuccess(wait.ErrorFunc(func() error {
		return replicaMatches(t, s1, s2, path)
	}), wait.Timeout(20*time.Second), wait.Gap(50*time.Millisecond)))
}

func replicaMatches(t *testing.T, s1, s2 *Server, path string) error {
	src := replicaOf(t, s1, path)
	local := replicaOf(t, s2, path)
	if src == nil || !src.Replicate {
		if local != nil && local.Replica != nil {
			return fmt.Errorf("replica of %s still exists", path)
		}
		return nil
	}
	if !secretReplicaInSync(local, s1.Region(), src) {
		return fmt.Errorf("replica of %s not in sync", path)
	}
	return nil
}

// accessRecords collects the SecretAccess records of a server.
type accessRecords struct {
	sub *stream.Subscription
}

func subscribeAccess(t *testing.T, s *Server) *accessRecords {
	t.Helper()
	broker, err := s.State().EventBroker()
	must.NoError(t, err)
	sub, err := broker.Subscribe(&stream.SubscribeRequest{
		Topics:    map[structs.Topic][]string{structs.TopicSecretAccess: {"*"}},
		Namespace: "*",
	})
	must.NoError(t, err)
	t.Cleanup(sub.Unsubscribe)
	return &accessRecords{sub: sub}
}

// waitFor returns the records until one matches.
func (a *accessRecords) waitFor(t *testing.T, match func(*structs.SecretAccessEvent) bool) *structs.SecretAccessEvent {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for {
		events, err := a.sub.Next(ctx)
		must.NoError(t, err)
		for _, e := range events.Events {
			if rec, ok := e.Payload.(*structs.SecretAccessEvent); ok && match(rec) {
				return rec
			}
		}
	}
}

func TestSecretReplication_Loop(t *testing.T) {
	ci.Parallel(t)
	s1, s2, root1, root2, cleanup := secretReplicationServers(t, true, nil)
	defer cleanup()
	audit1 := subscribeAccess(t, s1)
	audit2 := subscribeAccess(t, s2)

	// A replicated and a region-local secret in the authoritative region.
	src, err := replWrite(t, s1, root1, "app/shared", structs.SecretItems{"password": secretCanary},
		&structs.SecretMeta{Replicate: true, Owner: "team-a", RotateEvery: time.Hour})
	must.NoError(t, err)
	must.True(t, src.Replicate)
	_, err = replWrite(t, s1, root1, "app/local", structs.SecretItems{"password": "local"}, nil)
	must.NoError(t, err)

	waitReplica(t, s1, s2, "app/shared")
	replica := replicaOf(t, s2, "app/shared")
	must.Eq(t, "region1", replica.Replica.Region)
	must.Eq(t, src.CreateIndex, replica.Replica.SourceCreateIndex)
	must.Eq(t, "team-a", replica.Owner)
	must.True(t, replica.Replicate)
	must.Eq(t, []string{"password"}, replica.ItemKeys)
	// Re-encrypted with the keys of region2: its own keyed hash, and the
	// hash of region1 kept for the comparison.
	must.NotEq(t, src.ValueHash, replica.ValueHash)
	must.StrHasPrefix(t, structs.SecretValueHashPrefix, replica.ValueHash)
	must.Eq(t, src.ValueHash, replica.Version(1).SourceHash)
	must.Nil(t, replicaOf(t, s2, "app/local"))

	r, err := replRead(t, s2, root2, "app/shared", 0)
	must.NoError(t, err)
	must.Eq(t, secretCanary, r.Items["password"])
	must.Eq(t, 1, r.Version)

	// Audit: the read for region2 in region1, the replica write in
	// region2, both by the system accessor, without values.
	isReplicate := func(rec *structs.SecretAccessEvent) bool {
		return rec.Operation == structs.SecretOpReplicate && rec.Path == "app/shared" &&
			rec.Result == structs.SecretAccessResultOK
	}
	rec1 := audit1.waitFor(t, isReplicate)
	must.Eq(t, "region2", rec1.PeerRegion)
	must.Eq(t, secretReplicationAccessor, rec1.Accessor.System)
	must.Eq(t, root1.AccessorID, rec1.Accessor.AccessorID)
	must.Eq(t, []uint64{1}, rec1.Versions)
	rec2 := audit2.waitFor(t, isReplicate)
	must.Eq(t, "region1", rec2.PeerRegion)
	must.Eq(t, "region2", rec2.Region)
	must.Eq(t, secretReplicationAccessor, rec2.Accessor.System)
	must.Eq(t, []uint64{1}, rec2.Versions)

	// A new version, then soft delete and destroy.
	_, err = replWrite(t, s1, root1, "app/shared", structs.SecretItems{"password": "two"}, nil)
	must.NoError(t, err)
	waitReplica(t, s1, s2, "app/shared")
	r, err = replRead(t, s2, root2, "app/shared", 0)
	must.NoError(t, err)
	must.Eq(t, "two", r.Items["password"])
	must.Eq(t, 2, r.Version)
	r, err = replRead(t, s2, root2, "app/shared", 1)
	must.NoError(t, err)
	must.Eq(t, secretCanary, r.Items["password"])

	must.NoError(t, replVersionsOp(t, s1, root1, structs.SecretDeleteRPCMethod, "app/shared", 2))
	must.NoError(t, replVersionsOp(t, s1, root1, structs.SecretDestroyRPCMethod, "app/shared", 1))
	waitReplica(t, s1, s2, "app/shared")
	replica = replicaOf(t, s2, "app/shared")
	must.True(t, replica.Deleted)
	must.True(t, replica.Version(1).Destroyed)
	must.Eq(t, "", replica.Version(1).ValueHash)
	_, err = replRead(t, s2, root2, "app/shared", 1)
	must.ErrorContains(t, err, "destroyed")
	blob, err := s2.State().SecretStoreBlobByPath(nil, secretKVDataKey("default", "app/shared", 1))
	must.NoError(t, err)
	must.Nil(t, blob)

	// The undelete restores the value, which region2 kept.
	must.NoError(t, replVersionsOp(t, s1, root1, structs.SecretUndeleteRPCMethod, "app/shared", 2))
	waitReplica(t, s1, s2, "app/shared")
	r, err = replRead(t, s2, root2, "app/shared", 0)
	must.NoError(t, err)
	must.Eq(t, "two", r.Items["password"])

	// Read-only in region2: writes, metadata, deletes and destroys.
	_, err = replWrite(t, s2, root2, "app/shared", structs.SecretItems{"password": "x"}, nil)
	must.ErrorContains(t, err, "read-only in region \"region2\"")
	_, err = replWrite(t, s2, root2, "app/shared", nil, &structs.SecretMeta{Owner: "me"})
	must.ErrorContains(t, err, "read-only")
	must.ErrorContains(t, replVersionsOp(t, s2, root2, structs.SecretDeleteRPCMethod, "app/shared"), "read-only")
	must.ErrorContains(t, replVersionsOp(t, s2, root2, structs.SecretDestroyRPCMethod, "app/shared"), "read-only")
	// Replicate can only be set in the authoritative region.
	_, err = replWrite(t, s2, root2, "app/other", structs.SecretItems{"a": "b"}, &structs.SecretMeta{Replicate: true})
	must.ErrorContains(t, err, "authoritative region")
	// Local secrets of region2 stay writable.
	_, err = replWrite(t, s2, root2, "app/mine", structs.SecretItems{"a": "b"}, nil)
	must.NoError(t, err)

	// The secrets watcher of region2 dispatches no rotation for a replica.
	tw := newTestWatcher(s2, time.Now().Add(3*time.Hour))
	events, _ := tw.run()
	must.SliceNotContains(t, events, structs.TypeSecretRotationDue+" app/shared")

	// Turning replication off removes the replica (a tombstone).
	_, err = replWrite(t, s1, root1, "app/shared", nil, &structs.SecretMeta{Owner: "team-a"})
	must.NoError(t, err)
	waitReplica(t, s1, s2, "app/shared")
	must.Nil(t, replicaOf(t, s2, "app/shared"))
	audit2.waitFor(t, func(rec *structs.SecretAccessEvent) bool {
		return rec.Operation == structs.SecretOpDestroy && rec.Path == "app/shared" &&
			rec.Accessor.System == secretReplicationAccessor
	})

	// And so does destroying the secret.
	_, err = replWrite(t, s1, root1, "app/shared", nil, &structs.SecretMeta{Replicate: true})
	must.NoError(t, err)
	waitReplica(t, s1, s2, "app/shared")
	must.NotNil(t, replicaOf(t, s2, "app/shared"))
	must.NoError(t, replVersionsOp(t, s1, root1, structs.SecretDestroyRPCMethod, "app/shared"))
	waitReplica(t, s1, s2, "app/shared")
	must.Nil(t, replicaOf(t, s2, "app/shared"))
	must.NotNil(t, replicaOf(t, s2, "app/mine"))
}

// testReplicator is a replicator of the second region driven by the test.
func testReplicator(s *Server) (*secretReplicator, *structs.SecretReplicationListRequest) {
	r := newSecretReplicator(s)
	req := &structs.SecretReplicationListRequest{
		ReplicaRegion: s.Region(),
		QueryOptions:  structs.QueryOptions{Region: r.region, AllowStale: true},
	}
	return r, req
}

func TestSecretReplication_CatchUpAndReconcile(t *testing.T) {
	ci.Parallel(t)
	s1, s2, _, _, cleanup := secretReplicationServers(t, false, func(c *Config) {
		c.secretsReplicationDisabled = true
	})
	defer cleanup()
	testutil.WaitForResult(func() (bool, error) {
		return s2.hasRegionServers("region1"), nil
	}, func(err error) { t.Fatal("region1 not known in region2") })
	repl, req := testReplicator(s2)
	round := func() {
		t.Helper()
		_, err := repl.round(req)
		must.NoError(t, err)
	}
	replicated := &structs.SecretMeta{Replicate: true}

	// History written while region2 does not replicate (offline): more
	// versions than one fetch carries, a deleted version region2 never
	// sees the value of, a destroyed one and a purged secret.
	for i := 1; i <= 12; i++ {
		_, err := replWrite(t, s1, nil, "many", structs.SecretItems{"v": fmt.Sprint(i)}, &structs.SecretMeta{Replicate: true, MaxVersions: 20})
		must.NoError(t, err)
	}
	_, err := replWrite(t, s1, nil, "hist", structs.SecretItems{"v": "1"}, replicated)
	must.NoError(t, err)
	_, err = replWrite(t, s1, nil, "hist", structs.SecretItems{"v": "2"}, nil)
	must.NoError(t, err)
	_, err = replWrite(t, s1, nil, "hist", structs.SecretItems{"v": "3"}, nil)
	must.NoError(t, err)
	must.NoError(t, replVersionsOp(t, s1, nil, structs.SecretDeleteRPCMethod, "hist", 2))
	must.NoError(t, replVersionsOp(t, s1, nil, structs.SecretDestroyRPCMethod, "hist", 1))
	_, err = replWrite(t, s1, nil, "gone", structs.SecretItems{"v": "1"}, replicated)
	must.NoError(t, err)
	// A path of region2's own: never overwritten by the replica.
	_, err = replWrite(t, s2, nil, "conflict", structs.SecretItems{"v": "region2"}, nil)
	must.NoError(t, err)
	_, err = replWrite(t, s1, nil, "conflict", structs.SecretItems{"v": "region1"}, replicated)
	must.NoError(t, err)

	round()
	for _, path := range []string{"many", "hist", "gone"} {
		must.NoError(t, replicaMatches(t, s1, s2, path), must.Sprint(path))
	}
	must.Len(t, 12, replicaOf(t, s2, "many").Versions)
	for _, v := range []uint64{1, 7, 12} {
		r, err := replRead(t, s2, nil, "many", v)
		must.NoError(t, err)
		must.Eq(t, fmt.Sprint(v), r.Items["v"])
	}
	hist := replicaOf(t, s2, "hist")
	must.True(t, hist.Version(2).Deleted)
	must.Eq(t, "", hist.Version(2).ValueHash, must.Sprint("the value of a deleted version is not fetched"))
	must.True(t, hist.Version(1).Destroyed)
	conflict := replicaOf(t, s2, "conflict")
	must.Nil(t, conflict.Replica)
	r, err := replRead(t, s2, nil, "conflict", 0)
	must.NoError(t, err)
	must.Eq(t, "region2", r.Items["v"])

	// A round without changes changes nothing.
	index := replicaOf(t, s2, "hist").ModifyIndex
	round()
	must.Eq(t, index, replicaOf(t, s2, "hist").ModifyIndex)

	// Another offline period: an undelete, a new version beyond
	// max_versions, a purge.
	must.NoError(t, replVersionsOp(t, s1, nil, structs.SecretUndeleteRPCMethod, "hist", 2))
	_, err = replWrite(t, s1, nil, "hist", structs.SecretItems{"v": "4"}, &structs.SecretMeta{Replicate: true, MaxVersions: 3})
	must.NoError(t, err)
	must.NoError(t, replVersionsOp(t, s1, nil, structs.SecretDestroyRPCMethod, "gone"))
	round()
	must.NoError(t, replicaMatches(t, s1, s2, "hist"))
	must.Nil(t, replicaOf(t, s2, "gone"))
	hist = replicaOf(t, s2, "hist")
	must.Len(t, 3, hist.Versions)
	must.Eq(t, 2, hist.Versions[0].Version)
	r, err = replRead(t, s2, nil, "hist", 2)
	must.NoError(t, err)
	must.Eq(t, "2", r.Items["v"], must.Sprint("the undeleted value is fetched"))
	blob, err := s2.State().SecretStoreBlobByPath(nil, secretKVDataKey("default", "hist", 1))
	must.NoError(t, err)
	must.Nil(t, blob)

	// Reconciliation: a replica that differs from the authoritative region
	// although its modify index matches (for example restored from an
	// older backup of region2) is repaired by the next comparison.
	broken := replicaOf(t, s2, "hist").Copy()
	broken.Version(3).SourceHash = structs.SecretValueHashPrefix + "bogus"
	_, _, err = s2.raftApply(structs.SecretApplyRequestType|structs.IgnoreUnknownTypeFlag,
		&structs.SecretApplyRequest{Secret: broken, ExpectedIndex: broken.ModifyIndex})
	must.NoError(t, err)
	must.Error(t, replicaMatches(t, s1, s2, "hist"))
	round()
	must.NoError(t, replicaMatches(t, s1, s2, "hist"))
	r, err = replRead(t, s2, nil, "hist", 3)
	must.NoError(t, err)
	must.Eq(t, "3", r.Items["v"])

	// A newer replica is not replaced by an older state of the
	// authoritative region (for example after its restore).
	newer := replicaOf(t, s2, "hist").Copy()
	newer.CurrentVersion = 9
	_, _, err = s2.raftApply(structs.SecretApplyRequestType|structs.IgnoreUnknownTypeFlag,
		&structs.SecretApplyRequest{Secret: newer, ExpectedIndex: newer.ModifyIndex})
	must.NoError(t, err)
	round()
	must.Eq(t, 9, replicaOf(t, s2, "hist").CurrentVersion)
}

func TestSecretReplication_KeyAndAuth(t *testing.T) {
	ci.Parallel(t)
	s1, s2, root1, root2, cleanup := secretReplicationServers(t, true, func(c *Config) {
		c.secretsReplicationDisabled = true
	})
	defer cleanup()
	testutil.WaitForResult(func() (bool, error) {
		return s2.hasRegionServers("region1") && s1.hasRegionServers("region2"), nil
	}, func(err error) { t.Fatal("regions not joined") })

	_, err := replWrite(t, s1, root1, "app/db", structs.SecretItems{"password": secretCanary}, &structs.SecretMeta{Replicate: true})
	must.NoError(t, err)

	// region2's key, published through Secret.ReplicationKey.
	key, err := s2.secretReplicationKey()
	must.NoError(t, err)
	again, err := s2.secretReplicationKey()
	must.NoError(t, err)
	must.Eq(t, key.ID(), again.ID())
	var keyReply structs.SecretReplicationKeyResponse
	must.NoError(t, s1.RPC(structs.SecretReplicationKeyRPCMethod,
		&structs.SecretReplicationKeyRequest{QueryOptions: structs.QueryOptions{Region: "region2"}}, &keyReply))
	must.Eq(t, "region2", keyReply.Region)
	must.Eq(t, key.ID(), keyReply.KeyID)
	must.Eq(t, key.PublicKey(), keyReply.PublicKey)

	replicate := func(token, region, keyID string) (*structs.SecretReplicateResponse, error) {
		var reply structs.SecretReplicateResponse
		err := s2.RPC(structs.SecretReplicateRPCMethod, &structs.SecretReplicateRequest{
			ReplicaRegion: region, KeyID: keyID, SecretNamespace: "default", Path: "app/db",
			Versions:     []uint64{1},
			QueryOptions: structs.QueryOptions{Region: "region1", AuthToken: token},
		}, &reply)
		return &reply, err
	}

	// The value is encrypted for region2's key only.
	reply, err := replicate(root1.SecretID, "region2", key.ID())
	must.NoError(t, err)
	must.NotNil(t, reply.Secret)
	must.Len(t, 1, reply.Values)
	v := reply.Values[0]
	must.False(t, strings.Contains(string(v.Sealed), secretCanary))
	aad := secretReplicaAAD("region1", "region2", key.ID(), "default", "app/db", 1, v.ValueHash)
	plaintext, err := key.Open(v.Sealed, aad)
	must.NoError(t, err)
	must.StrContains(t, string(plaintext), secretCanary)
	other, err := secretsrepl.GenerateKey()
	must.NoError(t, err)
	_, err = other.Open(v.Sealed, aad)
	must.True(t, errors.Is(err, secretsrepl.ErrDecrypt))

	// A key ID the region does not publish is refused: the request can't
	// choose the key.
	_, err = replicate(root1.SecretID, "region2", other.ID())
	must.ErrorContains(t, err, "replication key of region")

	// Only with a management token, and for another region.
	_, err = replicate("", "region2", key.ID())
	must.ErrorContains(t, err, structs.ErrPermissionDenied.Error())
	_, err = replicate(root1.SecretID, "region1", key.ID())
	must.ErrorContains(t, err, "invalid replica region")
	var list structs.SecretReplicationListResponse
	err = s2.RPC(structs.SecretReplicationListRPCMethod, &structs.SecretReplicationListRequest{
		ReplicaRegion: "region2", QueryOptions: structs.QueryOptions{Region: "region1"},
	}, &list)
	must.ErrorContains(t, err, structs.ErrPermissionDenied.Error())
	must.NoError(t, s2.RPC(structs.SecretReplicationListRPCMethod, &structs.SecretReplicationListRequest{
		ReplicaRegion: "region2", QueryOptions: structs.QueryOptions{Region: "region1", AuthToken: root1.SecretID},
	}, &list))
	must.Len(t, 1, list.Secrets)

	// The key is rotated with region2's keyring; the authoritative region
	// fetches the new public key, and replication goes on.
	var rot structs.KeyringRotateRootKeyResponse
	must.NoError(t, s2.RPC("Keyring.Rotate", &structs.KeyringRotateRootKeyRequest{
		WriteRequest: structs.WriteRequest{Region: "region2", AuthToken: root2.SecretID},
	}, &rot))
	rotated, err := s2.secretReplicationKey()
	must.NoError(t, err)
	must.NotEq(t, key.ID(), rotated.ID())
	reply, err = replicate(root1.SecretID, "region2", rotated.ID())
	must.NoError(t, err)
	_, err = rotated.Open(reply.Values[0].Sealed,
		secretReplicaAAD("region1", "region2", rotated.ID(), "default", "app/db", 1, reply.Values[0].ValueHash))
	must.NoError(t, err)

	repl, req := testReplicator(s2)
	_, err = repl.round(req)
	must.NoError(t, err)
	must.NoError(t, replicaMatches(t, s1, s2, "app/db"))
}
