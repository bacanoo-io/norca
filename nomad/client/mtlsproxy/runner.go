// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package mtlsproxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/nomad/client/discovery"
	"github.com/hashicorp/nomad/nomad/structs"
)

// The files the mtls hook writes into the mtls directory of the task.
const (
	certFile   = "cert.pem"
	keyFile    = "key.pem"
	bundleFile = "bundle.pem"
)

var (
	// defaultInterval is how often the runner renders the configuration
	// from its in-memory state. The catalog and the intentions are kept up
	// to date by blocking queries, so a change reaches Envoy within about
	// one interval.
	defaultInterval = time.Second

	// initialWait bounds how long Start waits for the first answers of the
	// servers before it writes the first configuration.
	initialWait = 10 * time.Second

	// lookupWait bounds the first lookup of a service in later renders.
	lookupWait = 2 * time.Second

	// blockingWait is the wait time of the blocking queries.
	blockingWait = 5 * time.Minute

	retryMin = time.Second
	retryMax = 30 * time.Second
)

// RunnerConfig configures a Runner.
type RunnerConfig struct {
	Logger hclog.Logger

	// RPC reaches the servers.
	RPC discovery.RPCFunc

	// CatalogToken returns the token of the catalog reads, which may be
	// forwarded to other regions.
	CatalogToken func() string

	// NodeSecret authenticates the intention reads in the local region.
	NodeSecret func() string

	// Region is the region of the client.
	Region string

	// CertDir is the mtls directory of the task, with the workload
	// certificate, its key and the trust bundle.
	CertDir string

	// Writer writes the configuration files.
	Writer *Writer

	// Sidecar or Gateway is the configuration to keep up to date; their
	// intentions and endpoints are filled in by the runner. Upstream
	// endpoints come from the instances of the upstream service, or of the
	// gateway service of a remote region.
	Sidecar *Sidecar
	Gateway *Gateway

	// Gateways maps upstream names to the gateway service of their region,
	// for upstreams in another region.
	Gateways map[string]string

	// Interval overrides defaultInterval.
	Interval time.Duration
}

// Runner keeps the Envoy configuration of a sidecar or gateway up to date:
// it follows the trust bundle and certificate files, the intentions of the
// namespace (blocking queries) and the instances of the upstream services
// (the catalog cache of the client discovery package), renders the
// configuration once per interval and writes the files that changed.
type Runner struct {
	cfg    RunnerConfig
	logger hclog.Logger
	cache  *discovery.Cache

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	notify chan struct{}

	lock       sync.Mutex
	intentions []*structs.ServiceIntention
	intIndex   uint64
	intReady   chan struct{}
	services   []discovery.Key
	svcIndex   uint64
	svcReady   chan struct{}
	endpoints  map[discovery.Key][]Endpoint
	regionCAs  map[string]string
	rootIndex  uint64
	rootsReady chan struct{}

	identity    *Identity
	identityRaw [3][]byte
	lastErr     string
}

// NewRunner returns a runner; Start starts it.
func NewRunner(cfg RunnerConfig) *Runner {
	if cfg.Interval == 0 {
		cfg.Interval = defaultInterval
	}
	ctx, cancel := context.WithCancel(context.Background())
	r := &Runner{
		cfg:        cfg,
		logger:     cfg.Logger.Named("mtls_proxy"),
		ctx:        ctx,
		cancel:     cancel,
		notify:     make(chan struct{}, 1),
		intReady:   make(chan struct{}),
		svcReady:   make(chan struct{}),
		rootsReady: make(chan struct{}),
		endpoints:  make(map[discovery.Key][]Endpoint),
	}
	r.cache = discovery.NewCache(cfg.Logger, cfg.RPC, cfg.CatalogToken)
	return r
}

