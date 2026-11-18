// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"crypto/x509"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/helper/tlsutil"
	"github.com/bacanoo-io/norca/norca/norca/mock"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/bacanoo-io/norca/norca/norca/structs/config"
	"github.com/bacanoo-io/norca/norca/testutil"
	msgpackrpc "github.com/hashicorp/net-rpc-msgpackrpc"
	"github.com/shoenig/test/must"
)

// nodeRPCRegionErr is the text a node RPC refused at a region boundary
// carries.
const nodeRPCRegionErr = "node RPCs stay in their region"

// regionTestCerts holds a CA and writes certificates signed by it for the
// two-region tests of node RPCs.
type regionTestCerts struct {
	dir   string
	caPEM string
	caKey string
}

func newRegionTestCerts(t *testing.T) regionTestCerts {
	t.Helper()
	caPEM, caKey, err := tlsutil.GenerateCA(tlsutil.CAOpts{
		Name:               "Norca Region Test CA",
		Country:            "ZZ",
		Days:               5,
		Organization:       "Norca",
		OrganizationalUnit: "Test",
	})
	must.NoError(t, err)
	c := regionTestCerts{dir: t.TempDir(), caPEM: caPEM, caKey: caKey}
	must.NoError(t, os.WriteFile(filepath.Join(c.dir, "ca.pem"), []byte(caPEM), 0600))
	return c
}

// tlsConfig writes a certificate named name (common name and DNS name, with
// localhost) and returns a TLS configuration with verify_server_hostname
// that uses it.
func (c regionTestCerts) tlsConfig(t *testing.T, name string) *config.TLSConfig {
	t.Helper()
	signer, err := tlsutil.ParseSigner(c.caKey)
	must.NoError(t, err)
	pem, key, err := tlsutil.GenerateCert(tlsutil.CertOpts{
		Signer:      signer,
		CA:          c.caPEM,
		Name:        name,
		Days:        5,
		DNSNames:    []string{name, "localhost"},
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	})
	must.NoError(t, err)
	base := filepath.Join(c.dir, name)
	must.NoError(t, os.WriteFile(base+".pem", []byte(pem), 0600))
	must.NoError(t, os.WriteFile(base+".key", []byte(key), 0600))
	return &config.TLSConfig{
		EnableRPC:            true,
		VerifyServerHostname: true,
		CAFile:               filepath.Join(c.dir, "ca.pem"),
		CertFile:             base + ".pem",
		KeyFile:              base + ".key",
	}
}

// startRegionPair starts one server in region "alpha" and one in "beta",
// joins them and waits until alpha can forward to beta. tlsFor returns the
// TLS configuration of a region's server, or nil for none.
func startRegionPair(t *testing.T, tlsFor func(region string) *config.TLSConfig) (alpha, beta *Server) {
	t.Helper()
	start := func(region string) *Server {
		s, cleanup := TestServer(t, func(c *Config) {
			c.Region = region
			c.NumSchedulers = 0
			if tlsConf := tlsFor(region); tlsConf != nil {
				c.TLSConfig = tlsConf
			}
		})
		t.Cleanup(cleanup)
		return s
	}
	alpha, beta = start("alpha"), start("beta")
	TestJoin(t, alpha, beta)
	testutil.WaitForLeader(t, alpha.RPC)
	testutil.WaitForLeader(t, beta.RPC)
	testutil.WaitForResult(func() (bool, error) {
		alpha.peerLock.RLock()
		defer alpha.peerLock.RUnlock()
		if len(alpha.peers["beta"]) == 0 {
			return false, fmt.Errorf("alpha does not know the servers of beta yet")
		}
		return true, nil
	}, func(err error) { t.Fatal(err) })
	return alpha, beta
}

func nodeInState(t *testing.T, s *Server, nodeID string) bool {
	t.Helper()
	node, err := s.State().NodeByID(nil, nodeID)
	must.NoError(t, err)
	return node != nil
}

