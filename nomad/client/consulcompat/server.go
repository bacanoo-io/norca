// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Package consulcompat is an HTTP API on the client that implements the
// catalog, health and agent service endpoints of the public, documented
// Consul HTTP API on top of the native service catalog, so tools that only
// know Consul (Traefik's consulcatalog provider, Prometheus'
// consul_sd_configs, consul-template's service functions, self-registering
// frameworks) work without a Consul agent.
//
// Supported endpoints: /v1/agent/self, /v1/status/leader, /v1/status/peers,
// /v1/catalog/datacenters, /v1/catalog/services, /v1/catalog/service/:name,
// /v1/catalog/nodes, /v1/catalog/node/:node, /v1/health/service/:name,
// /v1/health/checks/:name, /v1/health/state/:state and
// /v1/health/connect/:name (always empty); and for the services registered at
// the agent, which are external services owned by the node:
// PUT /v1/agent/service/register, PUT /v1/agent/service/deregister/:id,
// PUT /v1/agent/check/pass|warn|fail/:id, PUT /v1/agent/check/update/:id,
// GET /v1/agent/services, /v1/agent/service/:id and /v1/agent/checks. Every
// other endpoint answers 501 Not Implemented.
//
// Requests are served with the Nomad ACL token they carry (X-Consul-Token,
// Authorization: Bearer or ?token=), or the configured anonymous token, so
// the Nomad permission rules apply. Blocking queries (?index, ?wait) map to
// Nomad blocking queries.
package consulcompat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/nomad/nomad/structs"
)

const (
	// maxWait is the longest wait of a blocking query, as in Consul.
	maxWait = 10 * time.Minute

	// defaultWait is the wait of a blocking query without ?wait.
	defaultWait = 5 * time.Minute

	// namespaceSeparator separates the namespace prefix from the name of
	// the services of the namespaces other than the first one.
	namespaceSeparator = "--"
)

// Backend gives the API access to the servers and the local node.
type Backend interface {
	// RPC performs an RPC to the servers.
	RPC(method string, args, reply any) error

	// NodeSecret is the secret of the local node; it is used to read the
	// details (name, address, meta) of the nodes running service instances.
	NodeSecret() string

	// LocalNode returns the local node.
	LocalNode() *structs.Node

	// Region returns the region of the client.
	Region() string

	// Topology returns the topology of the client.
	Topology() structs.Topology

	// LocalServices returns the external services owned by the local node,
	// nil while the client is starting.
	LocalServices() LocalServices
}

// Config of the API.
type Config struct {
	// Bind is the host:port to listen on.
	Bind string

	// DatacenterIsRegion shows Nomad regions as Consul datacenters;
	// otherwise the Nomad datacenters of the local region.
	DatacenterIsRegion bool

	// Namespaces shown; the first one without a prefix.
	Namespaces []string

	// AnonymousToken is used for requests without a token.
	AnonymousToken string
}

// Server is the Consul-compatible API.
type Server struct {
	logger  hclog.Logger
	cfg     Config
	backend Backend
	nodes   *nodeCache

	ln  net.Listener
	srv *http.Server
}

// New returns the API. It does not listen until Start is called.
func New(logger hclog.Logger, cfg Config, backend Backend) *Server {
	if len(cfg.Namespaces) == 0 {
		cfg.Namespaces = []string{structs.DefaultNamespace}
	}
	return &Server{
		logger:  logger.Named("consul_compat"),
		cfg:     cfg,
		backend: backend,
		nodes:   newNodeCache(backend),
	}
}

// Start listens on the configured address and serves the API.
func (s *Server) Start() error {
	ln, err := net.Listen("tcp", s.cfg.Bind)
	if err != nil {
		return fmt.Errorf("failed to listen on %s: %w", s.cfg.Bind, err)
	}
	s.ln = ln
	s.srv = &http.Server{Handler: s, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := s.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.logger.Error("consul-compatible API stopped", "error", err)
		}
	}()
	s.logger.Info("consul-compatible API started", "address", ln.Addr())
	return nil
}

// Addr returns the address the API listens on.
func (s *Server) Addr() string {
	if s.ln == nil {
		return ""
	}
	return s.ln.Addr().String()
}

// Shutdown stops the API.
func (s *Server) Shutdown() {
	if s.srv == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = s.srv.Shutdown(ctx)
}

// handlerFunc serves one endpoint; it returns the object to encode as JSON.
type handlerFunc func(w http.ResponseWriter, r *request, arg string) (any, error)

