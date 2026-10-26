// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"filippo.io/age"
	msgpackrpc "github.com/hashicorp/net-rpc-msgpackrpc"
	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/helper/tlsutil"
	"github.com/hashicorp/nomad/nomad/backup"
	"github.com/hashicorp/nomad/nomad/backup/s3fake"
	"github.com/hashicorp/nomad/nomad/seal"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/nomad/nomad/structs/config"
	"github.com/hashicorp/nomad/testutil"
	"github.com/hashicorp/raft"
	"github.com/shoenig/test/must"
	"github.com/shoenig/test/wait"
)

// sealTestCluster is a region of servers that keep their data directories,
// so they can be stopped and restarted.
type sealTestCluster struct {
	t       *testing.T
	root    string
	keyring *config.KeyringConfig
	tls     *config.TLSConfig

	restarts int

	lock     sync.Mutex
	servers  []*Server
	cleanups map[*Server]func()
}

func newSealTestCluster(t *testing.T, n int, keyring *config.KeyringConfig, tls *config.TLSConfig) *sealTestCluster {
	c := &sealTestCluster{t: t, root: t.TempDir(), keyring: keyring, tls: tls, cleanups: map[*Server]func(){}}
	for i := 0; i < n; i++ {
		c.start(filepath.Join(c.root, fmt.Sprintf("srv%d", i)), "", "")
	}
	TestJoin(t, c.servers...)
	for _, s := range c.servers {
		testutil.WaitForLeader(t, s.RPC)
	}
	t.Cleanup(c.stopAll)
	return c
}

func (c *sealTestCluster) start(dir, nodeID, name string) *Server {
	s, cleanup := TestServer(c.t, func(cfg *Config) {
		cfg.DevMode = false
		cfg.DataDir = dir
		cfg.NodeName = filepath.Base(dir)
		if name != "" {
			cfg.NodeName = name
		}
		if nodeID != "" {
			cfg.NodeID = nodeID
		}
		cfg.BootstrapExpect = 3
		cfg.NumSchedulers = 0
		cfg.KeyringConfig = c.keyring
		if c.tls != nil {
			cfg.TLSConfig = c.tls.Copy()
		}
	})
	c.lock.Lock()
	c.servers = append(c.servers, s)
	c.cleanups[s] = cleanup
	c.lock.Unlock()
	return s
}

// stop shuts a server down, keeping its data directory.
func (c *sealTestCluster) stop(s *Server) (string, string) {
	dir, nodeID := s.config.DataDir, s.config.NodeID
	c.lock.Lock()
	c.servers = slices.DeleteFunc(c.servers, func(o *Server) bool { return o == s })
	cleanup := c.cleanups[s]
	delete(c.cleanups, s)
	c.lock.Unlock()
	cleanup()
	return dir, nodeID
}

// restart starts a stopped server again from its data directory, under a
// new Serf name (the old one is still known as failed).
func (c *sealTestCluster) restart(dir, nodeID string) *Server {
	c.restarts++
	s := c.start(dir, nodeID, fmt.Sprintf("%s-r%d", filepath.Base(dir), c.restarts))
	TestJoin(c.t, s, c.leader())
	testutil.WaitForLeader(c.t, s.RPC)
	return s
}

func (c *sealTestCluster) stopAll() {
	c.lock.Lock()
	cleanups := c.cleanups
	c.cleanups = map[*Server]func(){}
	c.lock.Unlock()
	for _, cleanup := range cleanups {
		cleanup()
	}
}

func (c *sealTestCluster) leader() *Server {
	var leader *Server
	waitForSeal(c.t, "leader", func() bool {
		c.lock.Lock()
		defer c.lock.Unlock()
		for _, s := range c.servers {
			if s.IsLeader() {
				leader = s
				return true
			}
		}
		return false
	})
	return leader
}