// Start starts the watches, writes the first configuration (waiting up to
// initialWait for the servers, so the first connections find their
// endpoints and intentions) and keeps it up to date until Stop.
func (r *Runner) Start(ctx context.Context) error {
	// The watches end with the blocking query running when the runner is
	// stopped; Stop doesn't wait for them.
	if r.cfg.Sidecar != nil {
		go r.watchIntentions()
		if len(r.cfg.Sidecar.Upstreams) > 0 {
			go r.watchRoots()
		}
	}
	if r.cfg.Gateway != nil {
		go r.watchServices()
	}

	waitCtx, cancel := context.WithTimeout(ctx, initialWait)
	defer cancel()
	if r.cfg.Sidecar != nil {
		select {
		case <-r.intReady:
		case <-waitCtx.Done():
			r.logger.Warn("intentions not available yet, denying all inbound connections until they are")
		}
		if len(r.cfg.Sidecar.Upstreams) > 0 {
			select {
			case <-r.rootsReady:
			case <-waitCtx.Done():
				r.logger.Warn("certificate authorities of the regions not available yet, upstreams have no endpoints until they are")
			}
		}
	}
	if r.cfg.Gateway != nil {
		select {
		case <-r.svcReady:
		case <-waitCtx.Done():
			r.logger.Warn("service catalog not available yet")
		}
	}
	if err := r.render(waitCtx); err != nil {
		r.Stop()
		return err
	}

	r.wg.Add(1)
	go r.loop()
	return nil
}

// Stop stops the runner and waits for a render in progress; the files stay
// in place.
func (r *Runner) Stop() {
	r.cancel()
	r.cache.Stop()
	r.wg.Wait()
}

func (r *Runner) loop() {
	defer r.wg.Done()
	ticker := time.NewTicker(r.cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-r.ctx.Done():
			return
		case <-ticker.C:
		case <-r.notify:
		}
		ctx, cancel := context.WithTimeout(r.ctx, lookupWait)
		err := r.render(ctx)
		cancel()
		r.lock.Lock()
		msg := ""
		if err != nil {
			msg = err.Error()
		}
		if msg != r.lastErr {
			if err != nil {
				r.logger.Error("failed to update the Envoy configuration", "error", err)
			} else {
				r.logger.Info("Envoy configuration updated again")
			}
			r.lastErr = msg
		}
		r.lock.Unlock()
	}
}

func (r *Runner) wake() {
	select {
	case r.notify <- struct{}{}:
	default:
	}
}

// render computes the configuration and writes what changed.
func (r *Runner) render(ctx context.Context) error {
	var files map[string][]byte
	var err error
	switch {
	case r.cfg.Sidecar != nil:
		var id *Identity
		id, err = r.readIdentity()
		if err != nil {
			return err
		}
		s := *r.cfg.Sidecar
		r.lock.Lock()
		s.Intentions = r.intentions
		r.lock.Unlock()
		r.lock.Lock()
		regionCAs := r.regionCAs
		r.lock.Unlock()
		s.Upstreams = make([]Upstream, len(r.cfg.Sidecar.Upstreams))
		for i, u := range r.cfg.Sidecar.Upstreams {
			u.Endpoints = r.lookup(ctx, r.upstreamKey(u), r.upstreamModes(u))
			region := u.Region
			if region == "" {
				region = r.cfg.Region
			}
			u.TrustedCAs = regionCAs[region]
			s.Upstreams[i] = u
		}
		files, err = s.Files(id)

	case r.cfg.Gateway != nil:
		g := *r.cfg.Gateway
		r.lock.Lock()
		keys := r.services
		r.lock.Unlock()
		g.Routes = nil
		for _, key := range keys {
			endpoints := r.lookup(ctx, key, []string{structs.MTLSModeApp, structs.MTLSModeSidecar})
			if len(endpoints) == 0 {
				continue
			}
			g.Routes = append(g.Routes, Route{Namespace: key.Namespace, Service: key.Name, Endpoints: endpoints})
		}
		files, err = g.Files()

	default:
		return errors.New("nothing to configure")
	}
	if err != nil {
		return err
	}
	changed, err := r.cfg.Writer.Write(files)
	if changed && err == nil {
		r.logger.Debug("wrote the Envoy configuration")
	}
	return err
}

// upstreamKey returns the catalog key of the endpoints of an upstream: the
// service itself, or the gateway of its region.
func (r *Runner) upstreamKey(u Upstream) discovery.Key {
	if u.Region != "" && u.Region != r.cfg.Region {
		gateway := r.cfg.Gateways[u.Name]
		if gateway == "" {
			gateway = structs.DefaultMTLSGatewayService
		}
		return discovery.Key{Region: u.Region, Namespace: structs.MTLSGatewayNamespace, Name: gateway}
	}
	return discovery.Key{Region: r.cfg.Region, Namespace: u.Namespace, Name: u.Service}
}

