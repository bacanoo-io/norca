// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Package discovery serves service discovery on the client from the native
// service catalog: a cache of the registrations of the services the
// consumers of the client ask for, and the resolution of a lookup with
// health filtering, topology-aware selection and regional failover. The
// built-in DNS server uses it.
package discovery

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/nomad/nomad/structs"
)

const (
	// defaultIdleTimeout is how long a cache entry is kept up to date after
	// its last use.
	defaultIdleTimeout = 5 * time.Minute

	// defaultWaitTime is the wait time of the blocking queries keeping cache
	// entries up to date. It bounds how long an idle entry outlives its idle
	// timeout.
	defaultWaitTime = time.Minute

	// maxRetryWait is the longest wait between two failed queries.
	maxRetryWait = 30 * time.Second
)

// ErrStopped is returned by lookups on a stopped cache.
var ErrStopped = errors.New("service catalog cache stopped")

// RPCFunc performs an RPC to the servers.
type RPCFunc func(method string, args, reply any) error

// Cache keeps the registrations of services up to date in memory. An entry
// (region, namespace, service name) is created on its first lookup, which
// waits for the first answer of the servers; from then on a blocking query
// keeps it up to date, so lookups do not reach the servers. Entries that are
// not used for a while are dropped. Entries of other regions reach their
// servers through region forwarding.
//
// The cache remembers the last discovery settings of a service even when all
// its instances are gone, so regional failover keeps working.
type Cache struct {
	logger hclog.Logger
	rpc    RPCFunc
	token  func() string

	idleTimeout time.Duration
	waitTime    time.Duration

	lock    sync.Mutex
	entries map[Key]*entry
	stopped bool

	ctx    context.Context
	cancel context.CancelFunc
}

// Key identifies the registrations of one service.
type Key struct {
	Region    string
	Namespace string
	Name      string
}

type entry struct {
	key   Key
	ready chan struct{}

	lock      sync.RWMutex
	regs      []*structs.ServiceRegistration
	discovery *structs.ServiceDiscovery
	index     uint64
	err       error

	lastUsed atomic.Int64
}

// NewCache returns a cache that reads the catalog through rpc, with the ACL
// token returned by token.
func NewCache(logger hclog.Logger, rpc RPCFunc, token func() string) *Cache {
	ctx, cancel := context.WithCancel(context.Background())
	return &Cache{
		logger:      logger.Named("catalog_cache"),
		rpc:         rpc,
		token:       token,
		idleTimeout: defaultIdleTimeout,
		waitTime:    defaultWaitTime,
		entries:     make(map[Key]*entry),
		ctx:         ctx,
		cancel:      cancel,
	}
}

// Stop stops keeping the entries up to date.
func (c *Cache) Stop() {
	c.lock.Lock()
	defer c.lock.Unlock()
	c.stopped = true
	c.cancel()
}

// Get returns the registrations of a service, and its last known discovery
// settings. The first lookup of a service waits for the servers until ctx is
// done.
func (c *Cache) Get(ctx context.Context, key Key) ([]*structs.ServiceRegistration, *structs.ServiceDiscovery, error) {
	c.lock.Lock()
	if c.stopped {
		c.lock.Unlock()
		return nil, nil, ErrStopped
	}
	e, ok := c.entries[key]
	if !ok {
		e = &entry{key: key, ready: make(chan struct{})}
		c.entries[key] = e
		go c.watch(e)
	}
	c.lock.Unlock()

	e.lastUsed.Store(time.Now().UnixNano())

	select {
	case <-e.ready:
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	}

	e.lock.RLock()
	defer e.lock.RUnlock()
	if e.err != nil && e.index == 0 {
		return nil, e.discovery, e.err
	}
	return e.regs, e.discovery, nil
}

// watch keeps e up to date until it is idle or the cache is stopped.
func (c *Cache) watch(e *entry) {
	var retry time.Duration
	first := true

	for {
		e.lock.RLock()
		index := e.index
		e.lock.RUnlock()

		args := &structs.ServiceRegistrationByNameRequest{
			ServiceName: e.key.Name,
			QueryOptions: structs.QueryOptions{
				Region:        e.key.Region,
				Namespace:     e.key.Namespace,
				AuthToken:     c.token(),
				AllowStale:    true,
				MinQueryIndex: index,
				MaxQueryTime:  c.waitTime,
			},
		}
		var reply structs.ServiceRegistrationByNameResponse
		err := c.rpc(structs.ServiceRegistrationGetServiceRPCMethod, args, &reply)

		e.lock.Lock()
		if err != nil {
			e.err = err
		} else {
			e.err = nil
			e.regs = reply.Services
			e.index = reply.Index
			if reply.Discovery != nil {
				e.discovery = reply.Discovery
			}
		}
		e.lock.Unlock()

		if first {
			close(e.ready)
			first = false
		}

		if c.drop(e) {
			return
		}

		if err != nil {
			c.logger.Debug("failed to read service registrations", "service", e.key.Name,
				"namespace", e.key.Namespace, "region", e.key.Region, "error", err)
			retry = min(max(2*retry, time.Second), maxRetryWait)
			select {
			case <-c.ctx.Done():
				return
			case <-time.After(retry):
			}
			continue
		}
		retry = 0
	}
}

// drop removes e from the cache if it is idle or the cache is stopped, and
// returns whether it did.
func (c *Cache) drop(e *entry) bool {
	c.lock.Lock()
	defer c.lock.Unlock()

	idle := time.Since(time.Unix(0, e.lastUsed.Load())) > c.idleTimeout
	if !c.stopped && !idle {
		return false
	}
	if c.entries[e.key] == e {
		delete(c.entries, e.key)
	}
	return true
}

// Len returns the number of entries.
func (c *Cache) Len() int {
	c.lock.Lock()
	defer c.lock.Unlock()
	return len(c.entries)
}