// waitStable waits until every server follows the same leader, which has
// every server in its Raft configuration as a voter.
func (c *sealTestCluster) waitStable() {
	waitLong(c.t, "stable cluster", func() bool {
		c.lock.Lock()
		servers := slices.Clone(c.servers)
		c.lock.Unlock()
		var leaderAddr string
		for _, s := range servers {
			addr, _ := s.raft.LeaderWithID()
			if addr == "" || (leaderAddr != "" && string(addr) != leaderAddr) {
				return false
			}
			leaderAddr = string(addr)
			if s.IsLeader() {
				future := s.raft.GetConfiguration()
				if future.Error() != nil || len(future.Configuration().Servers) != len(servers) {
					return false
				}
				for _, srv := range future.Configuration().Servers {
					if srv.Suffrage != raft.Voter {
						return false
					}
				}
			}
		}
		return true
	})
}

func (c *sealTestCluster) followers() []*Server {
	leader := c.leader()
	c.lock.Lock()
	defer c.lock.Unlock()
	var out []*Server
	for _, s := range c.servers {
		if s != leader {
			out = append(out, s)
		}
	}
	return out
}

// unsealVia enters shares for a server through another server.
func unsealVia(t *testing.T, via, target *Server, shares []string) {
	t.Helper()
	waitForSeal(t, "server known to "+via.config.NodeName, func() bool {
		_, err := via.findServer(target.config.NodeName)
		return err == nil
	})
	for _, share := range shares {
		var out structs.SealUnsealResponse
		must.NoError(t, via.RPC("Operator.SealUnseal", &structs.SealUnsealRequest{
			ServerID:     target.config.NodeName,
			Key:          share,
			WriteRequest: structs.WriteRequest{Region: "global"},
		}, &out))
	}
	must.False(t, target.seal.sealed())
}

func waitForKeyOn(t *testing.T, s *Server, keyID string) {
	t.Helper()
	waitForSeal(t, "key "+keyID+" on "+s.config.NodeName, func() bool {
		key, err := s.encrypter.GetKey(keyID)
		return err == nil && len(key) > 0
	})
}

func waitLong(t *testing.T, what string, cond func() bool) {
	t.Helper()
	must.Wait(t, wait.InitialSuccess(
		wait.BoolFunc(cond),
		wait.Timeout(90*time.Second),
		wait.Gap(100*time.Millisecond),
	), must.Sprintf("waiting for %s", what))
}

func activeKeyID(t *testing.T, s *Server) string {
	t.Helper()
	meta, err := s.State().GetActiveRootKeyMeta(nil)
	must.NoError(t, err)
	must.NotNil(t, meta)
	return meta.KeyID
}

// newTestServerCert returns the TLS configuration of servers of the global
// region signed by a new CA, and a function that issues another certificate
// with the same server identity (a "stolen" one).
func newTestServerCert(t *testing.T) (*config.TLSConfig, func() *config.TLSConfig) {
	h := tlsTestHelper{dir: t.TempDir(), nodeID: 1}
	var err error
	h.caPEM, h.pk, err = tlsutil.GenerateCA(tlsutil.CAOpts{
		Name: "Norca test CA", Country: "ZZ", Days: 5, Organization: "Norca", OrganizationalUnit: "Norca",
	})
	must.NoError(t, err)
	must.NoError(t, os.WriteFile(filepath.Join(h.dir, "ca.pem"), []byte(h.caPEM), 0o600))
	issue := func() *config.TLSConfig {
		h.nodeID++
		path := h.newCert(t, "server.global.nomad")
		return &config.TLSConfig{
			EnableRPC:            true,
			VerifyServerHostname: true,
			CAFile:               filepath.Join(h.dir, "ca.pem"),
			CertFile:             path + ".pem",
			KeyFile:              path + ".key",
		}
	}
	return issue(), issue
}

