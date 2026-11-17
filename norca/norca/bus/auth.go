// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package bus

import (
	"crypto/subtle"
	"crypto/tls"
	"math"
	"net"
	"slices"
	"strings"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
)

// Grant kinds, shown in the status.
const (
	KindInternal = "internal"
	KindNode     = "node"
	KindToken    = "token"
	// KindWorkload is the workload identity of a task (Norca).
	KindWorkload = "workload"
	// KindLeaf is a NATS server attached as a leaf node with an ACL token.
	KindLeaf = "leaf"
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

	// Leaf is set for a leaf node connection, which authenticates with its
	// Token only.
	Leaf bool
}

// Grant is what an authenticated connection may do.
type Grant struct {
	// Kind is KindNode, KindToken or KindWorkload (KindInternal for the
	// server's own connections).
	Kind string

	// Name identifies the connection in the status: the node ID, the
	// token accessor ID, or <allocation ID>/<task> of a workload identity.
	Name string

	// All grants every subject of the bus.
	All bool

	// Publish and Subscribe are the subjects (NATS wildcards allowed) the
	// connection may publish and subscribe to, unless All is set.
	Publish   []string
	Subscribe []string

	// PublishDeny and SubscribeDeny are subjects the connection may not
	// publish or receive although Publish or Subscribe cover them (Norca):
	// the admin events its token may not see, and the JetStream API of the
	// admin event stream. NATS also withholds a denied subject from a wider
	// subscription that covers it.
	PublishDeny   []string
	SubscribeDeny []string

	// Inbox is the subject prefix of the replies of the connection's own
	// requests, e.g. "_INBOX.<accessor id>": the connection may subscribe
	// below it and nowhere else in the inboxes. Empty: the connection gets
	// no replies (a node only publishes). The connections of one account
	// share the subjects, so a common "_INBOX.>" would let every connection
	// read the replies of all the others.
	Inbox string

	// Account is the NATS account of the connection: a reader account
	// (ReaderAccount). Empty: the bus account (AccountName).
	Account string
}

// InboxPrefix is the first token of the inbox subjects, as in NATS.
const InboxPrefix = "_INBOX"

// Equal reports whether two grants allow the same.
func (g *Grant) Equal(o *Grant) bool {
	if g == nil || o == nil {
		return g == o
	}
	return g.Kind == o.Kind && g.Name == o.Name && g.All == o.All && g.Inbox == o.Inbox && g.Account == o.Account &&
		slices.Equal(g.Publish, o.Publish) && slices.Equal(g.Subscribe, o.Subscribe) &&
		slices.Equal(g.PublishDeny, o.PublishDeny) && slices.Equal(g.SubscribeDeny, o.SubscribeDeny)
}

// Authorizer authenticates the credentials of a connection. It returns nil
// and an error, which is logged, when the connection is refused.
type Authorizer func(Credentials) (*Grant, error)

// clientAuth implements the custom client authentication of one embedded
// server: a connection is registered in an account of that server, also
// while a restarted server replaces it.
type clientAuth struct {
	s  *Server
	ns *natsserver.Server
}

// Check authenticates a client connection and registers its permissions
// and account.
func (a *clientAuth) Check(c natsserver.ClientAuthentication) bool {
	s := a.s
	opts := c.GetOpts()
	creds := Credentials{
		Remote: c.RemoteAddress(),
		TLS:    c.GetTLSConnectionState(),
	}
	switch c.Kind() {
	case natsserver.CLIENT:
		creds.Username, creds.Password, creds.Token = opts.Username, opts.Password, opts.Token
	case natsserver.LEAF:
		// A leaf sends the user of its URL also as the token
		// (nats-leaf://<token>@host); it never logs in as a node or with
		// the internal token.
		creds.Token, creds.Leaf = opts.Token, true
		if creds.Token == "" {
			creds.Token = opts.Username
		}
	default:
		return false
	}

	var grant *Grant
	if !creds.Leaf && creds.Token != "" && subtle.ConstantTimeCompare([]byte(creds.Token), []byte(s.internalToken)) == 1 {
		grant = &Grant{Kind: KindInternal, Name: "server", All: true}
	} else {
		g, err := s.authz(creds)
		if err != nil || g == nil {
			s.logger.Debug("connection refused", "remote", addrString(creds.Remote), "error", err)
			return false
		}
		grant = g
	}
	if creds.Leaf != (grant.Kind == KindLeaf) {
		s.logger.Warn("bus connection refused: a leaf node connection needs a leaf grant, and only a leaf node gets one",
			"name", grant.Kind+":"+grant.Name, "remote", addrString(creds.Remote))
		return false
	}

	accName := grant.Account
	if accName == "" {
		accName = AccountName
	}
	if until := s.readerBanned(accName); !until.IsZero() {
		s.logger.Debug("bus connection refused: the account is cut off for its waiting fetches", "account", accName,
			"name", grant.Kind+":"+grant.Name, "remote", addrString(creds.Remote), "until", until)
		return false
	}
	acc, err := a.ns.LookupAccount(accName)
	if err != nil {
		s.logger.Warn("bus connection refused: unknown account", "account", accName,
			"name", grant.Kind+":"+grant.Name, "remote", addrString(creds.Remote), "error", err)
		return false
	}
	// The limits of the account are checked here: when RegisterUser fails on
	// them it closes the connection, but the authentication would still
	// succeed and nats-server would keep a closed leaf connection in its
	// list of leaf nodes for good. The lock makes the check and the
	// registration one step for the connections authenticated here.
	s.registerLock.Lock()
	full := (creds.Leaf && acc.MaxTotalLeafNodesReached()) || (!creds.Leaf && acc.MaxTotalConnectionsReached())
	if !full {
		c.RegisterUser(&natsserver.User{
			Username:    grant.Kind + ":" + grant.Name,
			Permissions: permissions(grant),
			Account:     acc,
		})
	}
	s.registerLock.Unlock()
	if full {
		s.logger.Warn("bus connection refused: the account has its maximum of connections", "account", accName,
			"name", grant.Kind+":"+grant.Name, "remote", addrString(creds.Remote))
		return false
	}
	s.l.Lock()
	s.clients[c.GetID()] = &client{creds: creds, grant: grant, at: time.Now()}
	s.l.Unlock()
	return true
}