// route maps a request path to its handler and its path argument.
func (s *Server) route(path string) (handlerFunc, string, bool) {
	exact := map[string]handlerFunc{
		"/v1/agent/self":          s.agentSelf,
		"/v1/status/leader":       s.statusLeader,
		"/v1/status/peers":        s.statusPeers,
		"/v1/catalog/datacenters": s.catalogDatacenters,
		"/v1/catalog/services":    s.catalogServices,
		"/v1/catalog/nodes":       s.catalogNodes,
		"/v1/agent/services":      s.agentServices,
		"/v1/agent/checks":        s.agentChecks,
	}
	if h, ok := exact[path]; ok {
		return h, "", true
	}

	prefixed := []struct {
		prefix string
		h      handlerFunc
	}{
		{"/v1/catalog/service/", s.catalogService},
		{"/v1/catalog/node/", s.catalogNode},
		{"/v1/health/service/", s.healthService},
		{"/v1/health/checks/", s.healthChecks},
		{"/v1/health/state/", s.healthState},
		{"/v1/health/connect/", s.healthConnect},
		{"/v1/agent/service/", s.agentService1},
	}
	for _, p := range prefixed {
		if arg, ok := strings.CutPrefix(path, p.prefix); ok && arg != "" {
			return p.h, arg, true
		}
	}
	return nil, "", false
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if h, arg, ok := s.writeRoute(req.URL.Path); ok {
		s.serveWrite(w, req, h, arg)
		return
	}

	h, arg, ok := s.route(req.URL.Path)
	if !ok {
		s.logger.Debug("unsupported endpoint", "method", req.Method, "path", req.URL.Path)
		http.Error(w, fmt.Sprintf(
			"Not Implemented: %s %s is not supported by the Consul-compatible API of Norca; "+
				"it serves the catalog, health and status endpoints and the agent service endpoints only",
			req.Method, req.URL.Path),
			http.StatusNotImplemented)
		return
	}
	if req.Method != http.MethodGet && req.Method != http.MethodHead {
		w.Header().Set("Allow", "GET")
		http.Error(w, fmt.Sprintf("method %s not allowed", req.Method), http.StatusMethodNotAllowed)
		return
	}

	r, err := s.parseRequest(req)
	if err != nil {
		s.writeError(w, err)
		return
	}

	out, err := h(w, r, arg)
	if err != nil {
		s.writeError(w, err)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	if _, pretty := req.URL.Query()["pretty"]; pretty {
		enc.SetIndent("", "    ")
	}
	if err := enc.Encode(out); err != nil {
		s.logger.Debug("failed to encode response", "error", err)
	}
}

// serveWrite serves a write endpoint: PUT only, and an empty body on success.
func (s *Server) serveWrite(w http.ResponseWriter, req *http.Request, h handlerFunc, arg string) {
	if req.Method != http.MethodPut {
		w.Header().Set("Allow", "PUT")
		http.Error(w, fmt.Sprintf("method %s not allowed", req.Method), http.StatusMethodNotAllowed)
		return
	}
	r, err := s.parseRequest(req)
	if err != nil {
		s.writeError(w, err)
		return
	}
	out, err := h(w, r, arg)
	if err != nil {
		s.writeError(w, err)
		return
	}
	if out != nil {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	}
}

// codedError is an error with an HTTP status.
type codedError struct {
	code int
	msg  string
}

func (e *codedError) Error() string { return e.msg }

func badRequest(format string, args ...any) error {
	return &codedError{http.StatusBadRequest, fmt.Sprintf(format, args...)}
}

// writeError writes err with the status Consul would use.
func (s *Server) writeError(w http.ResponseWriter, err error) {
	var coded *codedError
	msg := err.Error()
	code := http.StatusInternalServerError
	switch {
	case errors.As(err, &coded):
		code, msg = coded.code, coded.msg
	case strings.Contains(msg, "RPC Error:: "):
		if c, m, ok := structs.CodeFromRPCCodedErr(errors.New(msg[strings.Index(msg, "RPC Error:: "):])); ok {
			code, msg = c, m
		}
	case strings.Contains(msg, structs.ErrPermissionDenied.Error()):
		code, msg = http.StatusForbidden, "Permission denied"
	case strings.Contains(msg, structs.ErrTokenNotFound.Error()):
		code, msg = http.StatusForbidden, "ACL not found"
	case strings.Contains(msg, "No path to region"):
		msg = "No path to datacenter"
	}
	http.Error(w, msg, code)
}

// request is a parsed Consul request.
type request struct {
	http *http.Request

	token string

	// region is the Nomad region queried
	region string

	// datacenter filters instances by Nomad datacenter, when Consul
	// datacenters are Nomad datacenters
	datacenter string

	// dc is the Consul datacenter reported in the response
	dc string

	index uint64
	wait  time.Duration
	stale bool

	filter   string
	nodeMeta map[string]string
	tags     []string
	passing  bool
	near     bool
}

// parseRequest parses the query parameters and headers common to the
// endpoints.
func (s *Server) parseRequest(req *http.Request) (*request, error) {
	q := req.URL.Query()
	r := &request{
		http:   req,
		region: s.backend.Region(),
		filter: q.Get("filter"),
		tags:   q["tag"],
	}

	// token: header, bearer, then query parameter
	r.token = req.Header.Get("X-Consul-Token")
	if r.token == "" {
		if auth := req.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
			r.token = strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
		}
	}
	if r.token == "" {
		r.token = q.Get("token")
	}
	if r.token == "" {
		r.token = s.cfg.AnonymousToken
	}

	// datacenter
	dc := q.Get("dc")
	if s.cfg.DatacenterIsRegion {
		if dc != "" {
			r.region = dc
		}
		r.dc = r.region
	} else {
		r.dc = s.backend.Topology().Datacenter
		if dc != "" {
			r.datacenter = dc
			r.dc = dc
		}
	}

	// blocking query
	if raw := q.Get("index"); raw != "" {
		index, err := strconv.ParseUint(raw, 10, 64)
		if err != nil {
			return nil, badRequest("Invalid index: %v", err)
		}
		r.index = index
		r.wait = defaultWait
	}
	if raw := q.Get("wait"); raw != "" {
		wait, err := time.ParseDuration(raw)
		if err != nil {
			return nil, badRequest("Invalid wait time: %v", err)
		}
		r.wait = wait
	}
	if r.wait > maxWait {
		r.wait = maxWait
	}
	_, r.stale = q["stale"]

	// filters
	_, r.passing = q["passing"]
	if raw := q.Get("passing"); raw != "" {
		passing, err := strconv.ParseBool(raw)
		if err != nil {
			return nil, badRequest("Invalid value for ?passing: %v", err)
		}
		r.passing = passing
	}
	for _, kv := range q["node-meta"] {
		key, value, _ := strings.Cut(kv, ":")
		if r.nodeMeta == nil {
			r.nodeMeta = make(map[string]string)
		}
		r.nodeMeta[key] = value
	}
	switch near := q.Get("near"); near {
	case "":
	case "_agent", "_self":
		r.near = true
	default:
		// sorting by the round trip time to an arbitrary node is not
		// supported; the answer is unsorted, as Consul does for unknown
		// nodes
	}
	return r, nil
}