// assertNoUsableKey checks that a Keyring.Get response carries a root key
// only wrapped by a shares seal: no key material, no key-encrypting key in
// the clear, nothing a standalone keyring (no seal) can unwrap.
func assertNoUsableKey(t *testing.T, resp *structs.KeyringGetRootKeyResponse, raw []byte) {
	t.Helper()
	must.Nil(t, resp.Key)
	must.NotNil(t, resp.WrappedKey)
	must.SliceEmpty(t, resp.WrappedKey.KeyEncryptionKey)
	must.SliceNotEmpty(t, resp.WrappedKey.SealedKEKs)
	for _, w := range resp.WrappedKey.SealedKEKs {
		must.Eq(t, structs.SealTypeShares, w.Seal)
		must.SliceEmpty(t, w.KEK)
	}
	buf, err := json.Marshal(resp)
	must.NoError(t, err)
	must.False(t, bytes.Contains(buf, raw), must.Sprint("the root key is in the response"))

	standalone := &Encrypter{keyring: map[string]*keyset{}, sealedKeys: map[string]*structs.KeyEncryptionKeyWrapper{}}
	_, err = standalone.unwrapKeyFile(resp.WrappedKey)
	must.True(t, structs.IsErrSealed(err), must.Sprintf("got %v", err))
}

// TestSealHardening_SharesCluster checks the key distribution of a three
// server region with the shares seal: the first rewrap initializes it on
// the leader without waiting for the sealed followers, every follower is
// unsealed by an operator, keys replicate to unsealed servers only in
// wrapped form, a client with a valid server certificate gets no usable key
// from any server, and a restarted server has no key until it is unsealed.
func TestSealHardening_SharesCluster(t *testing.T) {
	ci.Parallel(t)
	serverTLS, stolen := newTestServerCert(t)
	c := newSealTestCluster(t, 3, sharesSealConfig(2, 3), serverTLS)
	leader := c.leader()

	resp := sealRewrap(t, leader, structs.SealTypeShares)
	must.Len(t, 3, resp.Shares)
	// The sealed followers don't hold the first rewrap up.
	waitRewrapComplete(t, leader, structs.SealTypeShares)
	cfg, err := leader.State().SealConfig(nil)
	must.NoError(t, err)
	must.Eq(t, resp.RewrapID, cfg.LastRewrapID)
	waitForKeyring(t, leader)
	for _, f := range c.followers() {
		must.True(t, f.seal.sealed())
		_, err := f.encrypter.GetKey(activeKeyID(t, leader))
		must.Error(t, err)
	}

	// Unsealed by an operator, every follower replicates the root key.
	for _, f := range c.followers() {
		unsealVia(t, leader, f, resp.Shares[:2])
		waitForKeyOn(t, f, activeKeyID(t, leader))
	}

	// A rotated key replicates, wrapped, to the unsealed followers.
	var rotate structs.KeyringRotateRootKeyResponse
	must.NoError(t, leader.RPC("Keyring.Rotate", &structs.KeyringRotateRootKeyRequest{
		WriteRequest: structs.WriteRequest{Region: "global"},
	}, &rotate))
	newKey := rotate.Key.KeyID
	for _, f := range c.followers() {
		waitForKeyOn(t, f, newKey)
	}
	raw, err := leader.encrypter.GetKey(newKey)
	must.NoError(t, err)
	must.NoError(t, writeTestVariable(c.followers()[0], "nomad/jobs/canary", "canary-v1"))
	for _, s := range []*Server{leader, c.followers()[0], c.followers()[1]} {
		waitForSeal(t, "variable on "+s.config.NodeName, func() bool {
			v, err := readTestVariable(s, "nomad/jobs/canary", true)
			return err == nil && v == "canary-v1"
		})
	}

	// A client with a valid server certificate.
	stolenTLS := stolen()
	for _, s := range append([]*Server{leader}, c.followers()...) {
		codec := rpcClientWithTLS(t, s, stolenTLS)
		for _, stale := range []bool{false, true} {
			var getResp structs.KeyringGetRootKeyResponse
			must.NoError(t, msgpackrpc.CallWithCodec(codec, "Keyring.Get", &structs.KeyringGetRootKeyRequest{
				KeyID:        newKey,
				QueryOptions: structs.QueryOptions{Region: "global", AllowStale: stale},
			}, &getResp))
			assertNoUsableKey(t, &getResp, raw)
		}
		var status structs.SealStatusResponse
		must.NoError(t, msgpackrpc.CallWithCodec(codec, "Operator.SealStatus", &structs.SealStatusRequest{
			QueryOptions: structs.QueryOptions{Region: "global"},
		}, &status))
		buf, err := json.Marshal(status)
		must.NoError(t, err)
		must.False(t, bytes.Contains(buf, raw))
		codec.Close()
	}

	// A restarted follower has no key until an operator unseals it.
	follower := c.followers()[0]
	dir, nodeID := c.stop(follower)
	restarted := c.restart(dir, nodeID)
	must.True(t, restarted.seal.sealed())
	_, err = restarted.encrypter.GetKey(newKey)
	must.Error(t, err)
	// once it has caught up with the state, reads are refused as sealed
	waitForSeal(t, "sealed variable read", func() bool {
		_, err = readTestVariable(restarted, "nomad/jobs/canary", true)
		return structs.IsErrSealed(err)
	})
	codec := rpcClientWithTLS(t, restarted, stolenTLS)
	var getResp structs.KeyringGetRootKeyResponse
	err = msgpackrpc.CallWithCodec(codec, "Keyring.Get", &structs.KeyringGetRootKeyRequest{
		KeyID: newKey, QueryOptions: structs.QueryOptions{Region: "global", AllowStale: true},
	}, &getResp)
	must.True(t, structs.IsErrSealed(err), must.Sprintf("got %v", err))
	must.Nil(t, getResp.Key)
	must.Nil(t, getResp.WrappedKey)
	codec.Close()
	assertKeystoreSealed(t, restarted)

	unsealVia(t, c.leader(), restarted, resp.Shares[1:])
	value, err := readTestVariable(restarted, "nomad/jobs/canary", true)
	must.NoError(t, err)
	must.Eq(t, "canary-v1", value)
}

