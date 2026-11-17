// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	msgpackrpc "github.com/hashicorp/net-rpc-msgpackrpc"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/bacanoo-io/norca/norca/testutil"
	"github.com/shoenig/test/must"
)

// A rewrap report is the word of the server it names (review finding
// SRV-4). Over a connection only the leader takes one, and never about
// itself; a follower takes none, because it would forward it as its own;
// no token, a management token included, stands for a server.
func TestSealRewrapDone_Reporter(t *testing.T) {
	ci.Parallel(t)
	root := t.TempDir()
	var servers []*Server
	for i := 0; i < 3; i++ {
		s, cleanup := TestServer(t, func(c *Config) {
			c.DevMode = false
			c.DataDir = filepath.Join(root, fmt.Sprintf("srv%d", i))
			c.NodeName = fmt.Sprintf("srv%d", i)
			c.BootstrapExpect = 3
			c.NumSchedulers = 0
			c.ACLEnabled = true
		})
		defer cleanup()
		servers = append(servers, s)
	}
	TestJoin(t, servers...)
	for _, s := range servers {
		testutil.WaitForLeader(t, s.RPC)
	}
	var leader, follower *Server
	for _, s := range servers {
		if s.IsLeader() {
			leader = s
		} else {
			follower = s
		}
	}
	must.NotNil(t, leader)
	must.NotNil(t, follower)
	var bootstrap structs.ACLTokenUpsertResponse
	must.NoError(t, leader.RPC("ACL.Bootstrap", &structs.ACLTokenBootstrapRequest{
		WriteRequest: structs.WriteRequest{Region: leader.Region()},
	}, &bootstrap))
	must.Len(t, 1, bootstrap.Tokens)
	rootToken := bootstrap.Tokens[0]

	report := func(serverID, token string) *structs.SealRewrapDoneRequest {
		return &structs.SealRewrapDoneRequest{
			RewrapID:     "no-such-rewrap",
			ServerID:     serverID,
			WriteRequest: structs.WriteRequest{Region: leader.Region(), AuthToken: token},
		}
	}
	leaderID := leader.seal.localID()
	followerID := follower.seal.localID()

	cases := []struct {
		name     string
		to       *Server
		serverID string
		token    string
	}{
		{"anonymous about the leader, to the leader", leader, leaderID, ""},
		{"management token about the leader, to the leader", leader, leaderID, rootToken.SecretID},
		{"anonymous about the leader, through a follower", follower, leaderID, ""},
		{"management token about the leader, through a follower", follower, leaderID, rootToken.SecretID},
		{"anonymous about a follower, through that follower", follower, followerID, ""},
		{"management token about a follower, through that follower", follower, followerID, rootToken.SecretID},
		{"about a server that is none", leader, "no-such-server", rootToken.SecretID},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			codec := rpcClient(t, tc.to)
			err := msgpackrpc.CallWithCodec(codec, "Operator.SealRewrapDone",
				report(tc.serverID, tc.token), new(structs.GenericResponse))
			must.ErrorContains(t, err, structs.ErrPermissionDenied.Error())
		})
	}

	// From another region nothing is taken either.
	t.Run("another region", func(t *testing.T) {
		req := report(followerID, rootToken.SecretID)
		req.Region = "elsewhere"
		err := msgpackrpc.CallWithCodec(rpcClient(t, leader), "Operator.SealRewrapDone", req, new(structs.GenericResponse))
		must.ErrorContains(t, err, structs.ErrPermissionDenied.Error())
	})

	// A report about a follower that reaches the leader directly: the
	// leader asks the follower, which did nothing of the kind. Neither done
	// nor sealed is taken, with no token and with a management token; nor is
	// the follower's own report while it is not true.
	for _, token := range []string{"", rootToken.SecretID} {
		for _, sealed := range []bool{false, true} {
			req := report(followerID, token)
			req.Sealed = sealed
			err := msgpackrpc.CallWithCodec(rpcClient(t, leader), "Operator.SealRewrapDone", req, new(structs.GenericResponse))
			must.ErrorContains(t, err, structs.ErrPermissionDenied.Error(),
				must.Sprintf("a forged report (sealed %t) about a follower was taken", sealed))
		}
	}
	err := follower.RPC("Operator.SealRewrapDone", report(followerID, ""), new(structs.GenericResponse))
	must.ErrorContains(t, err, structs.ErrPermissionDenied.Error())

	// The servers' own reports work: each in process, a follower's
	// forwarded to the leader, which the follower confirms once it has
	// wrapped its keys for that rewrap. In process a server speaks for
	// itself only.
	follower.seal.lock.Lock()
	follower.seal.rewrapped = "no-such-rewrap"
	follower.seal.lock.Unlock()
	for _, s := range []*Server{leader, follower} {
		must.NoError(t, s.RPC("Operator.SealRewrapDone", report(s.seal.localID(), ""), new(structs.GenericResponse)))
	}
	// It confirms what is true and nothing else: not that it is sealed.
	sealedReport := report(followerID, "")
	sealedReport.Sealed = true
	err = msgpackrpc.CallWithCodec(rpcClient(t, leader), "Operator.SealRewrapDone", sealedReport, new(structs.GenericResponse))
	must.ErrorContains(t, err, structs.ErrPermissionDenied.Error())
	follower.seal.lock.Lock()
	follower.seal.rewrapped, follower.seal.cantRewrap = "", "no-such-rewrap"
	follower.seal.lock.Unlock()
	must.NoError(t, follower.RPC("Operator.SealRewrapDone", sealedReport, new(structs.GenericResponse)))

	err = follower.RPC("Operator.SealRewrapDone", report(leaderID, ""), new(structs.GenericResponse))
	must.ErrorContains(t, err, "for itself only")

	// The question itself is answered by the server it is about only.
	err = msgpackrpc.CallWithCodec(rpcClient(t, leader), "Operator.SealRewrapConfirm", sealedReport, new(structs.GenericResponse))
	must.ErrorContains(t, err, structs.ErrPermissionDenied.Error())
}

