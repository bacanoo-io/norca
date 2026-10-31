// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/hashicorp/nomad/helper/tlsutil"
	"github.com/hashicorp/nomad/nomad/bus"
	"github.com/hashicorp/nomad/nomad/state"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/nomad/nomad/structs/config"
	"github.com/hashicorp/serf/serf"
)

const (
	// busTag and busClusterTag are the Serf tags with the client and cluster
	// ports of a server's system bus.
	busTag        = "bus"
	busClusterTag = "bus_cluster"

	// busRoutesInterval is how often the routes are compared with the Serf
	// members besides the member events.
	busRoutesInterval = 30 * time.Second

	// busRevalidateInterval is how often the connections are checked for
	// changed ACL objects and nodes, and busRevalidateFullInterval how
	// often they are checked anyway (expiring tokens).
	busRevalidateInterval     = 5 * time.Second
	busRevalidateFullInterval = time.Minute
)

// busTelemetrySubject is the only subject a node may publish on: its usage
// reports.
func busTelemetrySubject(region, nodeID string) string {
	return "norca.telemetry." + region + "." + nodeID
}

// setupBus starts the system bus when it is enabled. The bus runs clustered
// when the region has or expects more than one server.
func (s *Server) setupBus() error {
	cfg := s.config.Bus.Copy()
	if cfg == nil || !cfg.Enabled {
		return nil
	}

	cfg.ServerName = fmt.Sprintf("%s.%s", s.config.NodeName, s.config.Region)
	cfg.Region = s.config.Region
	if s.config.RPCAddr != nil {
		cfg.BindAddr = s.config.RPCAddr.IP.String()
	}
	if cfg.StoreDir == "" {
		dir := filepath.Join(s.config.DataDir, "bus")
		if s.config.DevMode && s.config.DataDir == "" {
			tmp, err := os.MkdirTemp("", "norca-bus")
			if err != nil {
				return fmt.Errorf("failed to create the bus store directory: %w", err)
			}
			dir = tmp
		}
		cfg.StoreDir = dir
	}
	cfg.Clustered = s.config.BootstrapExpect > 1 || s.raftServers() > 1

	if s.config.TLSConfig != nil && s.config.TLSConfig.EnableRPC {
		clientTLS, clusterTLS, err := busTLS(s.config.TLSConfig, s.config.Region)
		if err != nil {
			return err
		}
		cfg.ClientTLS, cfg.ClusterTLS = clientTLS, clusterTLS
	}

	b, err := bus.New(cfg, s.logger, s.busAuthorize)
	if err != nil {
		return err
	}
	s.bus = b

	s.busRoutesCh = make(chan struct{}, 1)
	go s.busRoutesLoop()
	go s.busRevalidateLoop()
	go s.busCarryOverLoop()
	return nil
}

// raftServers returns the number of servers in the Raft configuration.
func (s *Server) raftServers() int {
	if s.raft == nil {
		return 0
	}
	future := s.raft.GetConfiguration()
	if err := future.Error(); err != nil {
		return 0
	}
	return len(future.Configuration().Servers)
}

// shutdownBus stops the system bus.
func (s *Server) shutdownBus() {
	if s.bus != nil {
		s.bus.Shutdown()
	}
}

// busTLS returns the TLS configurations of the bus listeners from the agent
// TLS configuration. Client connections need not present a certificate:
// they authenticate with node secrets or ACL tokens. Routes require the
// certificate of a server of the region, in both directions.
func busTLS(conf *config.TLSConfig, region string) (*tls.Config, *tls.Config, error) {
	tlsConf, err := tlsutil.NewTLSConfiguration(conf, true, true)
	if err != nil {
		return nil, nil, err
	}
	incoming, err := tlsConf.IncomingTLSConfig()
	if err != nil {
		return nil, nil, err
	}
	outgoing, err := tlsConf.OutgoingTLSConfig()
	if err != nil {
		return nil, nil, err
	}

	clientTLS := incoming.Clone()
	clientTLS.ClientAuth = tls.VerifyClientCertIfGiven

	pool := incoming.ClientCAs
	clusterTLS := incoming.Clone()
	clusterTLS.ClientAuth = tls.RequireAnyClientCert
	clusterTLS.GetClientCertificate = outgoing.GetClientCertificate
	clusterTLS.RootCAs = pool
	// The routes dial IP addresses, which the certificates don't name: the
	// chain and the server name of the region are verified here instead.
	clusterTLS.InsecureSkipVerify = true
	clusterTLS.VerifyPeerCertificate = busServerCertVerifier(pool, region)
	return clientTLS, clusterTLS, nil
}

