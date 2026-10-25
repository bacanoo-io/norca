// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package client

import (
	"testing"
	"time"

	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/client/config"
	"github.com/hashicorp/nomad/helper/pointer"
	"github.com/hashicorp/nomad/nomad/structs"
	structsc "github.com/hashicorp/nomad/nomad/structs/config"
	"github.com/hashicorp/nomad/testutil"
	"github.com/miekg/dns"
	"github.com/shoenig/test/must"
	"github.com/shoenig/test/wait"
)

// TestClient_DNS answers a service registered in the servers through the DNS
// server of a client, which reads the catalog with its node identity.
func TestClient_DNS(t *testing.T) {
	ci.Parallel(t)

	s1, addr, cleanupS1 := testServer(t, nil)
	defer cleanupS1()
	testutil.WaitForLeader(t, s1.RPC)

	c1, cleanupC1 := TestClient(t, func(c *config.Config) {
		c.Servers = []string{addr}
		c.DNS = &structsc.DNSConfig{Enabled: pointer.Of(true), Bind: "127.0.0.1:0"}
	})
	defer cleanupC1()
	must.NotEq(t, "", c1.DNSAddr())

	reg := &structs.ServiceRegistration{
		ID: "web-1", ServiceName: "web", Namespace: "default", NodeID: c1.NodeID(), JobID: "j", AllocID: "a",
		Datacenter: "dc1", Address: "10.1.2.3", Port: 8080, Ready: true, Health: structs.ServiceHealthPassing,
	}
	must.NoError(t, s1.State().UpsertServiceRegistrations(structs.MsgTypeTestSetup, 1000, []*structs.ServiceRegistration{reg}))

	query := func() *dns.Msg {
		m := new(dns.Msg)
		m.SetQuestion("web.service.nomad.", dns.TypeA)
		resp, _, err := (&dns.Client{Timeout: 5 * time.Second}).Exchange(m, c1.DNSAddr())
		if err != nil {
			return nil
		}
		return resp
	}

	// the node registers before it can read the catalog
	must.Wait(t, wait.InitialSuccess(
		wait.BoolFunc(func() bool {
			resp := query()
			return resp != nil && len(resp.Answer) == 1
		}),
		wait.Timeout(20*time.Second), wait.Gap(100*time.Millisecond),
	))
	resp := query()
	must.Eq(t, "10.1.2.3", resp.Answer[0].(*dns.A).A.String())

	// a failing instance disappears
	failing := reg.Copy()
	failing.Health = structs.ServiceHealthCritical
	must.NoError(t, s1.State().UpsertServiceRegistrations(structs.MsgTypeTestSetup, 1001, []*structs.ServiceRegistration{failing}))
	must.Wait(t, wait.InitialSuccess(
		wait.BoolFunc(func() bool {
			resp := query()
			return resp != nil && resp.Rcode == dns.RcodeNameError
		}),
		wait.Timeout(10*time.Second), wait.Gap(50*time.Millisecond),
	))
}
