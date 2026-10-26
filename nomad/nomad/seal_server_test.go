// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/nomad/mock"
	"github.com/hashicorp/nomad/nomad/seal"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/nomad/nomad/structs/config"
	"github.com/hashicorp/nomad/secretstore/ubix/export/storage"
	"github.com/hashicorp/nomad/testutil"
	"github.com/shoenig/test/must"
)

func sharesSealConfig(threshold, shares int) *config.KeyringConfig {
	return &config.KeyringConfig{Seals: []*config.KeyringSealConfig{{
		Type: config.KeyringSealShares, Threshold: threshold, Shares: shares,
	}}}
}

// testSealServer starts a server that keeps its data directory, so it can
// be restarted from it with the same node ID.
func testSealServer(t *testing.T, dir, nodeID string, bootstrapExpect int, keyring *config.KeyringConfig) (*Server, func()) {
	return TestServer(t, func(c *Config) {
		c.DevMode = false
		c.DataDir = dir
		c.NodeName = filepath.Base(dir)
		if nodeID != "" {
			c.NodeID = nodeID
		}
		c.BootstrapExpect = bootstrapExpect
		c.NumSchedulers = 0
		c.KeyringConfig = keyring
	})
}

func waitForSeal(t *testing.T, what string, cond func() bool) {
	t.Helper()
	waitForSecretStore(t, what, cond)
}

func sealRewrap(t *testing.T, s *Server, to string) *structs.KeyringRewrapResponse {
	t.Helper()
	var resp structs.KeyringRewrapResponse
	must.NoError(t, s.RPC("Keyring.Rewrap", &structs.KeyringRewrapRequest{
		To:           to,
		WriteRequest: structs.WriteRequest{Region: s.Region()},
	}, &resp))
	return &resp
}

func sealUnseal(t *testing.T, s *Server, share string) *structs.ServerSealStatus {
	t.Helper()
	var resp structs.SealUnsealResponse
	must.NoError(t, s.RPC("Operator.SealUnseal", &structs.SealUnsealRequest{
		ServerID:     s.config.NodeName,
		Key:          share,
		WriteRequest: structs.WriteRequest{Region: s.Region()},
	}, &resp))
	return resp.Status
}

// waitRewrapComplete waits until no rewrap is in progress and the region
// uses the given seal.
func waitRewrapComplete(t *testing.T, s *Server, sealType string) {
	t.Helper()
	waitForSeal(t, "rewrap to "+sealType, func() bool {
		cfg, err := s.State().SealConfig(nil)
		return err == nil && cfg != nil && cfg.Rewrap == nil &&
			len(cfg.Seals) == 1 && cfg.Seals[0].Type == sealType
	})
}

func writeTestVariable(s *Server, path, value string) error {
	req := &structs.VariablesApplyRequest{
		Op: structs.VarOpSet,
		Var: &structs.VariableDecrypted{
			VariableMetadata: structs.VariableMetadata{Namespace: structs.DefaultNamespace, Path: path},
			Items:            structs.VariableItems{"password": value},
		},
		WriteRequest: structs.WriteRequest{Region: s.Region()},
	}
	return s.RPC(structs.VariablesApplyRPCMethod, req, new(structs.VariablesApplyResponse))
}

func readTestVariable(s *Server, path string, stale bool) (string, error) {
	req := &structs.VariablesReadRequest{
		Path: path,
		QueryOptions: structs.QueryOptions{
			Region: s.Region(), Namespace: structs.DefaultNamespace, AllowStale: stale,
		},
	}
	var resp structs.VariablesReadResponse
	if err := s.RPC(structs.VariablesReadRPCMethod, req, &resp); err != nil {
		return "", err
	}
	if resp.Data == nil {
		return "", errors.New("variable not found")
	}
	return resp.Data.Items["password"], nil
}