// TestNodeRPCRegion_TLS checks that the RPCs of a node stay in its region
// with mTLS and verify_server_hostname (review finding JOG-18). A node with a
// client.alpha.norca certificate could register itself in region beta by
// naming beta in the request to a server of alpha: alpha forwarded the
// request, beta accepted the certificate of alpha's server (servers of any
// region are accepted for federation) and Node.Register does not check the
// caller of a new node.
func TestNodeRPCRegion_TLS(t *testing.T) {
	ci.Parallel(t)

	certs := newRegionTestCerts(t)
	alpha, beta := startRegionPair(t, func(region string) *config.TLSConfig {
		return certs.tlsConfig(t, "server."+region+".norca")
	})
	clientTLS := certs.tlsConfig(t, "client.alpha.norca")
	alphaServerTLS := certs.tlsConfig(t, "server.alpha.norca")

	register := func(t *testing.T, s *Server, tlsConf *config.TLSConfig, region string) (string, error) {
		t.Helper()
		node := mock.Node()
		codec := rpcClientWithTLS(t, s, tlsConf)
		defer codec.Close()
		req := &structs.NodeRegisterRequest{
			Node:         node,
			WriteRequest: structs.WriteRequest{Region: region},
		}
		var resp structs.NodeUpdateResponse
		return node.ID, msgpackrpc.CallWithCodec(codec, "Node.Register", req, &resp)
	}

	t.Run("register in the own region", func(t *testing.T) {
		nodeID, err := register(t, alpha, clientTLS, "alpha")
		must.NoError(t, err)
		must.True(t, nodeInState(t, alpha, nodeID))
	})

	t.Run("register in another region through the own region", func(t *testing.T) {
		nodeID, err := register(t, alpha, clientTLS, "beta")
		must.ErrorContains(t, err, nodeRPCRegionErr)
		must.False(t, nodeInState(t, beta, nodeID))
		must.False(t, nodeInState(t, alpha, nodeID))
	})

	t.Run("register forwarded by a server of another region", func(t *testing.T) {
		// What a server of alpha that does not refuse the request sends:
		// beta must refuse it on its own.
		nodeID, err := register(t, beta, alphaServerTLS, "beta")
		must.ErrorContains(t, err, nodeRPCRegionErr)
		must.False(t, nodeInState(t, beta, nodeID))
	})

	t.Run("update status in another region", func(t *testing.T) {
		// A node of beta, registered there in-process.
		node := mock.Node()
		var regResp structs.NodeUpdateResponse
		must.NoError(t, beta.RPC("Node.Register", &structs.NodeRegisterRequest{
			Node:         node,
			WriteRequest: structs.WriteRequest{Region: "beta"},
		}, &regResp))

		codec := rpcClientWithTLS(t, alpha, clientTLS)
		defer codec.Close()
		req := &structs.NodeUpdateStatusRequest{
			NodeID:       node.ID,
			Status:       structs.NodeStatusDown,
			WriteRequest: structs.WriteRequest{Region: "beta", AuthToken: node.SecretID},
		}
		var resp structs.NodeUpdateResponse
		err := msgpackrpc.CallWithCodec(codec, "Node.UpdateStatus", req, &resp)
		must.ErrorContains(t, err, nodeRPCRegionErr)

		out, err := beta.State().NodeByID(nil, node.ID)
		must.NoError(t, err)
		must.NotNil(t, out)
		must.NotEq(t, structs.NodeStatusDown, out.Status)

		// A read that is not a node RPC still goes to beta.
		getReq := &structs.NodeSpecificRequest{
			NodeID:       node.ID,
			QueryOptions: structs.QueryOptions{Region: "beta"},
		}
		var getResp structs.SingleNodeResponse
		must.NoError(t, msgpackrpc.CallWithCodec(codec, "Node.GetNode", getReq, &getResp))
		must.NotNil(t, getResp.Node)
		must.Eq(t, node.ID, getResp.Node.ID)
	})
}

// TestNodeRPCRegion_Plain checks that without TLS too a node RPC naming
// another region is refused by the server it reaches, instead of being
// forwarded there (review finding JOG-18): the rule is about regions, not
// certificates. A client whose region does not match the servers it was
// pointed at gets the error instead of registering elsewhere.
func TestNodeRPCRegion_Plain(t *testing.T) {
	ci.Parallel(t)

	alpha, beta := startRegionPair(t, func(string) *config.TLSConfig { return nil })

	codec := rpcClient(t, alpha)
	defer codec.Close()

	node := mock.Node()
	req := &structs.NodeRegisterRequest{
		Node:         node,
		WriteRequest: structs.WriteRequest{Region: "beta"},
	}
	var resp structs.NodeUpdateResponse
	err := msgpackrpc.CallWithCodec(codec, "Node.Register", req, &resp)
	must.ErrorContains(t, err, nodeRPCRegionErr)
	must.ErrorContains(t, err, `this server is in region "alpha", the request names "beta"`)
	must.False(t, nodeInState(t, beta, node.ID))

	req.Region = "alpha"
	must.NoError(t, msgpackrpc.CallWithCodec(codec, "Node.Register", req, &resp))
	must.True(t, nodeInState(t, alpha, node.ID))

	// Another node RPC of the registered node, naming beta.
	allocsReq := &structs.NodeSpecificRequest{
		NodeID:       node.ID,
		SecretID:     node.SecretID,
		QueryOptions: structs.QueryOptions{Region: "beta", AuthToken: node.SecretID},
	}
	var allocsResp structs.NodeClientAllocsResponse
	err = msgpackrpc.CallWithCodec(codec, "Node.GetClientAllocs", allocsReq, &allocsResp)
	must.ErrorContains(t, err, nodeRPCRegionErr)
}
