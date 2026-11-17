// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package norca

import (
	"testing"
	"time"

	msgpackrpc "github.com/hashicorp/net-rpc-msgpackrpc"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	"github.com/bacanoo-io/norca/norca/norca/structs/config"
	"github.com/bacanoo-io/norca/norca/testutil"
	"github.com/shoenig/test/must"
	"github.com/shoenig/test/wait"
)

// The mark of a pack rendered by a server is not to be had by sending
// Release.ApplyRendered through a server that forwards it (review finding
// SRV-8): the forwarding server's own certificate is what the next server
// sees, so the first server decides who the caller is. A client node's
// certificate gets an ordinary Apply; a server of the federation keeps the
// mark through the same forwarding.
func TestRelease_ApplyRendered_ThroughForwardingServer(t *testing.T) {
	ci.Parallel(t)

	const testdata = "../helper/tlsutil/testdata/"
	tlsConfig := func(region, role string) *config.TLSConfig {
		return &config.TLSConfig{
			EnableRPC:            true,
			VerifyServerHostname: true,
			CAFile:               testdata + "norca-agent-ca.pem",
			CertFile:             testdata + region + "-" + role + "-norca.pem",
			KeyFile:              testdata + region + "-" + role + "-norca-key.pem",
		}
	}
	global, cleanupGlobal := TestServer(t, func(c *Config) {
		c.Region = "global"
		c.AuthoritativeRegion = "global"
		c.TLSConfig = tlsConfig("global", "server")
	})
	t.Cleanup(cleanupGlobal)
	foo, cleanupFoo := TestServer(t, func(c *Config) {
		c.Region = "regionFoo"
		c.AuthoritativeRegion = "global"
		c.TLSConfig = tlsConfig("regionFoo", "server")
	})
	t.Cleanup(cleanupFoo)
	TestJoin(t, global, foo)
	testutil.WaitForLeader(t, global.RPC)
	testutil.WaitForLeader(t, foo.RPC)
	must.Wait(t, wait.InitialSuccess(wait.BoolFunc(func() bool {
		return len(global.Regions()) == 2 && len(foo.Regions()) == 2 &&
			global.releases.ensureKinds() == nil && foo.releases.ensureKinds() == nil
	}), wait.Timeout(20*time.Second), wait.Gap(50*time.Millisecond)))

	// A release for the region "global", sent to a server with a
	// certificate of the given role.
	apply := func(name string, to *Server, certRegion, role string) *structs.ReleaseApplyResponse {
		t.Helper()
		codec := rpcClientWithTLS(t, to, tlsConfig(certRegion, role))
		defer codec.Close()
		req := &structs.ReleaseApplyRequest{
			Name:         name,
			Pack:         &structs.ReleasePack{Name: name, ServerRendered: true, Digest: "sha256:copied-from-a-signed-pack"},
			Resources:    []*structs.ReleaseResource{{Type: structs.ReleaseResourceJob, Job: testReleaseJob(name)}},
			WriteRequest: structs.WriteRequest{Region: "global", Namespace: "default"},
		}
		var resp structs.ReleaseApplyResponse
		must.NoError(t, msgpackrpc.CallWithCodec(codec, "Release.ApplyRendered", req, &resp))
		must.NotNil(t, resp.Release)
		return &resp
	}

	// A client node of regionFoo, through a server of regionFoo that
	// forwards to the region "global" with its own server certificate.
	resp := apply("forged", foo, "regionFoo", "client")
	must.False(t, resp.Release.Spec.Pack.ServerRendered,
		must.Sprint("a client got the server-rendered mark through a forwarding server"))

	// The same client directly at the region.
	resp = apply("direct", global, "global", "client")
	must.False(t, resp.Release.Spec.Pack.ServerRendered)

	// A server of the federation, through the same forwarding server: the
	// mark holds, as for the multi-region install of a rendered pack.
	resp = apply("rendered", foo, "global", "server")
	must.True(t, resp.Release.Spec.Pack.ServerRendered)
}
