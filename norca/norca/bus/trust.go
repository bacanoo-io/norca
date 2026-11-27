// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package bus

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"net"
	"strconv"
	"sync"
	"time"
)

// routeTrustMax bounds the dialled routes routeTrust remembers.
const routeTrustMax = 256

// routeTrust remembers the route connections with the TLS state they were
// verified with, so that a reloaded TLS configuration checks them again
// (ReloadTLS): a route the new configuration refuses (a distrusted or
// removed CA) is closed, as a new handshake would be refused. NATS has no
// way to close a route, so the routes this server accepted are remembered
// by their connection, which is closed directly, and the routes it dialled
// by the certificate of the peer: when one of them is refused, the routes
// this server dialled are made again.
type routeTrust struct {
	mu sync.Mutex

	// accepted are the routes this server accepted.
	accepted map[net.Conn]acceptedRoute

	// dialled are the routes this server dialled, by the SHA-256 of the
	// peer's certificate; overflow is set when one was not remembered
	// because there were routeTrustMax already.
	dialled  map[[32]byte]tls.ConnectionState
	overflow bool
}

type acceptedRoute struct {
	state tls.ConnectionState
	at    time.Time
}

// wrap returns the route TLS configuration given to NATS: base, with the
// verified routes remembered. Nil stays nil.
func (t *routeTrust) wrap(base *tls.Config) *tls.Config {
	if base == nil {
		return nil
	}
	verify := base.VerifyConnection
	check := func(cs tls.ConnectionState) error {
		if verify == nil {
			return nil
		}
		return verify(cs)
	}
	conf := base.Clone()
	// Used when this server dials: the accepting side uses the
	// configuration of the connection below.
	conf.VerifyConnection = func(cs tls.ConnectionState) error {
		if err := check(cs); err != nil {
			return err
		}
		t.dialledRoute(cs)
		return nil
	}
	conf.GetConfigForClient = func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
		c := base.Clone()
		conn := hello.Conn
		c.VerifyConnection = func(cs tls.ConnectionState) error {
			if err := check(cs); err != nil {
				return err
			}
			t.acceptedRoute(conn, cs)
			return nil
		}
		return c, nil
	}
	return conf
}

func (t *routeTrust) dialledRoute(cs tls.ConnectionState) {
	if len(cs.PeerCertificates) == 0 {
		return
	}
	key := sha256.Sum256(cs.PeerCertificates[0].Raw)
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.dialled == nil {
		t.dialled = map[[32]byte]tls.ConnectionState{}
	}
	if _, ok := t.dialled[key]; !ok && len(t.dialled) >= routeTrustMax {
		t.overflow = true
		return
	}
	t.dialled[key] = cs
}

func (t *routeTrust) acceptedRoute(conn net.Conn, cs tls.ConnectionState) {
	if conn == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.accepted == nil {
		t.accepted = map[net.Conn]acceptedRoute{}
	}
	t.accepted[conn] = acceptedRoute{state: cs, at: time.Now()}
}

// prune forgets the accepted routes that are closed: not among the live
// remote addresses and older than connectGrace (a route in its handshake is
// not listed yet).
func (t *routeTrust) prune(live map[string]bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for conn, r := range t.accepted {
		if !live[conn.RemoteAddr().String()] && time.Since(r.at) > connectGrace {
			delete(t.accepted, conn)
		}
	}
}

// recheck checks the remembered routes with verify, the route verification
// of a reloaded configuration. It closes the accepted routes it refuses and
// returns them, and reports whether the dialled routes must be made again:
// one of them is refused, or not all were remembered. The dialled routes
// are forgotten then; they are remembered again when they are made.
func (t *routeTrust) recheck(verify func(tls.ConnectionState) error) (closed []net.Conn, redial bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for conn, r := range t.accepted {
		if verify != nil && verify(r.state) != nil {
			_ = conn.Close()
			delete(t.accepted, conn)
			closed = append(closed, conn)
		}
	}
	redial = t.overflow
	for _, cs := range t.dialled {
		if verify != nil && verify(cs) != nil {
			redial = true
		}
	}
	if redial {
		t.dialled, t.overflow = nil, false
	}
	return closed, redial
}

