// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package client

import (
	"errors"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/nomad/client/consulcompat"
	"github.com/hashicorp/nomad/helper/tlsutil"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/nats-io/nats.go"
)

// errBusUnknown is returned by BusConn before the first heartbeat told the
// client whether its servers run a system bus.
var errBusUnknown = errors.New("the servers of the client are not known yet")

// busClient is the connection of the client to the system bus of the
// servers (embedded NATS), opened on first use with the node ID and secret.
type busClient struct {
	l sync.Mutex

	// known is set by the first heartbeat; addrs are the bus addresses of
	// the servers it listed.
	known bool
	addrs []string

	nc *nats.Conn
}

// setBusServers records the bus addresses of the servers of a heartbeat.
// An open connection keeps its servers: NATS learns the servers of the
// cluster from the one it is connected to.
func (c *Client) setBusServers(servers []*structs.NodeServerInfo) {
	var addrs []string
	for _, s := range servers {
		if s.BusAdvertiseAddr != "" {
			addrs = append(addrs, s.BusAdvertiseAddr)
		}
	}
	slices.Sort(addrs)
	c.bus.l.Lock()
	defer c.bus.l.Unlock()
	c.bus.known = true
	c.bus.addrs = addrs
}

// BusConn returns the connection of the client to the system bus,
// connecting on first use. It returns consulcompat.ErrNoBus when the bus is
// disabled on the client or the servers run none.
func (c *Client) BusConn() (*nats.Conn, error) {
	cfg := c.GetConfig()
	if cfg.BusDisabled {
		return nil, consulcompat.ErrNoBus
	}

	c.bus.l.Lock()
	defer c.bus.l.Unlock()
	if c.bus.nc != nil && !c.bus.nc.IsClosed() {
		return c.bus.nc, nil
	}
	if !c.bus.known {
		return nil, errBusUnknown
	}
	if len(c.bus.addrs) == 0 {
		return nil, consulcompat.ErrNoBus
	}

	scheme := "nats://"
	opts := []nats.Option{
		nats.Name("norca-client " + c.NodeID()),
		nats.UserInfo(c.NodeID(), c.secretNodeID()),
		nats.MaxReconnects(-1),
		nats.ReconnectWait(time.Second),
		nats.Timeout(10 * time.Second),
	}
	if tlsConf := cfg.TLSConfig; tlsConf != nil && tlsConf.EnableRPC {
		tw, err := tlsutil.NewTLSConfiguration(tlsConf, true, true)
		if err != nil {
			return nil, err
		}
		tc, err := tw.OutgoingTLSConfig()
		if err != nil {
			return nil, err
		}
		tc = tc.Clone()
		// The servers present the certificate of the region's servers.
		tc.ServerName = "server." + cfg.Region + ".nomad"
		opts = append(opts, nats.Secure(tc))
		scheme = "tls://"
	}
	urls := make([]string, 0, len(c.bus.addrs))
	for _, a := range c.bus.addrs {
		urls = append(urls, scheme+a)
	}
	nc, err := nats.Connect(strings.Join(urls, ","), opts...)
	if err != nil {
		return nil, err
	}
	c.bus.nc = nc
	c.logger.Debug("connected to the system bus of the servers", "server", nc.ConnectedUrlRedacted())
	return nc, nil
}

// shutdownBus closes the bus connection.
func (c *Client) shutdownBus() {
	c.bus.l.Lock()
	defer c.bus.l.Unlock()
	if c.bus.nc != nil {
		c.bus.nc.Close()
		c.bus.nc = nil
	}
}
