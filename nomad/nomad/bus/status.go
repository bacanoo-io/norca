// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package bus

import (
	"net"
	"strconv"
	"strings"

	"github.com/hashicorp/nomad/nomad/structs"
	natsserver "github.com/nats-io/nats-server/v2/server"
)

// Status returns the status of the embedded server: routes, JetStream and
// connections.
func (s *Server) Status() (*structs.BusStatus, error) {
	ns := s.server()
	varz, err := ns.Varz(nil)
	if err != nil {
		return nil, err
	}
	st := &structs.BusStatus{
		Enabled:     true,
		Server:      s.config.ServerName,
		Cluster:     s.config.Region,
		ClientURL:   ns.ClientURL(),
		Clustered:   s.Clustered(),
		ClusterPort: s.config.ClusterPort,
		TLS:         s.config.ClientTLS != nil,
		Started:     varz.Start,
		Connections: &structs.BusConnections{
			InMsgs:   varz.InMsgs,
			OutMsgs:  varz.OutMsgs,
			InBytes:  varz.InBytes,
			OutBytes: varz.OutBytes,
		},
	}

	routez, err := ns.Routez(nil)
	if err != nil {
		return nil, err
	}
	for _, r := range routez.Routes {
		st.Routes = append(st.Routes, &structs.BusRoute{
			Server:   r.RemoteName,
			Address:  net.JoinHostPort(r.IP, strconv.Itoa(r.Port)),
			Outgoing: r.DidSolicit,
			RTT:      r.RTT,
		})
	}

	jsz, err := ns.Jsz(&natsserver.JSzOptions{Accounts: true, Streams: true, Config: true})
	if err != nil {
		return nil, err
	}
	if !jsz.Disabled {
		js := &structs.BusJetStream{
			Memory:     jsz.Memory,
			Storage:    jsz.Store,
			MaxMemory:  jsz.Config.MaxMemory,
			MaxStorage: jsz.Config.MaxStore,
			Streams:    jsz.Streams,
			Consumers:  jsz.Consumers,
			Messages:   jsz.Messages,
			Bytes:      jsz.Bytes,
		}
		if jsz.Meta != nil {
			js.MetaLeader = jsz.Meta.Leader
			js.ClusterSize = jsz.Meta.Size
		} else {
			js.MetaLeader = s.config.ServerName
			js.ClusterSize = 1
		}
		for _, acc := range jsz.AccountDetails {
			for _, sd := range acc.Streams {
				js.StreamDetails = append(js.StreamDetails, busStream(&sd, s.config.ServerName))
			}
		}
		st.JetStream = js
	}

	connz, err := ns.Connz(&natsserver.ConnzOptions{Username: true, Limit: s.config.MaxConnections + 100})
	if err != nil {
		return nil, err
	}
	conns := st.Connections
	for _, c := range connz.Conns {
		conns.Total++
		kind, _, _ := strings.Cut(c.AuthorizedUser, ":")
		switch kind {
		case KindInternal:
			conns.Internal++
		case KindNode:
			conns.Nodes++
		case KindToken:
			conns.Tokens++
		}
	}
	return st, nil
}

// busStream converts the monitoring detail of a stream.
func busStream(sd *natsserver.StreamDetail, self string) *structs.BusStream {
	out := &structs.BusStream{
		Name:      sd.Name,
		Messages:  sd.State.Msgs,
		Bytes:     sd.State.Bytes,
		FirstSeq:  sd.State.FirstSeq,
		LastSeq:   sd.State.LastSeq,
		Consumers: sd.State.Consumers,
		Replicas:  1,
		Leader:    self,
	}
	if c := sd.Config; c != nil {
		out.Subjects = c.Subjects
		out.Replicas = max(c.Replicas, 1)
		out.Storage = c.Storage.String()
		out.Compression = c.Compression.String()
		out.MaxAge = c.MaxAge
		out.MaxBytes = c.MaxBytes
	}
	if sd.Cluster != nil {
		out.Leader = sd.Cluster.Leader
	}
	return out
}