// upstreamModes returns the mtls modes of the registrations an upstream may
// connect to.
func (r *Runner) upstreamModes(u Upstream) []string {
	if u.Region != "" && u.Region != r.cfg.Region {
		return []string{structs.MTLSModeGateway}
	}
	return []string{structs.MTLSModeApp, structs.MTLSModeSidecar}
}

// lookup returns the healthy instances of a service whose registrations have
// one of the mtls modes. If the catalog can't answer in time, the last known
// endpoints are kept.
func (r *Runner) lookup(ctx context.Context, key discovery.Key, modes []string) []Endpoint {
	regs, _, err := r.cache.Get(ctx, key)
	if err != nil {
		r.lock.Lock()
		defer r.lock.Unlock()
		return r.endpoints[key]
	}
	endpoints := FilterEndpoints(regs, modes)
	r.lock.Lock()
	r.endpoints[key] = endpoints
	r.lock.Unlock()
	return endpoints
}

// FilterEndpoints returns the addresses of the discoverable registrations
// whose mtls mode meta is one of modes.
func FilterEndpoints(regs []*structs.ServiceRegistration, modes []string) []Endpoint {
	var out []Endpoint
	for _, reg := range discovery.Eligible(regs, "", "") {
		mode := reg.Meta[structs.MTLSServiceMetaKey]
		ok := false
		for _, m := range modes {
			if m == mode {
				ok = true
				break
			}
		}
		if !ok || reg.Address == "" || reg.Port <= 0 {
			continue
		}
		out = append(out, Endpoint{Address: reg.Address, Port: reg.Port})
	}
	return out
}

// readIdentity reads the files of the mtls hook, parsing the bundle only
// when it changed.
func (r *Runner) readIdentity() (*Identity, error) {
	var raw [3][]byte
	for i, name := range []string{certFile, keyFile, bundleFile} {
		b, err := os.ReadFile(filepath.Join(r.cfg.CertDir, name))
		if err != nil {
			return nil, fmt.Errorf("failed to read the workload certificate: %w", err)
		}
		raw[i] = b
	}
	r.lock.Lock()
	defer r.lock.Unlock()
	if r.identity != nil && bytes.Equal(raw[0], r.identityRaw[0]) &&
		bytes.Equal(raw[1], r.identityRaw[1]) && bytes.Equal(raw[2], r.identityRaw[2]) {
		return r.identity, nil
	}
	bundles, err := ParseBundle(raw[2])
	if err != nil {
		return nil, err
	}
	r.identity = &Identity{CertPEM: raw[0], KeyPEM: raw[1], Bundles: bundles}
	r.identityRaw = raw
	return r.identity, nil
}

// watchIntentions follows the intentions of the namespace of the sidecar.
func (r *Runner) watchIntentions() {
	ready := false
	backoff := retryMin
	for r.ctx.Err() == nil {
		r.lock.Lock()
		index := r.intIndex
		r.lock.Unlock()

		args := &structs.ServiceIntentionListRequest{QueryOptions: structs.QueryOptions{
			Region:        r.cfg.Region,
			Namespace:     r.cfg.Sidecar.Namespace,
			AuthToken:     r.cfg.NodeSecret(),
			AllowStale:    true,
			MinQueryIndex: index,
			MaxQueryTime:  blockingWait,
		}}
		var reply structs.ServiceIntentionListResponse
		if err := r.cfg.RPC("ServiceIntention.List", args, &reply); err != nil {
			if r.ctx.Err() != nil {
				return
			}
			r.logger.Debug("failed to read the service intentions", "error", err)
			select {
			case <-r.ctx.Done():
				return
			case <-time.After(backoff):
			}
			backoff = min(2*backoff, retryMax)
			continue
		}
		backoff = retryMin

		r.lock.Lock()
		changed := reply.Index != r.intIndex || !ready
		if reply.Index < r.intIndex {
			// the servers were restored to an older state
			r.intIndex = 0
		} else {
			r.intIndex = reply.Index
		}
		r.intentions = reply.Intentions
		r.lock.Unlock()
		if !ready {
			ready = true
			close(r.intReady)
		}
		if changed {
			r.wake()
		}
	}
}