// runVariableLoad writes and reads Variables on servers until stopped and
// returns the first error.
func runVariableLoad(t *testing.T, servers []*Server) (stop func() error) {
	waitForSeal(t, "base variable written", func() bool {
		return writeTestVariable(servers[0], "nomad/jobs/base", "v0") == nil
	})
	for _, s := range servers {
		s := s
		waitForSeal(t, "base variable", func() bool { _, err := readTestVariable(s, "nomad/jobs/base", true); return err == nil })
	}
	done := make(chan struct{})
	var wg sync.WaitGroup
	var writes atomic.Int64
	var loadErr atomic.Value
	for i, s := range servers {
		wg.Add(1)
		go func(i int, s *Server) {
			defer wg.Done()
			for n := 0; ; n++ {
				select {
				case <-done:
					return
				default:
				}
				if err := writeTestVariable(s, fmt.Sprintf("nomad/jobs/load%d", i), fmt.Sprint(n)); err != nil {
					loadErr.Store(fmt.Errorf("write on %s: %w", s.config.NodeName, err))
					return
				}
				if _, err := readTestVariable(s, "nomad/jobs/base", true); err != nil {
					loadErr.Store(fmt.Errorf("read on %s: %w", s.config.NodeName, err))
					return
				}
				writes.Add(1)
				time.Sleep(5 * time.Millisecond)
			}
		}(i, s)
	}
	return func() error {
		close(done)
		wg.Wait()
		if err, _ := loadErr.Load().(error); err != nil {
			return err
		}
		if writes.Load() == 0 {
			return fmt.Errorf("no Variables were written")
		}
		return nil
	}
}