// assertKeystoreSealed checks that no key file of a server holds a
// key-encrypting key in the clear, and that a standalone keyring (no seal)
// can't unwrap any of them: a copy of the data directory is useless.
func assertKeystoreSealed(t *testing.T, s *Server) {
	t.Helper()
	dir := filepath.Join(s.config.DataDir, "keystore")
	files, err := filepath.Glob(filepath.Join(dir, "*"+nomadKeystoreExtension))
	must.NoError(t, err)
	must.SliceNotEmpty(t, files)
	for _, f := range files {
		raw, err := os.ReadFile(f)
		must.NoError(t, err)
		var w structs.KeyEncryptionKeyWrapper
		must.NoError(t, json.Unmarshal(raw, &w))
		must.SliceEmpty(t, w.KeyEncryptionKey, must.Sprintf("KEK in the clear in %s", f))
		must.SliceNotEmpty(t, w.SealedKEKs)
		for _, sw := range w.SealedKEKs {
			must.Eq(t, structs.SealTypeShares, sw.Seal)
			must.SliceEmpty(t, sw.KEK)
		}
	}

	copyDir := t.TempDir()
	for _, f := range files {
		raw, err := os.ReadFile(f)
		must.NoError(t, err)
		must.NoError(t, os.WriteFile(filepath.Join(copyDir, filepath.Base(f)), raw, 0o600))
	}
	standalone, err := NewEncrypter(&Server{shutdownCtx: context.Background()}, copyDir)
	must.NoError(t, err)
	must.MapEmpty(t, standalone.keyring)
	must.MapLen(t, len(files), standalone.sealedKeys)
}

