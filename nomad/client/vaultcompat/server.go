// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Package vaultcompat is the Vault-compatible read endpoint of the client
// (docs/design/secrets-plan.md, "Vault compatibility"). When the agent has
// no Vault integration, tasks with a vault block get a token of this
// endpoint instead of a Vault token (VAULT_TOKEN, secrets/vault_token) and
// VAULT_ADDR points to it, so that applications using a Vault SDK read their
// KV secrets from the built-in secret store.
//
// The endpoint is a small subset of the public Vault HTTP API, written for
// Norca (no code of Vault or of uBixVault's HTTP layer):
//
//   - GET /v1/<mount>/data/<path> (KV v2, ?version=n), GET /v1/<mount>/<path>
//     (KV v1) and GET /v1/<mount>/metadata/<path> (KV v2 metadata), for the
//     mounts of the client's vault_kv rules
//   - GET /v1/auth/token/lookup-self, POST|PUT /v1/auth/token/renew-self
//   - GET /v1/sys/health
//
// Everything else answers 501 (405 for writes to KV paths) with an error in
// Vault's {"errors": [...]} shape saying that it is not supported by the
// built-in store. Reads resolve like every Vault-style lookup (explicit
// provider, client routes, defaults, fallback to the built-in store) and are
// made with the task's workload identity, so the read-secret policies and
// secret_access grants decide, never Vault policies; the servers audit the
// reads of the built-in store (and denials), the client records the reads of
// other providers.
//
// Binding: the endpoint listens on 127.0.0.1 for tasks in the host network
// and, once a task in bridge networking mode asks for a token, on the
// gateway address of the bridge network (the first address of the client's
// bridge subnet), which the tasks' network namespaces reach. It never
// listens on other addresses. The tokens of tasks in bridge mode are only
// accepted from their allocation's address. TLS is not offered: the traffic
// never leaves the host.
package vaultcompat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/go-hclog"
	"golang.org/x/time/rate"

	"github.com/hashicorp/nomad/client/secrets"
	"github.com/hashicorp/nomad/helper/uuid"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/nomad/version"
)

// defaultBridgeSubnet is the default subnet of the client's bridge network.
const defaultBridgeSubnet = "172.26.64.0/20"

// supportedText lists what the endpoint serves, for its errors.
const supportedText = "the Vault-compatible endpoint of Norca's built-in secret store serves KV v1/v2 reads, " +
	"auth/token/lookup-self, auth/token/renew-self and sys/health only"

// AllocInfo reports about a task of an allocation of the client: the
// allocation, whether the task still runs (or is starting), and the
// allocation's address in the bridge network ("" for other networking
// modes).
type AllocInfo func(allocID, task string) (alloc *structs.Allocation, live bool, bridgeIP string)

// Config configures the endpoint.
type Config struct {
	Logger hclog.Logger

	// Reader serves the reads.
	Reader *secrets.VaultReader

	// NodeSecret is the secret of the node; the token key is derived
	// from it.
	NodeSecret string

	// Port is the TCP port; 0 picks a free one (tests).
	Port int

	// Bridge enables the listener on the bridge gateway, BridgeSubnet is
	// the client's bridge subnet ("" is the default).
	Bridge       bool
	BridgeSubnet string

	// TokenTTL, RateLimit and RateBurst are the token time to live and the
	// request rate limit per token.
	TokenTTL  time.Duration
	RateLimit float64
	RateBurst int

	// AllocInfo reports about the tasks of the client; nil treats every
	// task as running in the host network (tests).
	AllocInfo AllocInfo
}

// Server is the endpoint.
type Server struct {
	logger hclog.Logger
	cfg    Config
	tokens *tokenStore

	// anonymous limits the requests without a valid token.
	anonymous *rate.Limiter

	lock     sync.Mutex
	ln       net.Listener
	bridgeLn net.Listener
	servers  []*http.Server
	port     int
}

