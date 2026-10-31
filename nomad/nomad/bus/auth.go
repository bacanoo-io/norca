// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package bus

import (
	"crypto/tls"
	"net"
	"slices"

	natsserver "github.com/nats-io/nats-server/v2/server"
)

// Grant kinds, shown in the status.
const (
	KindInternal = "internal"
	KindNode     = "node"
	KindToken    = "token"
)

// Credentials are what a connection presented in its CONNECT message.
type Credentials struct {
	// Username and Password: a node logs in with its node ID and secret.
	Username string
	Password string

	// Token is an ACL token secret.
	Token string

	// Remote is the address of the connection.
	Remote net.Addr

	// TLS is the TLS state of the connection; nil without TLS.
	TLS *tls.ConnectionState
}

// Grant is what an authenticated connection may do.
type Grant struct {
	// Kind is KindNode or KindToken (KindInternal for the server's own
	// connections).
	Kind string

	// Name identifies the connection in the status: the node ID or the
	// token accessor ID.
	Name string

	// All grants every subject of the bus.
	All bool

	// Publish and Subscribe are the subjects (NATS wildcards allowed) the
	// connection may publish and subscribe to, unless All is set.
	Publish   []string
	Subscribe []string
}

// Equal reports whether two grants allow the same.
func (g *Grant) Equal(o *Grant) bool {
	if g == nil || o == nil {
		return g == o
	}
	return g.Kind == o.Kind && g.Name == o.Name && g.All == o.All &&
		slices.Equal(g.Publish, o.Publish) && slices.Equal(g.Subscribe, o.Subscribe)
}

// Authorizer authenticates the credentials of a connection. It returns nil
// and an error, which is logged, when the connection is refused.
type Authorizer func(Credentials) (*Grant, error)

// clientAuth implements the custom client authentication of the embedded
// server.
type clientAuth Server

// Check authenticates a client connection and registers its permissions.
func (a *clientAuth) Check(c natsserver.ClientAuthentication) bool {
	s := (*Server)(a)
	if c.Kind() != natsserver.CLIENT {
		return false
	}
	opts := c.GetOpts()
	creds := Credentials{
		Username: opts.Username,
		Password: opts.Password,
		Token:    opts.Token,
		Remote:   c.RemoteAddress(),
		TLS:      c.GetTLSConnectionState(),
	}

	var grant *Grant
	if creds.Token != "" && creds.Token == s.internalToken {
		grant = &Grant{Kind: KindInternal, Name: "server", All: true}
	} else {
		g, err := s.authz(creds)
		if err != nil || g == nil {
			s.logger.Debug("connection refused", "remote", addrString(creds.Remote), "error", err)
			return false
		}
		grant = g
	}

	c.RegisterUser(&natsserver.User{
		Username:    grant.Kind + ":" + grant.Name,
		Permissions: permissions(grant),
	})
	s.l.Lock()
	s.clients[c.GetID()] = &client{creds: creds, grant: grant}
	s.l.Unlock()
	return true
}

// permissions converts a grant into NATS permissions. Every connection may
// receive the replies of its own requests (_INBOX).
func permissions(g *Grant) *natsserver.Permissions {
	if g.All {
		return nil
	}
	return &natsserver.Permissions{
		Publish: &natsserver.SubjectPermission{Allow: nonEmpty(g.Publish)},
		Subscribe: &natsserver.SubjectPermission{
			Allow: append(slices.Clone(g.Subscribe), "_INBOX.>"),
		},
		// Requests to the connection's own subscriptions may be answered.
		Response: &natsserver.ResponsePermission{MaxMsgs: 1},
	}
}

// nonEmpty returns a list that allows nothing for an empty one: NATS treats
// an empty allow list as "everything".
func nonEmpty(subjects []string) []string {
	if len(subjects) == 0 {
		return []string{"_NORCA.DENY.NOTHING"}
	}
	return slices.Clone(subjects)
}

// Revalidate authenticates the connections again, with the current tokens,
// policies and nodes, and closes those that are refused now or whose grant
// changed (a client reconnects and gets the new grant). The Norca server
// calls it when ACL objects or nodes change.
func (s *Server) Revalidate() {
	type entry struct {
		id uint64
		c  *client
	}
	s.l.Lock()
	entries := make([]entry, 0, len(s.clients))
	for id, c := range s.clients {
		entries = append(entries, entry{id, c})
	}
	s.l.Unlock()

	open := s.openClients()
	for _, e := range entries {
		if !open[e.id] {
			s.l.Lock()
			delete(s.clients, e.id)
			s.l.Unlock()
			continue
		}
		if e.c.grant.Kind == KindInternal {
			continue
		}
		grant, err := s.authz(e.c.creds)
		if err == nil && grant.Equal(e.c.grant) {
			continue
		}
		s.logger.Info("closing a bus connection whose rights changed",
			"name", e.c.grant.Kind+":"+e.c.grant.Name, "remote", addrString(e.c.creds.Remote), "error", err)
		if err := s.server().DisconnectClientByID(e.id); err != nil {
			s.logger.Debug("failed to close a bus connection", "id", e.id, "error", err)
		}
		s.l.Lock()
		delete(s.clients, e.id)
		s.l.Unlock()
	}
}

// openClients returns the IDs of the open client connections.
func (s *Server) openClients() map[uint64]bool {
	open := map[uint64]bool{}
	connz, err := s.server().Connz(&natsserver.ConnzOptions{Limit: s.config.MaxConnections + 100})
	if err != nil {
		return open
	}
	for _, c := range connz.Conns {
		open[c.Cid] = true
	}
	return open
}

func addrString(a net.Addr) string {
	if a == nil {
		return ""
	}
	return a.String()
}