// queryOptions returns the Nomad query options of r for namespace.
func (r *request) queryOptions(namespace string, blocking bool) structs.QueryOptions {
	opts := structs.QueryOptions{
		Region:     r.region,
		Namespace:  namespace,
		AuthToken:  r.token,
		AllowStale: r.stale,
	}
	if blocking {
		opts.MinQueryIndex = r.index
		opts.MaxQueryTime = r.wait
	}
	return opts
}

// setMeta sets the Consul query meta headers.
func setMeta(w http.ResponseWriter, r *request, meta structs.QueryMeta) {
	index := meta.Index
	if index == 0 {
		index = 1
	}
	w.Header().Set("X-Consul-Index", strconv.FormatUint(index, 10))
	w.Header().Set("X-Consul-KnownLeader", strconv.FormatBool(meta.KnownLeader))
	w.Header().Set("X-Consul-LastContact", strconv.FormatInt(meta.LastContact.Milliseconds(), 10))
	consistency := "leader"
	if r.stale {
		consistency = "stale"
	}
	w.Header().Set("X-Consul-Effective-Consistency", consistency)
}

// consulName returns the Consul name of a Nomad service.
func (s *Server) consulName(namespace, name string) string {
	if namespace == s.cfg.Namespaces[0] {
		return name
	}
	return namespace + namespaceSeparator + name
}

// nomadName returns the Nomad namespace and name of a Consul service name.
func (s *Server) nomadName(consulName string) (string, string) {
	for _, ns := range s.cfg.Namespaces[1:] {
		if name, ok := strings.CutPrefix(consulName, ns+namespaceSeparator); ok && name != "" {
			return ns, name
		}
	}
	return s.cfg.Namespaces[0], consulName
}
