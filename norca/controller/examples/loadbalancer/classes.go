// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bacanoo-io/norca/norca/controller"
)

// ClassKind is the cluster-scoped object type that registers a load balancer
// provider: which class it handles and where the operator calls it (see
// loadbalancerclass.kind.hcl). Providers are external applications; they
// write their own LoadBalancerClass object when they start and report a
// heartbeat in its status.
var ClassKind = controller.Kind{Group: "norca.io", Kind: "LoadBalancerClass"}

// ClassSpec is the registration of a provider.
type ClassSpec struct {
	// Endpoint is the base URL of the provider API (https).
	Endpoint string `json:"endpoint"`

	// TokenVariable is the path of the Variable whose item "token" is sent
	// as the bearer token of the calls. The token itself never is part of
	// the object.
	TokenVariable string `json:"tokenVariable,omitempty"`

	Capabilities ClassCapabilities `json:"capabilities,omitzero"`

	// Parameters are provider specific settings, passed on with every call.
	Parameters map[string]any `json:"parameters,omitempty"`
}

// ClassCapabilities is what a provider supports.
type ClassCapabilities struct {
	// Internal is true if the provider can create load balancers that are
	// not reachable from the internet.
	Internal bool `json:"internal,omitempty"`

	// Ports limits the frontend ports ("1-65535", "80,443", "8000-8100,443").
	// Empty means any port.
	Ports string `json:"ports,omitempty"`
}

// ClassStatus is reported by the provider.
type ClassStatus struct {
	// LastHeartbeat (RFC 3339) is when the provider last said it is alive.
	// A class without a heartbeat is a static registration and always
	// available; one with a heartbeat older than the class TTL is not.
	LastHeartbeat string `json:"lastHeartbeat,omitempty"`

	// Version of the provider.
	Version string `json:"version,omitempty"`
}

// classInfo is a registered provider.
type classInfo struct {
	Name   string
	Spec   ClassSpec
	Status ClassStatus
}

// Validate checks the registration.
func (c *classInfo) validate(allowHTTP bool) error {
	u, err := url.Parse(c.Spec.Endpoint)
	if err != nil || u.Host == "" {
		return fmt.Errorf("invalid endpoint %q", c.Spec.Endpoint)
	}
	if u.Scheme != "https" && !(allowHTTP && u.Scheme == "http") {
		return fmt.Errorf("endpoint %q must be an https URL", c.Spec.Endpoint)
	}
	if _, err := parsePortRanges(c.Spec.Capabilities.Ports); err != nil {
		return fmt.Errorf("capabilities.ports: %w", err)
	}
	return nil
}

// available reports whether the heartbeat of the provider is recent.
func (c *classInfo) available(now time.Time, ttl time.Duration) (bool, string) {
	if c.Status.LastHeartbeat == "" {
		return true, ""
	}
	t, err := time.Parse(time.RFC3339, c.Status.LastHeartbeat)
	if err != nil {
		return false, fmt.Sprintf("invalid heartbeat %q", c.Status.LastHeartbeat)
	}
	if age := now.Sub(t); age > ttl {
		return false, fmt.Sprintf("no heartbeat for %s (last at %s)", age.Round(time.Second), c.Status.LastHeartbeat)
	}
	return true, ""
}

// unsupported returns why the provider cannot implement the spec, or "".
func (c *classInfo) unsupported(spec Spec) string {
	if spec.Internal && !c.Spec.Capabilities.Internal {
		return fmt.Sprintf("class %q cannot create internal load balancers", c.Name)
	}
	ranges, _ := parsePortRanges(c.Spec.Capabilities.Ports)
	if len(ranges) > 0 && !slices.ContainsFunc(ranges, func(r [2]int) bool { return spec.Port >= r[0] && spec.Port <= r[1] }) {
		return fmt.Sprintf("class %q does not support port %d (supported: %s)", c.Name, spec.Port, c.Spec.Capabilities.Ports)
	}
	return ""
}

// parsePortRanges parses "80,443,8000-8100".
func parsePortRanges(s string) ([][2]int, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	var out [][2]int
	for part := range strings.SplitSeq(s, ",") {
		lo, hi, isRange := strings.Cut(strings.TrimSpace(part), "-")
		from, err := strconv.Atoi(lo)
		if err != nil || from < 1 || from > 65535 {
			return nil, fmt.Errorf("invalid port %q", lo)
		}
		to := from
		if isRange {
			if to, err = strconv.Atoi(hi); err != nil || to < from || to > 65535 {
				return nil, fmt.Errorf("invalid port range %q", part)
			}
		}
		out = append(out, [2]int{from, to})
	}
	return out, nil
}

// classRegistry reads the LoadBalancerClass objects. The list is cached for
// a few seconds, so a burst of reconciles makes one call.
type classRegistry struct {
	objs controller.ObjectsAPI
	vars controller.VariablesAPI

	// ttl is how old a heartbeat may be.
	ttl time.Duration

	// tokenNamespace is the namespace of the token Variables.
	tokenNamespace string

	// allowHTTP permits http endpoints (development and tests).
	allowHTTP bool

	// cacheFor is how long a listing is reused.
	cacheFor time.Duration
	now      func() time.Time
	client   *http.Client

	mu     sync.Mutex
	cached map[string]*classInfo
	at     time.Time
}

func newClassRegistry(b controller.Backend, tokenNamespace string, ttl time.Duration) *classRegistry {
	if tokenNamespace == "" {
		tokenNamespace = "default"
	}
	return &classRegistry{
		objs:           b,
		vars:           b,
		ttl:            ttl,
		tokenNamespace: tokenNamespace,
		cacheFor:       3 * time.Second,
		now:            time.Now,
		client:         &http.Client{Timeout: 30 * time.Second},
	}
}