// permissions converts a grant into NATS permissions. A connection may
// receive the replies of its own requests below its Inbox only. The publish
// and subscribe subjects of a grant never reach into the inboxes
// (CoversInbox): the authorizer leaves such subjects out.
func permissions(g *Grant) *natsserver.Permissions {
	if g.All {
		return nil
	}
	subscribe := slices.Clone(g.Subscribe)
	if g.Inbox != "" {
		subscribe = append(subscribe, g.Inbox+".>")
	}
	return &natsserver.Permissions{
		Publish:   &natsserver.SubjectPermission{Allow: nonEmpty(g.Publish), Deny: slices.Clone(g.PublishDeny)},
		Subscribe: &natsserver.SubjectPermission{Allow: nonEmpty(subscribe), Deny: slices.Clone(g.SubscribeDeny)},
		// Requests to the connection's own subscriptions may be answered.
		Response: &natsserver.ResponsePermission{MaxMsgs: 1},
	}
}

// CoversInbox reports whether a subject of a rule reaches the inbox
// subjects: its first token is "_INBOX" or a wildcard. Such a rule would let
// a connection read, or write, the replies of other connections.
func CoversInbox(subject string) bool {
	first, _, _ := strings.Cut(subject, ".")
	return first == InboxPrefix || first == "*" || first == ">"
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

	// A connection is forgotten only when it is known to be gone: it is
	// not listed as open, the listing is complete, and it is not so new
	// that the server may not list it yet (a connection is recorded here
	// during its CONNECT, a leaf node gets into the list of the server only
	// after it). A connection forgotten while it is open would never be
	// checked again.
	open, listed := s.openClients()
	retry := false
	for _, e := range entries {
		present := open[e.id]
		recent := time.Since(e.c.at) < connectGrace
		if listed && !present && !recent {
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
			if !present && recent {
				// Not known to the server yet (or closed a moment ago):
				// kept, and looked at again when it must be listed.
				retry = true
				continue
			}
		}
		s.l.Lock()
		delete(s.clients, e.id)
		s.l.Unlock()
	}
	if retry {
		time.AfterFunc(connectGrace, func() {
			select {
			case <-s.stopCh:
			default:
				s.Revalidate()
			}
		})
	}
}

// connectGrace is how long after its authentication a connection may be
// missing from the lists of the server without being taken for closed.
const connectGrace = time.Minute

// openClients returns the IDs of the open client and leaf node connections,
// and whether the list is complete.
func (s *Server) openClients() (map[uint64]bool, bool) {
	open := map[uint64]bool{}
	ns := s.server()
	// One call without a real limit: pages could skip a connection when
	// another one closes between them. The server cuts the limit to the
	// number of connections.
	connz, err := ns.Connz(&natsserver.ConnzOptions{Limit: math.MaxInt32})
	if err != nil {
		return open, false
	}
	for _, c := range connz.Conns {
		open[c.Cid] = true
	}
	leafz, err := ns.Leafz(nil)
	if err != nil {
		return open, false
	}
	for _, l := range leafz.Leafs {
		open[l.ID] = true
	}
	return open, true
}

func addrString(a net.Addr) string {
	if a == nil {
		return ""
	}
	return a.String()
}
