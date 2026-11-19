// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Package bus runs the system bus of a Norca server: an embedded NATS server
// with JetStream. The servers of a region form one NATS cluster; their
// routes follow the Serf members of the region. Connections are
// authenticated by the Norca server (node secrets, ACL tokens, workload
// identities) through an Authorizer, never by NATS users or JWTs. They
// connect into the account NORCA (AccountName), where the streams live; a
// reader of the change streams may get an account of its own that imports
// from it (ReaderAccount).
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
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
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
	// clustered. ns, opts, clustered and the TLS of config change only under
	// routesLock as well, so a holder of routesLock reads them without
	// nsLock; opts is used under routesLock only.
	nsLock    sync.RWMutex
	ns        *natsserver.Server
	opts      *natsserver.Options
	clustered bool

	// registerLock serializes the limit check and the registration of a
	// connection in its account (clientAuth.Check).
	registerLock sync.Mutex

	// apiLock guards apiServer and apiLeader: the embedded server and the
	// meta leader whose JetStream API answered (JetStreamReady).
	apiLock   sync.Mutex
	apiServer *natsserver.Server
	apiLeader string

	l sync.Mutex
	// clients are the authenticated connections by NATS client ID, with the
	// credentials and grant they were admitted with, for Revalidate.
	clients map[uint64]*client

	// readerLock guards the reader accounts made on the embedded server
	// readersOf (account.go); a restarted server starts without them.
	// readers are the live ones by name, readerKeys the live one of each
	// ReaderAccountName; readerSeq is the last generation given to a name.
	// retiredReaders counts the retired ones, which a reload of the options
	// drops. reloading is set while the options are reloaded (reload), and
	// compactedAt is when the retired ones were last dropped.
	readerLock     sync.Mutex
	readers        map[string]*reader
	readerKeys     map[string]string
	readersOf      *natsserver.Server
	readerSeq      uint64
	retiredReaders int
	reloading      bool
	compactedAt    time.Time

	// The limits of the fetches of a reader account (leafwatch.go), and
	// the retirement and compaction of the reader accounts (account.go),
	// guarded by readerLock; fields so that tests can shorten them.
	maxPendingResponses int
	responseThreshold   time.Duration
	leafBanTime         time.Duration
	retireGrace         time.Duration
	compactMin          int
	compactInterval     time.Duration

	// trust remembers the routes with their TLS state, for ReloadTLS.
	trust routeTrust

	// stopCh is closed by Shutdown.
	stopCh   chan struct{}
	stopOnce sync.Once
}

type client struct {
	creds Credentials
	grant *Grant

	// at is when the connection was authenticated.
	at time.Time

	// acc is the account it was registered in.
	acc *natsserver.Account
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

		maxPendingResponses: ReaderMaxPendingResponses,
		responseThreshold:   ReaderResponseThreshold,
		leafBanTime:         ReaderBanTime,
		retireGrace:         readerRetireGrace,
		compactMin:          config.ReaderCompactThreshold,
		compactInterval:     readerCompactInterval,
		stopCh:              make(chan struct{}),
	}
	ns, opts, err := s.start(config.Clustered, nil)
	if err != nil {
		return nil, err
	}
	s.ns, s.opts, s.clustered = ns, opts, config.Clustered
	go s.leafWatchLoop()
	go s.readerSweepLoop()
	return s, nil
}