// busServerCertVerifier verifies that the peer presents a certificate of
// the CA for "server.<region>.nomad".
func busServerCertVerifier(pool *x509.CertPool, region string) func([][]byte, [][]*x509.Certificate) error {
	want := "server." + region + ".nomad"
	return func(raw [][]byte, _ [][]*x509.Certificate) error {
		if len(raw) == 0 {
			return errors.New("bus route: no certificate")
		}
		certs := make([]*x509.Certificate, 0, len(raw))
		for _, r := range raw {
			c, err := x509.ParseCertificate(r)
			if err != nil {
				return fmt.Errorf("bus route: %w", err)
			}
			certs = append(certs, c)
		}
		inter := x509.NewCertPool()
		for _, c := range certs[1:] {
			inter.AddCert(c)
		}
		_, err := certs[0].Verify(x509.VerifyOptions{
			Roots:         pool,
			Intermediates: inter,
			DNSName:       want,
			KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageAny},
		})
		if err != nil {
			return fmt.Errorf("bus route: %w", err)
		}
		return nil
	}
}

// busAuthorize authenticates a bus connection: a node with its ID and
// secret, anyone else with an ACL token whose policies have a bus rule.
func (s *Server) busAuthorize(c bus.Credentials) (*bus.Grant, error) {
	if c.Username != "" {
		node, err := s.fsm.State().NodeByID(nil, c.Username)
		if err != nil {
			return nil, err
		}
		if node == nil || node.SecretID == "" || !constantTimeEqual(node.SecretID, c.Password) {
			return nil, errors.New("unknown node or wrong secret")
		}
		return &bus.Grant{
			Kind:    bus.KindNode,
			Name:    node.ID,
			Publish: append([]string{busTelemetrySubject(s.config.Region, node.ID)}, bus.ConsulKVSubjects()...),
		}, nil
	}

	if !s.config.ACLEnabled {
		return &bus.Grant{Kind: bus.KindToken, Name: "anonymous", All: true}, nil
	}
	aclObj, err := s.ResolveToken(c.Token)
	if err != nil {
		return nil, err
	}
	g := aclObj.BusGrant()
	if g.Empty() {
		return nil, errors.New("the token has no bus rights")
	}
	name := "anonymous"
	if c.Token != "" {
		if token, err := s.ResolveSecretToken(c.Token); err == nil && token != nil {
			name = token.AccessorID
		} else {
			name = "leader"
		}
	}
	return &bus.Grant{
		Kind:      bus.KindToken,
		Name:      name,
		All:       g.All,
		Publish:   g.Publish,
		Subscribe: g.Subscribe,
	}, nil
}

func constantTimeEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// busRoutesLoop keeps the routes of the bus in line with the servers of the
// region in Serf.
func (s *Server) busRoutesLoop() {
	ticker := time.NewTicker(busRoutesInterval)
	defer ticker.Stop()
	for {
		s.updateBusRoutes()
		select {
		case <-s.shutdownCh:
			return
		case <-s.busRoutesCh:
		case <-ticker.C:
		}
	}
}

// notifyBusRoutes asks busRoutesLoop to compare the routes now.
func (s *Server) notifyBusRoutes() {
	if s.busRoutesCh == nil {
		return
	}
	select {
	case s.busRoutesCh <- struct{}{}:
	default:
	}
}

func (s *Server) updateBusRoutes() {
	if s.serf == nil {
		return
	}
	local := s.serf.LocalMember().Name
	var routes []string
	for _, m := range s.serf.Members() {
		if m.Name == local || m.Status != serf.StatusAlive {
			continue
		}
		if ok, parts := isNomadServer(m); !ok || parts.Region != s.config.Region {
			continue
		}
		port, err := strconv.Atoi(m.Tags[busClusterTag])
		if err != nil || port <= 0 {
			continue
		}
		routes = append(routes, net.JoinHostPort(m.Addr.String(), strconv.Itoa(port)))
	}
	if err := s.bus.SetRoutes(routes); err != nil {
		s.logger.Error("failed to update the routes of the system bus", "error", err)
	}
}

// busRevalidateLoop closes the bus connections whose token, policies or
// node changed or went away.
func (s *Server) busRevalidateLoop() {
	ticker := time.NewTicker(busRevalidateInterval)
	defer ticker.Stop()
	var lastIndex uint64
	lastFull := time.Now()
	for {
		select {
		case <-s.shutdownCh:
			return
		case <-ticker.C:
		}
		index := busAuthIndex(s.fsm.State())
		if index == lastIndex && time.Since(lastFull) < busRevalidateFullInterval {
			continue
		}
		lastIndex, lastFull = index, time.Now()
		s.bus.Revalidate()
	}
}

// busAuthIndex is the highest index of the tables the bus authentication
// depends on.
func busAuthIndex(st *state.StateStore) uint64 {
	var max uint64
	for _, table := range []string{"acl_token", "acl_policy", state.TableACLRoles, "nodes"} {
		if idx, err := st.Index(table); err == nil && idx > max {
			max = idx
		}
	}
	return max
}

// busStatus returns the status of the local bus.
func (s *Server) busStatus() (*structs.BusStatus, error) {
	if s.bus == nil {
		return &structs.BusStatus{Enabled: false}, nil
	}
	return s.bus.Status()
}