// TestSealHardening_RewrapServerDown moves a three server region from aead
// to shares, then to new shares, each time online under Variable load with
// a follower down: the rewrap completes without it, the follower is sealed
// when it returns, the old shares no longer unseal it, the new shares do,
// and it gets its keys back.
func TestSealHardening_RewrapServerDown(t *testing.T) {
	ci.Parallel(t)
	c := newSealTestCluster(t, 3, nil, nil)
	for _, s := range c.servers {
		waitForKeyring(t, s)
	}

	// aead -> shares with a follower down.
	c.waitStable()
	down := c.followers()[0]
	downDir, downID := c.stop(down)
	stop := runVariableLoad(t, append([]*Server{c.leader()}, c.followers()...))
	resp1 := sealRewrap(t, c.leader(), structs.SealTypeShares)
	waitLong(t, "rewrap to shares", func() bool {
		cfg, err := c.leader().State().SealConfig(nil)
		return err == nil && cfg != nil && cfg.Rewrap == nil && cfg.LastRewrapID == resp1.RewrapID
	})
	must.NoError(t, stop())
	cfg, err := c.leader().State().SealConfig(nil)
	must.NoError(t, err)
	must.Eq(t, structs.SealTypeShares, cfg.Seals[0].Type)
	for _, s := range c.servers {
		s := s
		waitForSeal(t, "aead wraps dropped", func() bool { return s.encrypter.keystoreSharesKeyID() != "" })
		assertKeystoreSealed(t, s)
	}

	returned := c.restart(downDir, downID)
	waitForSeal(t, "seal configuration on the returned server", func() bool {
		cfg, _ := returned.State().SealConfig(nil)
		return cfg != nil && cfg.Rewrap == nil && len(cfg.Seals) == 1 && cfg.Seals[0].Type == structs.SealTypeShares
	})
	// While it catches up, the returned server may apply the rewrap record
	// of the log and unwrap the new key with aead (which protected nothing
	// before); otherwise it is unsealed with the new shares.
	if returned.seal.sealed() {
		unsealVia(t, c.leader(), returned, resp1.Shares[:3])
	}
	waitForSeal(t, "returned server wraps its keystore with the shares", func() bool {
		return returned.encrypter.keystoreSharesKeyID() != ""
	})
	assertKeystoreSealed(t, returned)
	value, err := readTestVariable(returned, "nomad/jobs/base", true)
	must.NoError(t, err)
	must.Eq(t, "v0", value)

	// shares -> new shares with another follower down.
	var running *Server
	for _, f := range c.followers() {
		if f != returned {
			down = f
		} else {
			running = f
		}
	}
	c.waitStable()
	downDir, downID = c.stop(down)
	stop = runVariableLoad(t, []*Server{c.leader(), running})
	resp2 := sealRewrap(t, c.leader(), structs.SealTypeShares)
	newKeyID := resp2.Config.Rewrap.To[0].KeyID
	waitLong(t, "rewrap to new shares", func() bool {
		cfg, err := c.leader().State().SealConfig(nil)
		return err == nil && cfg != nil && cfg.Rewrap == nil && cfg.LastRewrapID == resp2.RewrapID
	})
	must.NoError(t, stop())
	for _, s := range []*Server{c.leader(), running} {
		s := s
		waitForSeal(t, "old unseal key dropped on "+s.config.NodeName, func() bool {
			return slices.Equal(s.seal.heldSharesKeys(), []string{newKeyID})
		})
		must.False(t, s.seal.sealed())
	}
	// the key of the core moved to the new shares
	env := c.leader().secretCoreEnvelope()
	if env != nil {
		must.Eq(t, []string{"shares:" + newKeyID}, env.Seals())
	}

	returned = c.restart(downDir, downID)
	waitForSeal(t, "new seal configuration on the returned server", func() bool {
		cfg, _ := returned.State().SealConfig(nil)
		return cfg != nil && cfg.LastRewrapID == resp2.RewrapID
	})
	must.True(t, returned.seal.sealed())
	// the old shares stop working
	for _, share := range resp1.Shares[:3] {
		err = c.leader().RPC("Operator.SealUnseal", &structs.SealUnsealRequest{
			ServerID: returned.config.NodeName, Key: share, WriteRequest: structs.WriteRequest{Region: "global"},
		}, new(structs.SealUnsealResponse))
		if err != nil {
			break
		}
	}
	must.ErrorContains(t, err, "unseal failed")
	must.True(t, returned.seal.sealed())
	unsealVia(t, c.leader(), returned, resp2.Shares[:3])
	for _, key := range []string{activeKeyID(t, c.leader())} {
		waitForKeyOn(t, returned, key)
	}
	value, err = readTestVariable(returned, "nomad/jobs/base", true)
	must.NoError(t, err)
	must.Eq(t, "v0", value)
	waitForSeal(t, "returned server rewrote its keystore with the new shares", func() bool {
		return returned.encrypter.keystoreSharesKeyID() == newKeyID && !returned.encrypter.keystoreUsesSeal(structs.SealTypeShares, cfg.Seals[0].KeyID)
	})
}