// TestSeal_Shares exercises the shares seal of a single server: a new
// region waits for its first rewrap, which generates the shares; after a
// restart the server is sealed and rejects every decryption until a
// threshold of shares is entered, and the secret store core unseals with it.
func TestSeal_Shares(t *testing.T) {
	ci.Parallel(t)
	dir := filepath.Join(t.TempDir(), "seal1")
	keyring := sharesSealConfig(2, 3)

	s1, cleanup1 := testSealServer(t, dir, "", 1, keyring)
	defer cleanup1()
	testutil.WaitForLeader(t, s1.RPC)
	nodeID := s1.config.NodeID

	// A new region with the shares seal is not initialized.
	must.True(t, s1.seal.sealed())
	must.False(t, s1.seal.status().Initialized)
	_, _, err := s1.encrypter.Encrypt([]byte("x"))
	must.True(t, structs.IsErrSealed(err))
	meta, err := s1.State().GetActiveRootKeyMeta(nil)
	must.NoError(t, err)
	must.Nil(t, meta)

	// The first rewrap generates the shares and initializes the keyring.
	resp := sealRewrap(t, s1, structs.SealTypeShares)
	must.Len(t, 3, resp.Shares)
	waitRewrapComplete(t, s1, structs.SealTypeShares)
	waitForSeal(t, "keyring", func() bool { ks, err := s1.encrypter.activeKeySet(); return err == nil && ks != nil })
	must.False(t, s1.seal.sealed())

	must.NoError(t, writeTestVariable(s1, "nomad/jobs/canary", "canary-s3cr3t"))
	claimToken, _, err := s1.encrypter.SignClaims(mock.Alloc().ToTaskIdentityClaims(nil, "web"))
	must.NoError(t, err)

	c, err := s1.secretCore()
	must.NoError(t, err)
	ctx := context.Background()
	must.NoError(t, c.Barrier().Put(ctx, &storage.Entry{Key: "secret/app", Value: []byte("canary-core")}))
	assertSecretStoreCiphertext(t, s1, "canary-core")

	// The key of the core is wrapped by the shares seal only.
	blob, err := s1.State().SecretStoreBlobByPath(nil, seal.CoreMasterKeyPath)
	must.NoError(t, err)
	must.NotNil(t, blob)
	env, err := seal.ParseCoreEnvelope(blob.Value)
	must.NoError(t, err)
	must.Eq(t, []string{"shares:" + resp.Config.Rewrap.To[0].KeyID}, env.Seals())

	cleanup1()
	assertKeystoreSealed(t, s1)

	// Restarted: sealed.
	s2, cleanup2 := testSealServer(t, dir, nodeID, 1, keyring)
	defer cleanup2()
	testutil.WaitForLeader(t, s2.RPC)
	waitForSeal(t, "seal configuration", func() bool {
		cfg, _ := s2.State().SealConfig(nil)
		return cfg != nil
	})
	must.True(t, s2.seal.sealed())

	_, err = readTestVariable(s2, "nomad/jobs/canary", false)
	must.True(t, structs.IsErrSealed(err), must.Sprintf("got %v", err))
	err = writeTestVariable(s2, "nomad/jobs/other", "x")
	must.True(t, structs.IsErrSealed(err), must.Sprintf("got %v", err))
	_, _, err = s2.encrypter.SignClaims(mock.Alloc().ToTaskIdentityClaims(nil, "web"))
	must.ErrorIs(t, err, structs.ErrSealed)
	must.True(t, structs.IsErrSealed(s2.mtls.ready()))
	_, err = s2.backupKeyring()
	must.ErrorIs(t, err, structs.ErrSealed)
	_, err = s2.secretCore()
	must.ErrorIs(t, err, structs.ErrSecretStoreSealed)

	// Workload identities are still verified with the stored public key.
	claims, err := s2.encrypter.VerifyClaim(claimToken)
	must.NoError(t, err)
	must.Eq(t, "web", claims.TaskName)

	// A wrong share combination fails and resets; fewer shares than the
	// threshold do not unseal.
	_, _, otherShares, err := seal.Generate(3, 2)
	must.NoError(t, err)
	st := sealUnseal(t, s2, resp.Shares[0])
	must.True(t, st.Sealed)
	must.Eq(t, 1, st.Progress)
	must.Eq(t, 2, st.Threshold)
	err = s2.RPC("Operator.SealUnseal", &structs.SealUnsealRequest{
		ServerID:     s2.config.NodeName,
		Key:          encodeShare(otherShares[1]),
		WriteRequest: structs.WriteRequest{Region: s2.Region()},
	}, new(structs.SealUnsealResponse))
	must.ErrorContains(t, err, "unseal failed")
	must.True(t, s2.seal.sealed())

	st = sealUnseal(t, s2, resp.Shares[2])
	must.True(t, st.Sealed)
	st = sealUnseal(t, s2, resp.Shares[1])
	must.False(t, st.Sealed)

	value, err := readTestVariable(s2, "nomad/jobs/canary", false)
	must.NoError(t, err)
	must.Eq(t, "canary-s3cr3t", value)
	_, _, err = s2.encrypter.SignClaims(mock.Alloc().ToTaskIdentityClaims(nil, "web"))
	must.NoError(t, err)

	var core2 = s2.secretStoreCore
	waitForSeal(t, "active secret store core", core2.Active)
	c2, err := s2.secretCore()
	must.NoError(t, err)
	e, err := c2.Barrier().Get(ctx, "secret/app")
	must.NoError(t, err)
	must.Eq(t, []byte("canary-core"), e.Value)

	// The seal status reports the server.
	var status structs.SealStatusResponse
	must.NoError(t, s2.RPC("Operator.SealStatus", &structs.SealStatusRequest{
		QueryOptions: structs.QueryOptions{Region: s2.Region()},
	}, &status))
	must.Len(t, 1, status.Servers)
	must.False(t, status.Servers[0].Sealed)
	must.True(t, status.Servers[0].SecretCoreActive)
	must.Eq(t, []string{structs.SealTypeShares}, status.Servers[0].Seals)
	must.Len(t, 0, status.Config.Seals[0].KeyCheck)
}

func encodeShare(b []byte) string {
	return base64.StdEncoding.EncodeToString(b)
}