// start creates and starts an embedded server.
func (s *Server) start(clustered bool, routes []string) (*natsserver.Server, *natsserver.Options, error) {
	config := s.config
	accounts, err := newAccounts()
	if err != nil {
		return nil, nil, err
	}
	auth := &clientAuth{s: s}
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
		Accounts:                   accounts,
		CustomClientAuthentication: auth,
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
			opts.Cluster.TLSConfig = s.trust.wrap(config.ClusterTLS)
			opts.Cluster.TLSTimeout = 5
		}
		opts.Routes = s.routeURLs(routes)
	}
	if config.LeafNodes.Enabled {
		// The leaf nodes authenticate as clients do, through the custom
		// authentication (no leaf users are configured); the TLS is that of
		// the client connections.
		opts.LeafNode = natsserver.LeafNodeOpts{
			Host: config.BindAddr,
			Port: config.LeafNodes.Port,
		}
		if config.ClientTLS != nil {
			opts.LeafNode.TLSConfig = config.ClientTLS
			opts.LeafNode.TLSTimeout = 5
		}
	}

	ns, err := natsserver.NewServer(opts)
	if err != nil {
		return nil, nil, fmt.Errorf("bus: %w", err)
	}
	// Set before the server accepts its first connection.
	auth.ns = ns
	ns.SetLoggerV2(&natsLogger{logger: s.logger}, false, false, false)
	go ns.Start()
	addr := net.JoinHostPort(config.BindAddr, strconv.Itoa(config.Port))
	if !ns.ReadyForConnections(startTimeout) {
		ns.Shutdown()
		return nil, nil, fmt.Errorf("bus: the NATS server did not start listening on %s within %s", addr, startTimeout)
	}
	s.logger.Info("system bus started", "server", config.ServerName, "addr", addr,
		"clustered", clustered, "cluster_port", config.ClusterPort, "tls", config.ClientTLS != nil,
		"leafnode_port", leafPort(config))
	return ns, opts, nil
}

// leafPort is the port of the leaf node listener, 0 when it is off.
func leafPort(c *Config) int {
	if !c.LeafNodes.Enabled {
		return 0
	}
	return c.LeafNodes.Port
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
	s.stopOnce.Do(func() { close(s.stopCh) })
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
	// The routes that are gone are forgotten by the TLS checks.
	s.nsLock.RLock()
	if s.clustered {
		s.trust.prune(s.liveRoutes())
	}
	s.nsLock.RUnlock()
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

	opts := s.opts.Clone()
	opts.Routes = s.routeURLs(addrs)
	if err := s.reload(s.ns, opts); err != nil {
		return fmt.Errorf("bus: reloading the routes: %w", err)
	}
	s.opts = opts
	s.routes = addrs
	s.logger.Debug("routes updated", "routes", addrs)
	return nil
}

// reload applies opts to the embedded server ns. The caller holds
// routesLock, and neither nsLock nor readerLock: nats-server authenticates
// connections again during a reload (clientAuth.Check), and the authorizer
// may ask for a reader account, which takes both. Such a call is refused
// while the reload runs: nats-server deletes the accounts made at runtime and
// resets the exports of the bus account to its configuration, which could
// leave an account made meanwhile half way.
//
// After the reload the retired reader accounts are gone, and so are the
// connections of the reader accounts: nats-server closes the client
// connections the custom authentication registered (it does not know their
// users) and keeps the leaf nodes in their deleted accounts, where they reach
// none of their imports; closeStaleReaders closes those.
func (s *Server) reload(ns *natsserver.Server, opts *natsserver.Options) error {
	s.readerLock.Lock()
	s.reloading = true
	s.readerLock.Unlock()
	err := ns.ReloadOptions(opts)
	s.readerLock.Lock()
	s.reloading = false
	if err == nil && s.readersOf == ns {
		s.retiredReaders = 0
	}
	s.readerLock.Unlock()
	if err != nil {
		return err
	}
	s.closeStaleReaders(ns)
	return nil
}

// ErrTLSNeedsRestart is returned by ReloadTLS for a change that turns the
// TLS of the bus on or off: that applies only after a restart.
var ErrTLSNeedsRestart = errors.New("bus: turning TLS on or off needs a restart")