// New returns the endpoint; it listens once Start is called.
func New(cfg Config) *Server {
	logger := cfg.Logger
	if logger == nil {
		logger = hclog.NewNullLogger()
	}
	if cfg.TokenTTL <= 0 {
		cfg.TokenTTL = time.Hour
	}
	if cfg.RateLimit <= 0 {
		cfg.RateLimit = 20
	}
	if cfg.RateBurst <= 0 {
		cfg.RateBurst = 40
	}
	return &Server{
		logger:    logger.Named("vault_compat"),
		cfg:       cfg,
		tokens:    newTokenStore(cfg.NodeSecret, cfg.TokenTTL, cfg.RateLimit, cfg.RateBurst),
		anonymous: rate.NewLimiter(rate.Limit(cfg.RateLimit), cfg.RateBurst),
	}
}

// Start listens on 127.0.0.1, if the endpoint does not yet. The client
// starts the endpoint when the first task with a vault block asks for a
// token, so that agents without such tasks open no port.
func (s *Server) Start() error {
	s.lock.Lock()
	defer s.lock.Unlock()
	if s.ln != nil {
		return nil
	}
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(s.cfg.Port)))
	if err != nil {
		return fmt.Errorf("failed to listen on 127.0.0.1:%d: %w", s.cfg.Port, err)
	}
	s.ln = ln
	s.port = ln.Addr().(*net.TCPAddr).Port
	s.serve(ln)
	s.logger.Info("Vault-compatible endpoint of the built-in secret store started", "address", ln.Addr())
	return nil
}

// serve serves a listener; the lock must be held.
func (s *Server) serve(ln net.Listener) {
	srv := &http.Server{Handler: s, ReadHeaderTimeout: 10 * time.Second}
	s.servers = append(s.servers, srv)
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.logger.Error("Vault-compatible endpoint stopped", "address", ln.Addr(), "error", err)
		}
	}()
}

// Addr returns the loopback address of the endpoint.
func (s *Server) Addr() string {
	s.lock.Lock()
	defer s.lock.Unlock()
	if s.ln == nil {
		return ""
	}
	return s.ln.Addr().String()
}

// Shutdown stops the endpoint. The live tokens are forgotten.
func (s *Server) Shutdown() {
	s.lock.Lock()
	servers := s.servers
	s.servers = nil
	s.lock.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, srv := range servers {
		_ = srv.Shutdown(ctx)
	}
}

// bridgeGateway returns the gateway address of the bridge network.
func (s *Server) bridgeGateway() (netip.Addr, error) {
	subnet := s.cfg.BridgeSubnet
	if subnet == "" {
		subnet = defaultBridgeSubnet
	}
	prefix, err := netip.ParsePrefix(subnet)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("invalid bridge subnet %q: %w", subnet, err)
	}
	return prefix.Masked().Addr().Next(), nil
}

// ensureBridge listens on the bridge gateway, if it does not yet. The bridge
// exists once the network of an allocation in bridge mode was created.
func (s *Server) ensureBridge() error {
	if !s.cfg.Bridge {
		return errors.New("the Vault-compatible endpoint does not listen on the bridge network (vault_compat bridge = false)")
	}
	gw, err := s.bridgeGateway()
	if err != nil {
		return err
	}
	s.lock.Lock()
	defer s.lock.Unlock()
	if s.bridgeLn != nil {
		return nil
	}
	if s.ln == nil {
		return errors.New("the Vault-compatible endpoint is not running")
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(gw.String(), strconv.Itoa(s.port)))
	if err != nil {
		return fmt.Errorf("failed to listen on the bridge gateway %s: %w", gw, err)
	}
	s.bridgeLn = ln
	s.serve(ln)
	s.logger.Info("Vault-compatible endpoint listens on the bridge network", "address", ln.Addr())
	return nil
}

// usesBridge reports whether the task group of the allocation uses bridge
// networking.
func usesBridge(alloc *structs.Allocation) bool {
	if alloc == nil || alloc.Job == nil {
		return false
	}
	tg := alloc.Job.LookupTaskGroup(alloc.TaskGroup)
	return tg != nil && len(tg.Networks) > 0 && tg.Networks[0].Mode == "bridge"
}

// VaultAddr returns the address of the endpoint for a task of the
// allocation: on the bridge gateway for bridge networking, else on the
// loopback address.
func (s *Server) VaultAddr(alloc *structs.Allocation) string {
	s.lock.Lock()
	port := s.port
	s.lock.Unlock()
	host := "127.0.0.1"
	if usesBridge(alloc) && s.cfg.Bridge {
		if gw, err := s.bridgeGateway(); err == nil {
			host = gw.String()
		}
	}
	return "http://" + net.JoinHostPort(host, strconv.Itoa(port))
}

