// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Package bus runs the system bus of a Norca server: an embedded NATS server
// with JetStream. The servers of a region form one NATS cluster; their
// routes follow the Serf members of the region. Connections are
// authenticated by the Norca server (node secrets, ACL tokens) through an
// Authorizer, never by NATS accounts or JWTs.
//
// A clustered JetStream needs a route to another server before it runs, so
// the bus of a region with a single server runs a standalone JetStream. When
// a second server appears, the bus restarts clustered; the standalone store
// is kept aside (StandaloneStoreDir). Its streams are not carried over by
// NATS: the Norca server copies the Consul KV bucket from it (ReadStoreKV),
// the other streams are rebuilt.
package bus

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/hashicorp/go-hclog"
	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
)

// startTimeout bounds the wait for the listeners of the embedded server.
const startTimeout = 10 * time.Second

// Server is the embedded NATS server of a Norca server.
type Server struct {
	config *Config
	logger hclog.Logger
	authz  Authorizer

	// internalToken authenticates the in-process connections of the Norca
	// server; it is random per start and never leaves the process.
	internalToken string

	// routesLock serializes the route changes, which may restart the
	// embedded server.
	routesLock sync.Mutex
	routes     []string

	// storeAside is set once the standalone store was moved aside, so a
	// retried restart does not replace it with the store of the failed one.
	storeAside bool

	// nsLock guards the embedded server, replaced when the bus becomes
	// clustered.
	nsLock    sync.RWMutex
	ns        *natsserver.Server
	opts      *natsserver.Options
	clustered bool

	l sync.Mutex
	// clients are the authenticated connections by NATS client ID, with the
	// credentials and grant they were admitted with, for Revalidate.
	clients map[uint64]*client
}

type client struct {
	creds Credentials
	grant *Grant
}

// New starts the embedded NATS server and waits until it accepts
// connections. It runs clustered when config.Clustered is set (the region
// has or expects more than one server), standalone otherwise.
func New(config *Config, logger hclog.Logger, authz Authorizer) (*Server, error) {
	if err := config.Validate(); err != nil {
		return nil, err
	}
	if authz == nil {
		return nil, errors.New("bus: no authorizer")
	}

	token, err := randomToken()
	if err != nil {
		return nil, err
	}
	s := &Server{
		config:        config.Copy(),
		logger:        logger.Named("bus"),
		authz:         authz,
		internalToken: token,
		clients:       map[uint64]*client{},
	}
	ns, opts, err := s.start(config.Clustered, nil)
	if err != nil {
		return nil, err
	}
	s.ns, s.opts, s.clustered = ns, opts, config.Clustered
	return s, nil
}

// start creates and starts an embedded server.
func (s *Server) start(clustered bool, routes []string) (*natsserver.Server, *natsserver.Options, error) {
	config := s.config
	opts := &natsserver.Options{
		ServerName:                 config.ServerName,
		Host:                       config.BindAddr,
		Port:                       config.Port,
		MaxConn:                    config.MaxConnections,
		MaxPayload:                 config.MaxPayload,
		JetStream:                  true,
		StoreDir:                   config.StoreDir,
		JetStreamMaxMemory:         config.MaxMemory,
		JetStreamMaxStore:          config.MaxStorage,
		NoSigs:                     true,
		CustomClientAuthentication: (*clientAuth)(s),
	}
	if config.ClientTLS != nil {
		opts.TLS = true
		opts.TLSConfig = config.ClientTLS
		opts.TLSTimeout = 5
	}
	if clustered {
		opts.Cluster = natsserver.ClusterOpts{
			Name: config.Region,
			Host: config.BindAddr,
			Port: config.ClusterPort,
			// The servers of a region sit in different datacenters (zones):
			// compress the routes by their round-trip time.
			Compression: natsserver.CompressionOpts{Mode: natsserver.CompressionS2Auto},
		}
		if config.ClusterTLS != nil {
			opts.Cluster.TLSConfig = config.ClusterTLS
			opts.Cluster.TLSTimeout = 5
		}
		opts.Routes = s.routeURLs(routes)
	}

	ns, err := natsserver.NewServer(opts)
	if err != nil {
		return nil, nil, fmt.Errorf("bus: %w", err)
	}
	ns.SetLoggerV2(&natsLogger{logger: s.logger}, false, false, false)
	go ns.Start()
	addr := net.JoinHostPort(config.BindAddr, strconv.Itoa(config.Port))
	if !ns.ReadyForConnections(startTimeout) {
		ns.Shutdown()
		return nil, nil, fmt.Errorf("bus: the NATS server did not start listening on %s within %s", addr, startTimeout)
	}
	s.logger.Info("system bus started", "server", config.ServerName, "addr", addr,
		"clustered", clustered, "cluster_port", config.ClusterPort, "tls", config.ClientTLS != nil)
	return ns, opts, nil
}

func (s *Server) server() *natsserver.Server {
	s.nsLock.RLock()
	defer s.nsLock.RUnlock()
	return s.ns
}

// Shutdown stops the embedded server and closes every connection.
func (s *Server) Shutdown() {
	if s == nil {
		return
	}
	if ns := s.server(); ns != nil {
		ns.Shutdown()
		ns.WaitForShutdown()
	}
}

// Clustered reports whether the bus runs a clustered JetStream.
func (s *Server) Clustered() bool {
	s.nsLock.RLock()
	defer s.nsLock.RUnlock()
	return s.clustered
}