// ReloadTLS replaces the TLS configurations of the listeners (a reloaded
// agent TLS configuration: a new CA, certificate or setting). The
// connections made from now on use them. The open connections are checked
// against them, and those a new handshake would refuse (a certificate of a
// distrusted or removed CA) are closed; the others keep the TLS they were
// made with. A bus that becomes clustered later starts with them.
func (s *Server) ReloadTLS(clientTLS, clusterTLS *tls.Config) error {
	s.routesLock.Lock()
	defer s.routesLock.Unlock()

	if (clientTLS == nil) != (s.config.ClientTLS == nil) || (clusterTLS == nil) != (s.config.ClusterTLS == nil) {
		return ErrTLSNeedsRestart
	}
	if clientTLS == nil {
		return nil
	}
	opts := s.opts.Clone()
	opts.TLSConfig = clientTLS
	if s.clustered {
		opts.Cluster.TLSConfig = s.trust.wrap(clusterTLS)
	}
	if s.config.LeafNodes.Enabled {
		opts.LeafNode.TLSConfig = clientTLS
	}
	if err := s.reload(s.ns, opts); err != nil {
		return fmt.Errorf("bus: reloading the TLS configuration: %w", err)
	}
	s.opts = opts
	s.nsLock.Lock()
	s.config.ClientTLS, s.config.ClusterTLS = clientTLS, clusterTLS
	s.nsLock.Unlock()
	s.logger.Info("TLS configuration reloaded")
	if err := s.recheckTrust(clientTLS, clusterTLS); err != nil {
		return fmt.Errorf("bus: checking the open connections against the reloaded TLS configuration: %w", err)
	}
	return nil
}

// tlsEnabled reports whether the listeners use TLS.
func (s *Server) tlsEnabled() bool {
	s.nsLock.RLock()
	defer s.nsLock.RUnlock()
	return s.config.ClientTLS != nil
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
		nats.CustomInboxPrefix(InternalInboxPrefix),
	}, options...)
	return nats.Connect("", options...)
}

// apiCheckTimeout bounds the JetStream API request of JetStreamReady.
const apiCheckTimeout = 250 * time.Millisecond

// JetStreamReady reports whether JetStream can be used: it runs and, when
// clustered, has a meta leader that answers the JetStream API.
//
// A meta leader shows in Jsz as soon as it is elected, but it answers API
// requests only once it has processed its first entries; a request in
// between gets no answer from any server (review finding MUK-23;
// nats-server 2.15.1 and later no longer report a smaller cluster in that
// gap). So, once per meta leader and embedded server, the readiness is
// confirmed by an API request ($JS.API.INFO) that must be answered. A later
// election of the same server is not checked again; the callers retry.
func (s *Server) JetStreamReady() bool {
	ns := s.server()
	jsz, err := ns.Jsz(&natsserver.JSzOptions{})
	if err != nil || jsz.Disabled {
		return false
	}
	if jsz.Meta == nil {
		return true
	}
	leader := jsz.Meta.Leader
	if leader == "" {
		return false
	}

	s.apiLock.Lock()
	defer s.apiLock.Unlock()
	if s.apiServer == ns && s.apiLeader == leader {
		return true
	}
	if !s.jetStreamAPIAnswers() {
		return false
	}
	s.apiServer, s.apiLeader = ns, leader
	return true
}

// jetStreamAPIAnswers sends $JS.API.INFO over an in-process connection and
// reports whether an answer came within apiCheckTimeout.
func (s *Server) jetStreamAPIAnswers() bool {
	nc, err := s.Connect(nats.Name("norca-server-jetstream-check"), nats.MaxReconnects(0))
	if err != nil {
		return false
	}
	defer nc.Close()
	_, err = nc.Request("$JS.API.INFO", nil, apiCheckTimeout)
	return err == nil
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

// Errorf logs an error of the embedded server. A reader account exists only
// on the server its leaf node attached to; the other servers of the region
// get its subscriptions over their routes and report each as one of an
// unknown account. They ignore it, which is right: nothing is published in
// that account on them. Logged at debug level, or a leaf with many
// subscriptions would fill the logs of every other server with errors.
func (l *natsLogger) Errorf(format string, v ...any) {
	msg := fmt.Sprintf(format, v...)
	if strings.Contains(msg, `Unknown account "`+ReaderAccountPrefix) {
		l.logger.Debug(msg)
		return
	}
	l.logger.Error(msg)
}
func (l *natsLogger) Debugf(format string, v ...any) { l.logger.Debug(fmt.Sprintf(format, v...)) }
func (l *natsLogger) Tracef(format string, v ...any) { l.logger.Trace(fmt.Sprintf(format, v...)) }