// TestSeal_RewrapOnline moves a three server region from aead to shares
// under Variable writes and reads, without failed reads; afterwards a
// restarted follower is sealed until it is unsealed with the shares. Then
// the region moves back to aead.
func TestSeal_RewrapOnline(t *testing.T) {
	ci.Parallel(t)
	root := t.TempDir()
	var servers []*Server
	for i := 0; i < 3; i++ {
		s, cleanup := testSealServer(t, filepath.Join(root, fmt.Sprintf("srv%d", i)), "", 3, nil)
		defer cleanup()
		servers = append(servers, s)
	}
	TestJoin(t, servers...)
	for _, s := range servers {
		testutil.WaitForLeader(t, s.RPC)
	}
	for _, s := range servers {
		s := s
		waitForSeal(t, "keyring", func() bool { ks, err := s.encrypter.activeKeySet(); return err == nil && ks != nil })
	}
	leader := func() *Server {
		for _, s := range servers {
			if s.IsLeader() {
				return s
			}
		}
		return nil
	}
	must.NoError(t, writeTestVariable(servers[0], "nomad/jobs/base", "v0"))

	// Write and read Variables during the rewrap.
	stop := make(chan struct{})
	var wg sync.WaitGroup
	var writes, reads atomic.Int64
	var loadErr atomic.Value
	for i, s := range servers {
		wg.Add(1)
		go func(i int, s *Server) {
			defer wg.Done()
			for n := 0; ; n++ {
				select {
				case <-stop:
					return
				default:
				}
				if err := writeTestVariable(s, fmt.Sprintf("nomad/jobs/load%d", i), fmt.Sprint(n)); err != nil {
					loadErr.Store(fmt.Errorf("write on %s: %w", s.config.NodeName, err))
					return
				}
				writes.Add(1)
				if _, err := readTestVariable(s, "nomad/jobs/base", true); err != nil {
					loadErr.Store(fmt.Errorf("read on %s: %w", s.config.NodeName, err))
					return
				}
				reads.Add(1)
			}
		}(i, s)
	}

	resp := sealRewrap(t, leader(), structs.SealTypeShares)
	must.Len(t, 5, resp.Shares)
	for _, s := range servers {
		waitRewrapComplete(t, s, structs.SealTypeShares)
	}
	// every server dropped its aead wraps
	for _, s := range servers {
		s := s
		waitForSeal(t, "aead wraps dropped", func() bool { return s.encrypter.keystoreSharesKeyID() != "" })
	}
	close(stop)
	wg.Wait()
	if err, _ := loadErr.Load().(error); err != nil {
		t.Fatalf("Variables failed during the rewrap: %v", err)
	}
	must.Positive(t, writes.Load())
	must.Positive(t, reads.Load())

	// Restart a follower: sealed until unsealed with the shares.
	var follower *Server
	for _, s := range servers {
		if !s.IsLeader() {
			follower = s
			break
		}
	}
	assertKeystoreSealed(t, follower)
	dir, nodeID := follower.config.DataDir, follower.config.NodeID
	must.NoError(t, follower.Shutdown())
	restarted, cleanup := testSealServer(t, dir, nodeID, 3, nil)
	defer cleanup()
	TestJoin(t, restarted, leader())
	testutil.WaitForLeader(t, restarted.RPC)
	must.True(t, restarted.seal.sealed())
	_, err := readTestVariable(restarted, "nomad/jobs/base", true)
	must.True(t, structs.IsErrSealed(err), must.Sprintf("got %v", err))

	// unseal through another server
	waitForSeal(t, "leader knows the restarted server", func() bool {
		l := leader()
		if l == nil {
			return false
		}
		_, err := l.getServer(l.Region(), restarted.config.NodeID)
		return err == nil
	})
	for _, share := range resp.Shares[:3] {
		var out structs.SealUnsealResponse
		must.NoError(t, leader().RPC("Operator.SealUnseal", &structs.SealUnsealRequest{
			ServerID:     restarted.config.NodeID,
			Key:          share,
			WriteRequest: structs.WriteRequest{Region: "global"},
		}, &out))
	}
	must.False(t, restarted.seal.sealed())
	value, err := readTestVariable(restarted, "nomad/jobs/base", true)
	must.NoError(t, err)
	must.Eq(t, "v0", value)

	// Back to aead: everything unseals automatically again.
	sealRewrap(t, leader(), structs.SealTypeAEAD)
	for _, s := range []*Server{leader(), restarted} {
		waitRewrapComplete(t, s, structs.SealTypeAEAD)
		s := s
		waitForSeal(t, "shares wraps dropped", func() bool { return s.encrypter.keystoreSharesKeyID() == "" })
	}
}