// A server does not take the word of the rewrap record about itself (review
// finding SRV-4): a sealed leader someone reported as done neither skips
// wrapping its keys nor completes the rewrap, which would move the region to
// a seal that wraps none of its keys.
func TestSealRewrap_ForgedDoneOfLeader(t *testing.T) {
	ci.Parallel(t)
	dir := filepath.Join(t.TempDir(), "seal1")
	keyring := sharesSealConfig(2, 3)

	s1, cleanup1 := testSealServer(t, dir, "", 1, keyring)
	defer cleanup1()
	testutil.WaitForLeader(t, s1.RPC)
	nodeID := s1.config.NodeID
	resp := sealRewrap(t, s1, structs.SealTypeShares)
	waitRewrapComplete(t, s1, structs.SealTypeShares)
	waitForSeal(t, "keyring", func() bool { ks, err := s1.encrypter.activeKeySet(); return err == nil && ks != nil })
	must.NoError(t, writeTestVariable(s1, "nomad/jobs/canary", "canary-s3cr3t"))
	cleanup1()

	// Restarted: sealed.
	s2, cleanup2 := testSealServer(t, dir, nodeID, 1, keyring)
	defer cleanup2()
	testutil.WaitForLeader(t, s2.RPC)
	waitForSeal(t, "seal configuration", func() bool {
		cfg, _ := s2.State().SealConfig(nil)
		return cfg != nil
	})
	must.True(t, s2.seal.sealed())

	// A rewrap to aead whose record says the leader is done.
	cfg, err := s2.State().SealConfig(nil)
	must.NoError(t, err)
	forged := cfg.Copy()
	forged.Rewrap = &structs.SealRewrap{
		ID:        "forged",
		To:        []*structs.SealInfo{{Type: structs.SealTypeAEAD}},
		Done:      []string{s2.seal.localID()},
		StartTime: time.Now().UnixNano(),
	}
	_, _, err = s2.raftApply(structs.SealConfigUpsertRequestType|structs.IgnoreUnknownTypeFlag,
		&structs.SealConfigUpsertRequest{Config: forged})
	must.NoError(t, err)

	// The reconcile loop runs within seconds; the rewrap must stay open.
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		cur, err := s2.State().SealConfig(nil)
		must.NoError(t, err)
		must.NotNil(t, cur.Rewrap, must.Sprint("the sealed leader completed a rewrap it took no part in"))
		must.Eq(t, structs.SealTypeShares, cur.Seals[0].Type)
		time.Sleep(200 * time.Millisecond)
	}
	must.True(t, s2.seal.sealed())

	// Unsealed, the leader wraps its keys with the new seal and completes.
	for _, share := range resp.Shares[:2] {
		sealUnseal(t, s2, share)
	}
	must.False(t, s2.seal.sealed())
	waitRewrapComplete(t, s2, structs.SealTypeAEAD)
	waitForSeal(t, "shares wraps dropped", func() bool { return s2.encrypter.keystoreSharesKeyID() == "" })
	value, err := readTestVariable(s2, "nomad/jobs/canary", false)
	must.NoError(t, err)
	must.Eq(t, "canary-s3cr3t", value)
}