// vaultResponse is the envelope of Vault's responses.
type vaultResponse struct {
	RequestID     string    `json:"request_id"`
	LeaseID       string    `json:"lease_id"`
	Renewable     bool      `json:"renewable"`
	LeaseDuration int       `json:"lease_duration"`
	Data          any       `json:"data"`
	WrapInfo      any       `json:"wrap_info"`
	Warnings      []string  `json:"warnings"`
	Auth          *authInfo `json:"auth"`
	MountType     string    `json:"mount_type,omitempty"`
}

type authInfo struct {
	ClientToken   string            `json:"client_token"`
	Accessor      string            `json:"accessor"`
	Policies      []string          `json:"policies"`
	TokenPolicies []string          `json:"token_policies"`
	Metadata      map[string]string `json:"metadata"`
	LeaseDuration int               `json:"lease_duration"`
	Renewable     bool              `json:"renewable"`
	EntityID      string            `json:"entity_id"`
	TokenType     string            `json:"token_type"`
	Orphan        bool              `json:"orphan"`
}

// httpError is an error with a status, written in Vault's error shape.
type httpError struct {
	code int
	msgs []string
}

func (e *httpError) Error() string { return strings.Join(e.msgs, "; ") }

func errorf(code int, format string, args ...any) *httpError {
	return &httpError{code: code, msgs: []string{fmt.Sprintf(format, args...)}}
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	out, err := s.handle(req)
	if err != nil {
		s.writeError(w, req, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if err := json.NewEncoder(w).Encode(out); err != nil {
		s.logger.Debug("failed to encode response", "error", err)
	}
}

func (s *Server) writeError(w http.ResponseWriter, req *http.Request, err error) {
	var herr *httpError
	if !errors.As(err, &herr) {
		herr = s.providerError(err)
	}
	if herr.code == http.StatusMethodNotAllowed {
		w.Header().Set("Allow", "GET")
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(herr.code)
	msgs := herr.msgs
	if msgs == nil {
		msgs = []string{}
	}
	_ = json.NewEncoder(w).Encode(map[string][]string{"errors": msgs})
}

// providerError maps an error of a lookup to the status Vault would use.
func (s *Server) providerError(err error) *httpError {
	switch {
	case secrets.IsUnsupported(err):
		return errorf(http.StatusNotImplemented, "%v", err)
	}
	switch secrets.KindOf(err) {
	case secrets.KindDenied:
		return errorf(http.StatusForbidden, "permission denied")
	case secrets.KindNotFound:
		// Vault answers a missing secret with 404 and no errors.
		return &httpError{code: http.StatusNotFound}
	case secrets.KindUnavailable:
		return errorf(http.StatusServiceUnavailable, "%v", err)
	}
	return errorf(http.StatusBadRequest, "%v", err)
}

// handle serves a request and returns the response to encode.
func (s *Server) handle(req *http.Request) (any, error) {
	path, ok := strings.CutPrefix(req.URL.Path, "/v1/")
	if !ok || path == "" {
		return nil, errorf(http.StatusNotFound, "%s is not a Vault API path; %s", req.URL.Path, supportedText)
	}
	path = strings.TrimSuffix(path, "/")

	switch path {
	case "sys/health":
		if !s.anonymous.Allow() {
			return nil, errorf(http.StatusTooManyRequests, "rate limit exceeded")
		}
		if req.Method != http.MethodGet && req.Method != http.MethodHead {
			return nil, errorf(http.StatusMethodNotAllowed, "method %s is not supported for sys/health", req.Method)
		}
		return s.health(), nil
	case "auth/token/lookup-self":
		t, err := s.authenticate(req, path)
		if err != nil {
			return nil, err
		}
		if req.Method != http.MethodGet && req.Method != http.MethodPost && req.Method != http.MethodPut {
			return nil, errorf(http.StatusMethodNotAllowed, "method %s is not supported for %s", req.Method, path)
		}
		return s.lookupSelf(req, t), nil
	case "auth/token/renew-self":
		t, err := s.authenticate(req, path)
		if err != nil {
			return nil, err
		}
		if req.Method != http.MethodPost && req.Method != http.MethodPut {
			return nil, errorf(http.StatusMethodNotAllowed, "method %s is not supported for %s", req.Method, path)
		}
		return s.renewSelf(req, t)
	}

	if strings.HasPrefix(path, "sys/") || strings.HasPrefix(path, "auth/") ||
		strings.HasPrefix(path, "identity/") || strings.HasPrefix(path, "cubbyhole/") {
		if !s.anonymous.Allow() {
			return nil, errorf(http.StatusTooManyRequests, "rate limit exceeded")
		}
		return nil, errorf(http.StatusNotImplemented, "%s /v1/%s is not supported by the built-in secret store: %s",
			req.Method, path, supportedText)
	}

	// KV reads.
	t, err := s.authenticate(req, path)
	if err != nil {
		return nil, err
	}
	q := req.URL.Query()
	if req.Method == "LIST" || q.Get("list") == "true" {
		return nil, errorf(http.StatusNotImplemented,
			"listing /v1/%s is not supported by the built-in secret store: %s", path, supportedText)
	}
	if req.Method != http.MethodGet {
		return nil, errorf(http.StatusMethodNotAllowed,
			"%s /v1/%s is not supported: the Vault-compatible endpoint of the built-in secret store is read-only",
			req.Method, path)
	}
	var version uint64
	if raw := q.Get("version"); raw != "" {
		if version, err = strconv.ParseUint(raw, 10, 64); err != nil {
			return nil, errorf(http.StatusBadRequest, "invalid version %q", raw)
		}
	}
	return s.read(req.Context(), t, path, version)
}

// authenticate returns the live token of the request.
func (s *Server) authenticate(req *http.Request, path string) (*token, error) {
	tok := req.Header.Get("X-Vault-Token")
	if tok == "" {
		if auth := req.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
			tok = strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
		}
	}
	remote := remoteIP(req)
	deny := func(reason string, err error) error {
		// Logged without the token; the servers audit the denials of
		// reads with a valid token.
		s.logger.Warn("request denied", "path", path, "remote", remote, "reason", reason)
		return err
	}
	if tok == "" {
		if !s.anonymous.Allow() {
			return nil, errorf(http.StatusTooManyRequests, "rate limit exceeded")
		}
		return nil, deny("missing token", errorf(http.StatusForbidden, "permission denied: missing client token"))
	}
	t, err := s.tokens.lookup(tok)
	if err != nil {
		if !s.anonymous.Allow() {
			return nil, errorf(http.StatusTooManyRequests, "rate limit exceeded")
		}
		return nil, deny("invalid token", errorf(http.StatusForbidden, "%v", err))
	}
	if !t.limiter.Allow() {
		return nil, errorf(http.StatusTooManyRequests, "rate limit exceeded")
	}
	if s.cfg.AllocInfo != nil {
		_, live, bridgeIP := s.cfg.AllocInfo(t.info.AllocID, t.info.Task)
		if !live {
			s.tokens.revoke(tok)
			return nil, deny("task not running", errorf(http.StatusForbidden, "%v", errTokenExpired))
		}
		if bridgeIP != "" && remote != bridgeIP {
			return nil, deny("token of another network namespace",
				errorf(http.StatusForbidden, "permission denied: the token can only be used by its task"))
		}
	}
	return t, nil
}

// remoteIP returns the address the request came from.
func remoteIP(req *http.Request) string {
	host, _, err := net.SplitHostPort(req.RemoteAddr)
	if err != nil {
		return req.RemoteAddr
	}
	if addr, err := netip.ParseAddr(host); err == nil {
		return addr.Unmap().String()
	}
	return host
}

// read serves a KV read.
func (s *Server) read(ctx context.Context, t *token, path string, version uint64) (any, error) {
	info := t.info
	vl, err := s.cfg.Reader.Resolve(secrets.LookupFor(info.Job, info.Group, "", path), version)
	if err != nil {
		return nil, err
	}
	kv, err := s.cfg.Reader.Read(ctx, vl, &secrets.Identity{
		Token:     info.Identity,
		Namespace: info.Namespace,
		JobID:     info.JobID,
		Group:     info.Group,
		Task:      info.Task,
		AllocID:   info.AllocID,
	})
	if err != nil {
		return nil, err
	}
	return &vaultResponse{
		RequestID:     uuid.Generate(),
		LeaseDuration: kv.LeaseDuration,
		Data:          kv.Data,
		MountType:     "kv",
	}, nil
}

func (s *Server) health() any {
	return map[string]any{
		"initialized":                  true,
		"sealed":                       false,
		"standby":                      false,
		"performance_standby":          false,
		"replication_performance_mode": "disabled",
		"replication_dr_mode":          "disabled",
		"server_time_utc":              time.Now().Unix(),
		"version":                      version.NorcaVersion + "+norca-builtin-store",
		"cluster_name":                 "norca-builtin-store",
		"cluster_id":                   "",
	}
}

// tokenMeta returns the metadata of a token.
func tokenMeta(info *TaskInfo) map[string]string {
	return map[string]string{
		"AllocationID": info.AllocID,
		"JobID":        info.JobID,
		"Namespace":    info.Namespace,
		"Task":         info.Task,
	}
}

func tokenPolicies(info *TaskInfo) []string {
	out := append([]string{"default"}, info.Policies...)
	sort.Strings(out[1:])
	return out
}

func (s *Server) lookupSelf(req *http.Request, t *token) any {
	tok := tokenFromRequest(req)
	now := time.Now()
	ttl := int(t.expires.Sub(now).Seconds())
	if ttl < 0 {
		ttl = 0
	}
	return &vaultResponse{
		RequestID: uuid.Generate(),
		Data: map[string]any{
			"accessor":         t.accessor,
			"creation_time":    t.created.Unix(),
			"creation_ttl":     int(s.cfg.TokenTTL.Seconds()),
			"display_name":     "norca-" + t.info.JobID + "-" + t.info.Task,
			"entity_id":        "",
			"expire_time":      t.expires.UTC().Format(time.RFC3339Nano),
			"explicit_max_ttl": 0,
			"id":               tok,
			"issue_time":       t.created.UTC().Format(time.RFC3339Nano),
			"meta":             tokenMeta(t.info),
			"num_uses":         0,
			"orphan":           true,
			"path":             "auth/token/create",
			"policies":         tokenPolicies(t.info),
			"renewable":        true,
			"ttl":              ttl,
			"type":             "service",
		},
	}
}

func tokenFromRequest(req *http.Request) string {
	if tok := req.Header.Get("X-Vault-Token"); tok != "" {
		return tok
	}
	return strings.TrimSpace(strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer "))
}

// renewSelf extends the token by the requested increment (seconds or a
// duration), at most the token TTL.
func (s *Server) renewSelf(req *http.Request, t *token) (any, error) {
	var body struct {
		Increment any `json:"increment"`
	}
	if req.Body != nil {
		data, err := io.ReadAll(io.LimitReader(req.Body, 64*1024))
		if err != nil {
			return nil, errorf(http.StatusBadRequest, "failed to read the request: %v", err)
		}
		if len(strings.TrimSpace(string(data))) > 0 {
			if err := json.Unmarshal(data, &body); err != nil {
				return nil, errorf(http.StatusBadRequest, "invalid request body: %v", err)
			}
		}
	}
	var increment time.Duration
	switch v := body.Increment.(type) {
	case nil:
	case float64:
		increment = time.Duration(v) * time.Second
	case string:
		if secs, err := strconv.Atoi(v); err == nil {
			increment = time.Duration(secs) * time.Second
		} else if d, err := time.ParseDuration(v); err == nil {
			increment = d
		} else {
			return nil, errorf(http.StatusBadRequest, "invalid increment %q", v)
		}
	default:
		return nil, errorf(http.StatusBadRequest, "invalid increment")
	}
	tok := tokenFromRequest(req)
	t, ttl, err := s.tokens.renewSelf(tok, increment)
	if err != nil {
		return nil, errorf(http.StatusForbidden, "%v", err)
	}
	policies := tokenPolicies(t.info)
	return &vaultResponse{
		RequestID: uuid.Generate(),
		Auth: &authInfo{
			ClientToken:   tok,
			Accessor:      t.accessor,
			Policies:      policies,
			TokenPolicies: policies,
			Metadata:      tokenMeta(t.info),
			LeaseDuration: int(ttl.Seconds()),
			Renewable:     true,
			TokenType:     "service",
			Orphan:        true,
		},
	}, nil
}