// watchRoots follows the trust bundle of the federation with blocking
// queries and keeps the CA certificates of each region (active and retired
// roots), so an upstream only trusts the CAs of the region of its
// destination.
func (r *Runner) watchRoots() {
	ready := false
	backoff := retryMin
	for r.ctx.Err() == nil {
		r.lock.Lock()
		index := r.rootIndex
		r.lock.Unlock()

		args := &structs.MTLSRootsRequest{QueryOptions: structs.QueryOptions{
			Region:        r.cfg.Region,
			AuthToken:     r.cfg.NodeSecret(),
			AllowStale:    true,
			MinQueryIndex: index,
			MaxQueryTime:  blockingWait,
		}}
		var reply structs.MTLSRootsResponse
		if err := r.cfg.RPC("MTLS.Roots", args, &reply); err != nil {
			if r.ctx.Err() != nil {
				return
			}
			r.logger.Debug("failed to read the certificate authorities", "error", err)
			select {
			case <-r.ctx.Done():
				return
			case <-time.After(backoff):
			}
			backoff = min(2*backoff, retryMax)
			continue
		}
		backoff = retryMin

		cas := RegionCAs(reply.Roots)
		r.lock.Lock()
		if reply.Index < r.rootIndex {
			r.rootIndex = 0
		} else {
			r.rootIndex = reply.Index
		}
		r.regionCAs = cas
		r.lock.Unlock()
		if !ready {
			ready = true
			close(r.rootsReady)
		}
		r.wake()
	}
}

// RegionCAs returns the PEM encoded CA certificates of each region of a
// trust bundle, sorted by root ID so the output is stable.
func RegionCAs(roots []*structs.MTLSRoot) map[string]string {
	sorted := make([]*structs.MTLSRoot, 0, len(roots))
	for _, root := range roots {
		if root != nil && root.Region != "" && strings.TrimSpace(root.RootCert) != "" {
			sorted = append(sorted, root)
		}
	}
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })
	out := make(map[string]string)
	for _, root := range sorted {
		out[root.Region] += strings.TrimSpace(root.RootCert) + "\n"
	}
	return out
}

// watchServices follows the service names of every namespace of the region
// of a gateway.
func (r *Runner) watchServices() {
	ready := false
	backoff := retryMin
	for r.ctx.Err() == nil {
		r.lock.Lock()
		index := r.svcIndex
		r.lock.Unlock()

		args := &structs.ServiceRegistrationListRequest{QueryOptions: structs.QueryOptions{
			Region:        r.cfg.Region,
			Namespace:     structs.AllNamespacesSentinel,
			AuthToken:     r.cfg.CatalogToken(),
			AllowStale:    true,
			MinQueryIndex: index,
			MaxQueryTime:  blockingWait,
		}}
		var reply structs.ServiceRegistrationListResponse
		if err := r.cfg.RPC(structs.ServiceRegistrationListRPCMethod, args, &reply); err != nil {
			if r.ctx.Err() != nil {
				return
			}
			r.logger.Debug("failed to list the services", "error", err)
			select {
			case <-r.ctx.Done():
				return
			case <-time.After(backoff):
			}
			backoff = min(2*backoff, retryMax)
			continue
		}
		backoff = retryMin

		var keys []discovery.Key
		for _, ns := range reply.Services {
			if ns == nil {
				continue
			}
			for _, svc := range ns.Services {
				if svc != nil {
					keys = append(keys, discovery.Key{Region: r.cfg.Region, Namespace: ns.Namespace, Name: svc.ServiceName})
				}
			}
		}
		sort.Slice(keys, func(i, j int) bool {
			if keys[i].Namespace != keys[j].Namespace {
				return keys[i].Namespace < keys[j].Namespace
			}
			return keys[i].Name < keys[j].Name
		})

		r.lock.Lock()
		if reply.Index < r.svcIndex {
			r.svcIndex = 0
		} else {
			r.svcIndex = reply.Index
		}
		r.services = keys
		r.lock.Unlock()
		if !ready {
			ready = true
			close(r.svcReady)
		}
		r.wake()
	}
}
