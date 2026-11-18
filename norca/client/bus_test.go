// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package client

import (
	"testing"
	"time"

	"github.com/bacanoo-io/norca/norca/ci"
	"github.com/bacanoo-io/norca/norca/helper/testlog"
	"github.com/bacanoo-io/norca/norca/norca/structs"
	nconfig "github.com/bacanoo-io/norca/norca/norca/structs/config"
	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/shoenig/test/must"
	"github.com/shoenig/test/wait"
)

// TestClient_SetBusServers checks what a heartbeat with other bus servers
// does to the bus connection. A connection that is up keeps its servers. One
// that is down is closed, so that the next use connects with the new list:
// it knew only the servers it had, and tried them for ever when all of them
// were replaced while it was down.
func TestClient_SetBusServers(t *testing.T) {
	ci.Parallel(t)

	ns, err := natsserver.NewServer(&natsserver.Options{Host: "127.0.0.1", Port: -1, NoSigs: true, NoLog: true})
	must.NoError(t, err)
	go ns.Start()
	must.True(t, ns.ReadyForConnections(10*time.Second))
	defer ns.Shutdown()

	nc, err := nats.Connect(ns.ClientURL(), nats.MaxReconnects(-1), nats.ReconnectWait(50*time.Millisecond))
	must.NoError(t, err)
	defer nc.Close()

	c := &Client{logger: testlog.HCLogger(t)}
	servers := func(addrs ...string) []*structs.NodeServerInfo {
		out := make([]*structs.NodeServerInfo, 0, len(addrs))
		for _, a := range addrs {
			out = append(out, &structs.NodeServerInfo{BusAdvertiseAddr: a})
		}
		return out
	}
	c.setBusServers(servers("10.0.0.1:4649"))
	c.bus.nc = nc

	// Up: another list changes nothing.
	c.setBusServers(servers("10.0.0.2:4649", "10.0.0.3:4649"))
	must.True(t, c.bus.nc == nc)
	must.Eq(t, []string{"10.0.0.2:4649", "10.0.0.3:4649"}, c.bus.addrs)

	// Down, the same list: the connection keeps trying.
	ns.Shutdown()
	must.Wait(t, wait.InitialSuccess(
		wait.BoolFunc(func() bool { return !nc.IsConnected() }),
		wait.Timeout(10*time.Second), wait.Gap(20*time.Millisecond)))
	c.setBusServers(servers("10.0.0.3:4649", "10.0.0.2:4649"))
	must.True(t, c.bus.nc == nc)
	must.False(t, nc.IsClosed())

	// Down, another list: closed, the next use opens a new one.
	c.setBusServers(servers("10.0.0.9:4649"))
	must.Nil(t, c.bus.nc)
	must.True(t, nc.IsClosed())
	run, err := c.ServersRunBus()
	must.NoError(t, err)
	must.True(t, run)
}

// A TLS reload closes the bus connection, so that the next use connects
// with the new TLS configuration: the connection kept the CA it was opened
// with, and after a change of the CA it could not connect again until the
// agent restarted (review finding MUK-17).
func TestClient_ReloadTLS_BusConn(t *testing.T) {
	ci.Parallel(t)

	ns, err := natsserver.NewServer(&natsserver.Options{Host: "127.0.0.1", Port: -1, NoSigs: true, NoLog: true})
	must.NoError(t, err)
	go ns.Start()
	must.True(t, ns.ReadyForConnections(10*time.Second))
	defer ns.Shutdown()
	nc, err := nats.Connect(ns.ClientURL())
	must.NoError(t, err)
	defer nc.Close()

	c, cleanup := TestClient(t, nil)
	defer func() { _ = cleanup() }()
	c.bus.l.Lock()
	c.bus.known, c.bus.addrs, c.bus.nc = true, []string{ns.Addr().String()}, nc
	c.bus.l.Unlock()

	const dir = "../helper/tlsutil/testdata/"
	must.NoError(t, c.reloadTLSConnections(&nconfig.TLSConfig{
		EnableRPC:            true,
		VerifyServerHostname: true,
		CAFile:               dir + "norca-agent-ca.pem",
		CertFile:             dir + "global-client-norca.pem",
		KeyFile:              dir + "global-client-norca-key.pem",
	}))
	must.True(t, nc.IsClosed(), must.Sprint("the bus connection kept the TLS of before the reload"))
	c.bus.l.Lock()
	defer c.bus.l.Unlock()
	must.Nil(t, c.bus.nc)
}