// restoreSealTest is a single ACL server with the shares seal and backups
// on a fake S3 store.
type restoreSealTest struct {
	*fullRestoreTest
	shares1 []string
}

func newRestoreSealTest(t *testing.T) (*restoreSealTest, func()) {
	s3 := s3fake.New()
	identity, err := age.GenerateX25519Identity()
	must.NoError(t, err)
	srv, root, cleanup := TestACLServer(t, func(c *Config) {
		c.DevMode = false
		c.DataDir = t.TempDir()
		c.NumSchedulers = 0
		c.KeyringConfig = sharesSealConfig(2, 3)
		c.Backup = testBackupConfig(s3, identity, false)
	})
	testutil.WaitForLeader(t, srv.RPC)
	ft := &fullRestoreTest{t: t, srv: srv, root: root, identity: identity}

	var resp structs.KeyringRewrapResponse
	must.NoError(t, ft.call("Keyring.Rewrap", &structs.KeyringRewrapRequest{
		To: structs.SealTypeShares, WriteRequest: ft.write(root),
	}, &resp))
	waitRewrapComplete(t, srv, structs.SealTypeShares)
	waitForKeyring(t, srv)
	waitForSecretCore(t, srv)

	var tokenResp structs.ACLTokenUpsertResponse
	must.NoError(t, ft.call(structs.ACLUpsertTokensRPCMethod, &structs.ACLTokenUpsertRequest{
		Tokens:       []*structs.ACLToken{{Name: "second admin", Type: structs.ACLManagementToken}},
		WriteRequest: ft.write(root),
	}, &tokenResp))
	ft.second = tokenResp.Tokens[0]
	return &restoreSealTest{fullRestoreTest: ft, shares1: resp.Shares}, func() { cleanup(); s3.Close() }
}

func (rt *restoreSealTest) putSecret(path, value string) {
	rt.t.Helper()
	var w structs.SecretWriteResponse
	must.NoError(rt.t, rt.call(structs.SecretUpsertRPCMethod,
		secretWriteReq(path, rt.root.SecretID, structs.SecretItems{"k": value}), &w))
}

func (rt *restoreSealTest) readSecret(path string) (string, error) {
	var rr structs.SecretReadResponse
	err := rt.call(structs.SecretReadRPCMethod,
		&structs.SecretReadRequest{Path: path, QueryOptions: secretQuery(rt.root.SecretID)}, &rr)
	return rr.Items["k"], err
}