// TestSeal_SecretCoreLeaderChange checks the secret store core on a three
// server aead region: initialized on first use by the leader, unsealed on
// every server, active on the leader only, and working after a change of
// leader.
func TestSeal_SecretCoreLeaderChange(t *testing.T) {
	ci.Parallel(t)
	var servers []*Server
	for i := 0; i < 3; i++ {
		s, cleanup := TestServer(t, func(c *Config) { c.BootstrapExpect = 3; c.NumSchedulers = 0 })
		defer cleanup()
		servers = append(servers, s)
	}
	TestJoin(t, servers...)
	for _, s := range servers {
		testutil.WaitForLeader(t, s.RPC)
	}
	leaderOf := func() *Server {
		for _, s := range servers {
			if s.IsLeader() {
				return s
			}
		}
		return nil
	}
	var leader *Server
	waitForSeal(t, "leader", func() bool { leader = leaderOf(); return leader != nil })

	// Followers: not active, an error wrapping ErrSecretStoreSealed.
	for _, s := range servers {
		if s != leader {
			_, err := s.secretCore()
			must.ErrorIs(t, err, structs.ErrSecretStoreSealed)
		}
	}

	ctx := context.Background()
	c, err := leader.secretCore()
	must.NoError(t, err)
	must.NoError(t, c.Barrier().Put(ctx, &storage.Entry{Key: "secret/app", Value: []byte("canary-1")}))
	for _, s := range servers {
		s := s
		waitForSeal(t, "unsealed core", func() bool { return !s.secretStoreCore.Barrier().Sealed() })
		if s != leader {
			waitForSeal(t, "standby core", s.secretStoreCore.Standby)
		}
	}

	old := leader
	must.NoError(t, old.raft.LeadershipTransfer().Error())
	waitForSeal(t, "new leader", func() bool { leader = leaderOf(); return leader != nil && leader != old })

	var c2 = leader.secretStoreCore
	waitForSeal(t, "active core on the new leader", c2.Active)
	c2, err = leader.secretCore()
	must.NoError(t, err)
	e, err := c2.Barrier().Get(ctx, "secret/app")
	must.NoError(t, err)
	must.Eq(t, []byte("canary-1"), e.Value)
	must.NoError(t, c2.Barrier().Put(ctx, &storage.Entry{Key: "secret/app", Value: []byte("canary-2")}))
	waitForSeal(t, "old leader standby", old.secretStoreCore.Standby)
	_, err = old.secretCore()
	must.ErrorIs(t, err, structs.ErrSecretStoreSealed)

	// Snapshots carry the wrapped core key: it is the aead envelope.
	blob, err := leader.State().SecretStoreBlobByPath(nil, seal.CoreMasterKeyPath)
	must.NoError(t, err)
	env, err := seal.ParseCoreEnvelope(blob.Value)
	must.NoError(t, err)
	must.Eq(t, []string{"aead:"}, env.Seals())
	must.False(t, bytes.Contains(blob.Value, []byte("canary")))
}

// TestSeal_AEADDefault checks that a server without a keyring block keeps
// Nomad's keystore format and writes no seal configuration.
func TestSeal_AEADDefault(t *testing.T) {
	ci.Parallel(t)
	s, cleanup := testSealServer(t, filepath.Join(t.TempDir(), "aead"), "", 1, nil)
	defer cleanup()
	testutil.WaitForLeader(t, s.RPC)
	waitForSeal(t, "keyring", func() bool { ks, err := s.encrypter.activeKeySet(); return err == nil && ks != nil })

	must.False(t, s.seal.sealed())
	cfg, err := s.State().SealConfig(nil)
	must.NoError(t, err)
	must.Nil(t, cfg)

	files, err := filepath.Glob(filepath.Join(s.config.DataDir, "keystore", "*"+nomadKeystoreExtension))
	must.NoError(t, err)
	must.Len(t, 1, files)
	raw, err := os.ReadFile(files[0])
	must.NoError(t, err)
	var w structs.KeyEncryptionKeyWrapper
	must.NoError(t, json.Unmarshal(raw, &w))
	must.Len(t, 32, w.KeyEncryptionKey)
	must.SliceEmpty(t, w.SealedKEKs)

	// A rewrap to the seal in use is refused.
	err = s.RPC("Keyring.Rewrap", &structs.KeyringRewrapRequest{
		To: structs.SealTypeAEAD, WriteRequest: structs.WriteRequest{Region: "global"},
	}, new(structs.KeyringRewrapResponse))
	must.ErrorContains(t, err, "already")
	err = s.RPC("Keyring.Rewrap", &structs.KeyringRewrapRequest{
		To: "awskms", WriteRequest: structs.WriteRequest{Region: "global"},
	}, new(structs.KeyringRewrapResponse))
	must.ErrorContains(t, err, "not implemented")
}