func (g *classRegistry) list(ctx context.Context) (map[string]*classInfo, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.cached != nil && g.now().Sub(g.at) < g.cacheFor {
		return g.cached, nil
	}
	objs, _, err := g.objs.ListObjects(ctx, ClassKind, "*", 0)
	if err != nil {
		return nil, err
	}
	out := make(map[string]*classInfo, len(objs))
	for _, o := range objs {
		info := &classInfo{Name: o.Name}
		if b, err := json.Marshal(o.Spec); err == nil {
			_ = json.Unmarshal(b, &info.Spec)
		}
		if b, err := json.Marshal(o.Status); err == nil {
			_ = json.Unmarshal(b, &info.Status)
		}
		out[o.Name] = info
	}
	g.cached, g.at = out, g.now()
	return out, nil
}

// lookup returns the registered class, or nil.
func (g *classRegistry) lookup(ctx context.Context, name string) (*classInfo, error) {
	all, err := g.list(ctx)
	if err != nil {
		return nil, err
	}
	return all[name], nil
}

// names returns the registered class names, sorted.
func (g *classRegistry) names(ctx context.Context) []string {
	all, err := g.list(ctx)
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(all))
	for name := range all {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// remoteProvider calls the HTTP API of a registered provider:
//
//	PUT    {endpoint}/v1/loadbalancers/{namespace}/{name}   create or update
//	DELETE {endpoint}/v1/loadbalancers/{namespace}/{name}   remove
//
// PUT is idempotent and returns the state of the load balancer.
type remoteProvider struct {
	info *classInfo
	reg  *classRegistry
}

func (g *classRegistry) provider(info *classInfo) *remoteProvider {
	return &remoteProvider{info: info, reg: g}
}

func (p *remoteProvider) Class() string { return p.info.Name }

// remoteRequest is the body of a PUT call.
type remoteRequest struct {
	Name       string         `json:"name"`
	Namespace  string         `json:"namespace"`
	Service    string         `json:"service"`
	Port       int            `json:"port"`
	Internal   bool           `json:"internal"`
	Targets    []remoteTarget `json:"targets"`
	Parameters map[string]any `json:"parameters,omitempty"`
}

type remoteTarget struct {
	Address string `json:"address"`
	Port    int    `json:"port"`
	AllocID string `json:"allocID,omitempty"`
}

// remoteResponse is the answer of a PUT call.
type remoteResponse struct {
	Ready     bool     `json:"ready"`
	Addresses []string `json:"addresses"`
	Resource  string   `json:"resource"`
	Message   string   `json:"message"`
}

func (p *remoteProvider) url(key controller.ObjectKey) string {
	return strings.TrimRight(p.info.Spec.Endpoint, "/") + "/v1/loadbalancers/" +
		url.PathEscape(key.Namespace) + "/" + url.PathEscape(key.Name)
}

func (p *remoteProvider) token(ctx context.Context) (string, error) {
	path := p.info.Spec.TokenVariable
	if path == "" {
		return "", nil
	}
	v, err := p.reg.vars.ReadVariable(ctx, p.reg.tokenNamespace, path)
	if err != nil {
		return "", fmt.Errorf("reading the token Variable %q of class %q: %w", path, p.info.Name, err)
	}
	return v.Items["token"], nil
}

func (p *remoteProvider) do(ctx context.Context, method string, key controller.ObjectKey, body any) (*http.Response, []byte, error) {
	if err := p.info.validate(p.reg.allowHTTP); err != nil {
		return nil, nil, fmt.Errorf("class %q: %w", p.info.Name, err)
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, p.url(key), rd)
	if err != nil {
		return nil, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	token, err := p.token(ctx)
	if err != nil {
		return nil, nil, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := p.reg.client.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("calling class %q: %w", p.info.Name, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp, data, err
}

func (p *remoteProvider) Ensure(ctx context.Context, obj *controller.Object, spec Spec, targets []Target) (Result, error) {
	req := remoteRequest{
		Name:       obj.Key.Name,
		Namespace:  obj.Key.Namespace,
		Service:    spec.Service,
		Port:       spec.Port,
		Internal:   spec.Internal,
		Targets:    make([]remoteTarget, len(targets)),
		Parameters: p.info.Spec.Parameters,
	}
	for i, t := range targets {
		req.Targets[i] = remoteTarget{Address: t.Address, Port: t.Port, AllocID: t.AllocID}
	}
	resp, data, err := p.do(ctx, http.MethodPut, obj.Key, req)
	if err != nil {
		return Result{}, err
	}
	if resp.StatusCode/100 != 2 {
		return Result{}, fmt.Errorf("class %q answered %d: %s", p.info.Name, resp.StatusCode, snippet(data))
	}
	var out remoteResponse
	if err := json.Unmarshal(data, &out); err != nil {
		return Result{}, fmt.Errorf("class %q: invalid answer: %w", p.info.Name, err)
	}
	return Result{Addresses: out.Addresses, Ready: out.Ready, Message: out.Message, Resource: out.Resource}, nil
}

func (p *remoteProvider) Delete(ctx context.Context, key controller.ObjectKey) error {
	resp, data, err := p.do(ctx, http.MethodDelete, key, nil)
	if err != nil {
		return err
	}
	if resp.StatusCode/100 == 2 || resp.StatusCode == http.StatusNotFound {
		return nil
	}
	return fmt.Errorf("class %q answered %d: %s", p.info.Name, resp.StatusCode, snippet(data))
}

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 200 {
		s = s[:200] + "..."
	}
	return s
}

var errClassNotRegistered = errors.New("class is not registered")