// SetRoutes replaces the routes to the other servers of the region
// ("host:port" of their cluster listeners). A clustered bus reloads them in
// place. A standalone bus that gets its first route restarts clustered: its
// standalone store is moved to StandaloneStoreDir. An unchanged list is
// ignored.
func (s *Server) SetRoutes(addrs []string) error {
	addrs = slices.Clone(addrs)
	slices.Sort(addrs)
	addrs = slices.Compact(addrs)

	s.routesLock.Lock()
	defer s.routesLock.Unlock()
	if slices.Equal(addrs, s.routes) {
		return nil
	}

	if !s.Clustered() {
		if len(addrs) == 0 {
			s.routes = addrs
			return nil
		}
		if err := s.becomeClustered(addrs); err != nil {
			return err
		}
		s.routes = addrs
		return nil
	}

	s.nsLock.Lock()
	defer s.nsLock.Unlock()
	opts := s.opts.Clone()
	opts.Routes = s.routeURLs(addrs)
	if err := s.ns.ReloadOptions(opts); err != nil {
		return fmt.Errorf("bus: reloading the routes: %w", err)
	}
	s.opts = opts
	s.routes = addrs
	s.logger.Debug("routes updated", "routes", addrs)
	return nil
}

// becomeClustered restarts the standalone server clustered with the given
// routes. Connections are closed and reconnect.
func (s *Server) becomeClustered(routes []string) error {
	s.logger.Warn("another server joined the region: restarting the system bus clustered; the Consul KV of the standalone bus is carried over, its other streams are rebuilt",
		"standalone_store", s.StandaloneStoreDir())

	old := s.server()
	old.Shutdown()
	old.WaitForShutdown()

	if s.config.StoreDir != "" && !s.storeAside {
		aside := s.StandaloneStoreDir()
		if err := os.RemoveAll(aside); err != nil {
			return fmt.Errorf("bus: removing %s: %w", aside, err)
		}
		if err := os.Rename(s.config.StoreDir, aside); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("bus: moving the standalone store aside: %w", err)
		}
		s.storeAside = true
	}
	s.l.Lock()
	clear(s.clients)
	s.l.Unlock()

	ns, opts, err := s.start(true, routes)
	if err != nil {
		return err
	}
	s.nsLock.Lock()
	s.ns, s.opts, s.clustered = ns, opts, true
	s.nsLock.Unlock()
	return nil
}

// StandaloneStoreDir is where the store of a standalone bus is kept when it
// becomes clustered.
func (s *Server) StandaloneStoreDir() string {
	if s.config.StoreDir == "" {
		return ""
	}
	return s.config.StoreDir + ".standalone"
}

// routeURLs returns the route URLs of the cluster addresses of the other
// servers, plus one to the server itself: NATS refuses a clustered
// JetStream without a configured route, and a server started before its
// peers are known has none yet. NATS recognizes and drops the route to
// itself; JetStream then waits for a real peer.
func (s *Server) routeURLs(addrs []string) []*url.URL {
	self := net.JoinHostPort(s.selfIP(), strconv.Itoa(s.config.ClusterPort))
	routes := []*url.URL{{Scheme: "nats-route", Host: self}}
	for _, a := range addrs {
		if a != self {
			routes = append(routes, &url.URL{Scheme: "nats-route", Host: a})
		}
	}
	return routes
}

// selfIP is the IP the server's own cluster listener is reached at.
func (s *Server) selfIP() string {
	if ip := net.ParseIP(s.config.BindAddr); ip != nil && !ip.IsUnspecified() {
		return s.config.BindAddr
	}
	return "127.0.0.1"
}

// InProcessConn implements nats.InProcessConnProvider: an in-process
// connection to the current embedded server, also after it was restarted
// clustered.
func (s *Server) InProcessConn() (net.Conn, error) {
	return s.server().InProcessConn()
}

// Connect opens an in-process connection of the Norca server with full
// rights. It does not use the network and reconnects when the embedded
// server restarts.
func (s *Server) Connect(options ...nats.Option) (*nats.Conn, error) {
	options = append([]nats.Option{
		nats.InProcessServer(s),
		nats.Token(s.internalToken),
		nats.Name("norca-server"),
		nats.MaxReconnects(-1),
	}, options...)
	return nats.Connect("", options...)
}

// JetStreamReady reports whether JetStream can be used: it runs and, when
// clustered, has a meta leader.
func (s *Server) JetStreamReady() bool {
	jsz, err := s.server().Jsz(&natsserver.JSzOptions{})
	if err != nil || jsz.Disabled {
		return false
	}
	return jsz.Meta == nil || jsz.Meta.Leader != ""
}

// ClientURL is the URL of the client listener, for tests and logs.
func (s *Server) ClientURL() string {
	return s.server().ClientURL()
}

func randomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("bus: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// natsLogger sends the log of the embedded server to the agent logger. Its
// notices (startup banner, listeners, routes, JetStream leaders) are logged
// at debug level; Norca logs the essentials itself.
type natsLogger struct {
	logger hclog.Logger
}

func (l *natsLogger) Noticef(format string, v ...any) { l.logger.Debug(fmt.Sprintf(format, v...)) }
func (l *natsLogger) Warnf(format string, v ...any)   { l.logger.Warn(fmt.Sprintf(format, v...)) }
func (l *natsLogger) Fatalf(format string, v ...any)  { l.logger.Error(fmt.Sprintf(format, v...)) }
func (l *natsLogger) Errorf(format string, v ...any)  { l.logger.Error(fmt.Sprintf(format, v...)) }
func (l *natsLogger) Debugf(format string, v ...any)  { l.logger.Debug(fmt.Sprintf(format, v...)) }
func (l *natsLogger) Tracef(format string, v ...any)  { l.logger.Trace(fmt.Sprintf(format, v...)) }