// rekey moves the region to new shares and returns them.
func (rt *restoreSealTest) rekey() []string {
	rt.t.Helper()
	var resp structs.KeyringRewrapResponse
	must.NoError(rt.t, rt.call("Keyring.Rewrap", &structs.KeyringRewrapRequest{
		To: structs.SealTypeShares, WriteRequest: rt.write(rt.root),
	}, &resp))
	waitForSeal(rt.t, "rekey", func() bool {
		cfg, err := rt.srv.State().SealConfig(nil)
		return err == nil && cfg != nil && cfg.Rewrap == nil && cfg.LastRewrapID == resp.RewrapID
	})
	return resp.Shares
}

// fullRestore restores a backup and returns its preview warnings.
func (rt *restoreSealTest) fullRestore(backupID string) []string {
	rt.t.Helper()
	req, err := rt.request(backupID, rt.root, "global", false)
	must.NoError(rt.t, err)
	approved, err := rt.approve(req.Restore.ID, rt.second, rt.identity.String())
	must.NoError(rt.t, err)
	must.Eq(rt.t, structs.BackupRestoreStatusComplete, approved.Restore.Status, must.Sprint(approved.Restore.StatusDescription))
	return req.Preview.Warnings
}

// selectiveRestore restores a secret from a backup and returns the results.
func (rt *restoreSealTest) selectiveRestore(backupID, path string) ([]*structs.BackupRestoreItemResult, error) {
	var resp structs.BackupRestoreResponse
	err := rt.call("Backup.Restore", &structs.BackupRestoreRequest{
		ID: backupID, Mode: structs.BackupRestoreModeSelective, AgeIdentity: rt.identity.String(),
		Items:        []*structs.BackupRestoreItem{secretItem(path)},
		WriteRequest: rt.write(rt.root),
	}, &resp)
	if err == nil && len(resp.Results) != 1 {
		return nil, fmt.Errorf("expected one result, got %d", len(resp.Results))
	}
	return resp.Results, err
}

func warningWith(warnings []string, text string) bool {
	return slices.ContainsFunc(warnings, func(w string) bool { return bytes.Contains([]byte(w), []byte(text)) })
}

// TestSealHardening_RestoreWithAgeKey restores a backup whose secret store
// is wrapped by shares the region no longer has: the secret store key in
// the backup re-wraps it with the current shares, and the secrets are
// readable.
func TestSealHardening_RestoreWithAgeKey(t *testing.T) {
	ci.Parallel(t)
	rt, cleanup := newRestoreSealTest(t)
	defer cleanup()

	rt.putSecret("app/db", "canary-before")
	record := rt.save()
	must.MapContainsKey(t, record.Destinations[0].Objects, structs.BackupObjectSecretStoreKey)
	shares2 := rt.rekey()
	must.Len(t, 3, shares2)
	rt.putSecret("app/db", "canary-after")

	preview, err := rt.request(record.ID, rt.root, "", true)
	must.NoError(t, err)
	must.True(t, warningWith(preview.Preview.Warnings, "re-wraps its key"), must.Sprint(preview.Preview.Warnings))

	rt.fullRestore(record.ID)
	waitForSeal(t, "restored secret", func() bool {
		v, err := rt.readSecret("app/db")
		return err == nil && v == "canary-before"
	})
	must.False(t, rt.srv.secretCoreNeedsRecovery())
	cfg, err := rt.srv.State().SealConfig(nil)
	must.NoError(t, err)
	must.Eq(t, []string{"shares:" + cfg.Seals[0].KeyID}, rt.srv.secretCoreEnvelope().Seals())
	must.False(t, rt.srv.seal.status().SecretCoreRecovery)
}