// verifyPeerAgain checks the certificate a client or leaf node connection
// presented against conf, as its handshake would be checked now: the chain
// to the client CAs and the check of the configuration (the distrusted
// CAs). A connection that presented no certificate passes: it is
// authenticated by its credentials, not by TLS.
func verifyPeerAgain(conf *tls.Config, cs *tls.ConnectionState) error {
	if conf == nil || cs == nil || len(cs.PeerCertificates) == 0 {
		return nil
	}
	inter := x509.NewCertPool()
	for _, c := range cs.PeerCertificates[1:] {
		inter.AddCert(c)
	}
	chains, err := cs.PeerCertificates[0].Verify(x509.VerifyOptions{
		Roots:         conf.ClientCAs,
		Intermediates: inter,
		KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	})
	if err != nil {
		return err
	}
	if conf.VerifyConnection == nil {
		return nil
	}
	again := *cs
	again.VerifiedChains = chains
	return conf.VerifyConnection(again)
}

// liveRoutes returns the remote addresses of the routes of the embedded
// server. The caller holds nsLock or routesLock.
func (s *Server) liveRoutes() map[string]bool {
	live := map[string]bool{}
	routez, err := s.ns.Routez(nil)
	if err != nil {
		return live
	}
	for _, r := range routez.Routes {
		live[net.JoinHostPort(r.IP, strconv.Itoa(r.Port))] = true
	}
	return live
}

// recheckTrust checks the open connections against the TLS configuration
// ReloadTLS just applied, and closes those a new handshake would refuse:
// clients and leaf nodes by the certificate they presented, routes as
// routeTrust says. The caller holds routesLock.
//
// NATS (v2.15) closes the client connections itself on any reload of its
// options (it reloads the authorization, and the users the custom
// authentication registers are not in its list): they reconnect under the
// new configuration. It keeps the leaf nodes, which are all in reader
// accounts: reload closes them. The check of the clients stays for a NATS
// that keeps them. The routes NATS keeps.
func (s *Server) recheckTrust(clientTLS, clusterTLS *tls.Config) error {
	s.l.Lock()
	var refused []uint64
	for id, c := range s.clients {
		if err := verifyPeerAgain(clientTLS, c.creds.TLS); err != nil {
			s.logger.Info("closing a bus connection whose certificate the reloaded TLS configuration refuses",
				"identity", c.grant.Kind+":"+c.grant.Name, "remote", addrString(c.creds.Remote), "error", err)
			refused = append(refused, id)
			delete(s.clients, id)
		}
	}
	s.l.Unlock()
	for _, id := range refused {
		if err := s.ns.DisconnectClientByID(id); err != nil {
			s.logger.Debug("failed to close a bus connection", "connection_id", id, "error", err)
		}
	}

	if clusterTLS == nil || !s.clustered {
		return nil
	}
	s.trust.prune(s.liveRoutes())
	closed, redial := s.trust.recheck(clusterTLS.VerifyConnection)
	for _, conn := range closed {
		s.logger.Info("closed a route whose certificate the reloaded TLS configuration refuses", "remote", conn.RemoteAddr().String())
	}
	if !redial {
		return nil
	}
	// The routes this server dialled are made again: NATS closes the
	// routes of the URLs a reload removes, and dials the ones it adds.
	s.logger.Info("making the routes this server dialled again under the reloaded TLS configuration")
	without := s.opts.Clone()
	without.Routes = s.routeURLs(nil)
	if err := s.reload(s.ns, without); err != nil {
		return err
	}
	opts := s.opts.Clone()
	if err := s.reload(s.ns, opts); err != nil {
		return err
	}
	s.opts = opts
	return nil
}