// TestSealHardening_RestoreWithOldShares restores a backup without a secret
// store key object (as taken before Norca wrote it) whose secret store is
// wrapped by old shares: the store stays sealed with a clear status until
// the old shares are entered, then the secrets are readable.
func TestSealHardening_RestoreWithOldShares(t *testing.T) {
	ci.Parallel(t)
	rt, cleanup := newRestoreSealTest(t)
	defer cleanup()

	rt.putSecret("app/db", "canary-before")
	record := rt.save()

	// The backup of an older Norca: no secret store key object.
	dest, st, err := rt.srv.backup.ReadDestination(record, "")
	must.NoError(t, err)
	ids, err := backup.ParseIdentities(rt.identity.String())
	must.NoError(t, err)
	coreKey, err := backup.ReadSecretStoreKey(context.Background(), dest, record, st, ids)
	must.NoError(t, err)
	must.Len(t, seal.KeySize, coreKey)
	old := record.Copy()
	delete(old.Destinations[0].Objects, structs.BackupObjectSecretStoreKey)
	_, _, err = rt.srv.raftApply(structs.BackupUpsertRequestType|structs.IgnoreUnknownTypeFlag,
		&structs.BackupUpsertRequest{Backups: []*structs.BackupRecord{old}})
	must.NoError(t, err)

	shares2 := rt.rekey()
	rt.putSecret("app/db", "canary-after")

	warnings := rt.fullRestore(record.ID)
	must.True(t, warningWith(warnings, "operator unseal -recover"), must.Sprint(warnings))

	// Sealed, with a clear status.
	_, err = rt.readSecret("app/db")
	must.ErrorContains(t, err, "recover")
	results, err := rt.selectiveRestore(record.ID, "app/db")
	if err == nil {
		must.StrContains(t, results[0].Error, "recover")
	} else {
		must.ErrorContains(t, err, "recover")
	}
	st2 := rt.srv.seal.status()
	must.True(t, st2.SecretCoreRecovery)
	must.Eq(t, []string{"shares:" + record.Seals[0].KeyID}, st2.SecretCoreSeals)

	recoverReq := func(r *structs.SealRecoverRequest) (*structs.SealRecoverResponse, error) {
		r.WriteRequest = rt.write(rt.root)
		var resp structs.SealRecoverResponse
		err := rt.call("Operator.SealRecover", r, &resp)
		return &resp, err
	}
	// A wrong key is refused; a share of the current seal does not help.
	_, err = recoverReq(&structs.SealRecoverRequest{CoreKey: bytes.Repeat([]byte{7}, seal.KeySize)})
	must.ErrorContains(t, err, "does not unlock")
	resp, err := recoverReq(&structs.SealRecoverRequest{Key: shares2[0]})
	must.NoError(t, err)
	must.False(t, resp.Recovered)
	must.Eq(t, 1, resp.Progress)
	_, err = recoverReq(&structs.SealRecoverRequest{Reset: true})
	must.NoError(t, err)

	resp, err = recoverReq(&structs.SealRecoverRequest{Key: rt.shares1[2]})
	must.NoError(t, err)
	must.False(t, resp.Recovered)
	must.Eq(t, 1, rt.srv.seal.status().RecoveryProgress)
	resp, err = recoverReq(&structs.SealRecoverRequest{Key: rt.shares1[0]})
	must.NoError(t, err)
	must.True(t, resp.Recovered)
	cfg, err := rt.srv.State().SealConfig(nil)
	must.NoError(t, err)
	must.Eq(t, []string{"shares:" + cfg.Seals[0].KeyID}, resp.Seals)

	waitForSeal(t, "recovered secret", func() bool {
		v, err := rt.readSecret("app/db")
		return err == nil && v == "canary-before"
	})
	must.False(t, rt.srv.seal.status().SecretCoreRecovery)

	// The selective restore of a secret from the backup works again.
	rt.putSecret("app/db", "canary-changed")
	results, err = rt.selectiveRestore(record.ID, "app/db")
	must.NoError(t, err)
	must.Eq(t, "", results[0].Error)
	v, err := rt.readSecret("app/db")
	must.NoError(t, err)
	must.Eq(t, "canary-before", v)

	// Nothing left to recover.
	resp, err = recoverReq(&structs.SealRecoverRequest{CoreKey: coreKey})
	must.NoError(t, err)
	must.True(t, resp.Recovered)
}
