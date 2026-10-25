// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

// Package externalsvc is the client side of external services: it keeps the
// external service registrations owned by the local node up to date in
// memory, and runs their http and tcp checks, reporting status changes to the
// servers. The Consul-compatible agent API reads the owned registrations from
// it, as a Consul agent serves its local services.
package externalsvc

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/nomad/nomad/structs"
)

const (
	// waitTime is the wait of the blocking query following the owned
	// registrations.
	waitTime = 5 * time.Minute

	// maxRetryWait is the longest wait between two failed queries.
	maxRetryWait = time.Minute
)

// Config of the agent.
type Config struct {
	// RPC performs an RPC to the servers.
	RPC func(method string, args, reply any) error

	// NodeID and NodeSecret return the identity of the local node.
	NodeID     func() string
	NodeSecret func() string

	// Region returns the region of the client.
	Region func() string
}

// Agent follows the external registrations owned by the local node and runs
// their checks.
type Agent struct {
	logger hclog.Logger
	cfg    Config

	lock    sync.RWMutex
	regs    []*structs.ServiceRegistration
	index   uint64
	runners map[runnerKey]*runner

	ctx    context.Context
	cancel context.CancelFunc
}

// New returns an agent; it does nothing until Start is called.
func New(logger hclog.Logger, cfg Config) *Agent {
	ctx, cancel := context.WithCancel(context.Background())
	return &Agent{
		logger:  logger.Named("external_services"),
		cfg:     cfg,
		runners: make(map[runnerKey]*runner),
		ctx:     ctx,
		cancel:  cancel,
	}
}

// Start follows the owned registrations.
func (a *Agent) Start() {
	go a.watch()
}

// Stop stops following the registrations and running checks. It does not
// wait for the blocking query in flight, which ends with its wait time or the
// RPC connection.
func (a *Agent) Stop() {
	a.cancel()
	a.lock.Lock()
	for key, r := range a.runners {
		r.stop()
		delete(a.runners, key)
	}
	a.lock.Unlock()
}

// Services returns the external registrations owned by the local node. The
// registrations must not be modified.
func (a *Agent) Services() []*structs.ServiceRegistration {
	a.lock.RLock()
	defer a.lock.RUnlock()
	return slices.Clone(a.regs)
}

// Refresh reads the owned registrations from the leader, so registrations
// just written are known.
func (a *Agent) Refresh() error {
	_, err := a.list(0, false)
	return err
}

// list reads the owned registrations and applies them.
func (a *Agent) list(minIndex uint64, stale bool) (uint64, error) {
	args := &structs.ExternalServiceListRequest{
		NodeID: a.cfg.NodeID(),
		QueryOptions: structs.QueryOptions{
			Region:        a.cfg.Region(),
			Namespace:     structs.AllNamespacesSentinel,
			AuthToken:     a.cfg.NodeSecret(),
			AllowStale:    stale,
			MinQueryIndex: minIndex,
		},
	}
	if minIndex > 0 {
		args.MaxQueryTime = waitTime
	}
	var reply structs.ExternalServiceListResponse
	if err := a.cfg.RPC(structs.ServiceRegistrationListExternalRPCMethod, args, &reply); err != nil {
		return 0, err
	}
	a.apply(reply.Services, reply.Index)
	return reply.Index, nil
}

// watch follows the owned registrations with a blocking query.
func (a *Agent) watch() {
	var retry time.Duration
	for {
		a.lock.RLock()
		index := a.index
		a.lock.RUnlock()

		_, err := a.list(index, true)
		if a.ctx.Err() != nil {
			return
		}
		if err != nil {
			retry = min(max(2*retry, time.Second), maxRetryWait)
			// servers without external services answer every query so
			level := hclog.Debug
			if strings.Contains(err.Error(), "must run Norca") || strings.Contains(err.Error(), "can't find method") ||
				errors.Is(err, context.Canceled) {
				level = hclog.Trace
			}
			a.logger.Log(level, "failed to read external services of the node", "error", err, "retry", retry)
			select {
			case <-a.ctx.Done():
				return
			case <-time.After(retry):
			}
			continue
		}
		retry = 0
	}
}

// apply records the owned registrations read at index, and starts and stops
// the check runners accordingly. Answers older than the recorded ones are
// ignored.
func (a *Agent) apply(regs []*structs.ServiceRegistration, index uint64) {
	a.lock.Lock()
	defer a.lock.Unlock()

	if index < a.index || a.ctx.Err() != nil {
		return
	}
	a.index = index
	a.regs = regs

	want := make(map[runnerKey]bool)
	for _, reg := range regs {
		if reg.External == nil {
			continue
		}
		for _, def := range reg.External.Checks {
			if !def.IsNodeCheck() {
				continue
			}
			key := runnerKey{namespace: reg.Namespace, id: reg.ID, check: def.ID}
			want[key] = true
			status := ""
			if state := reg.CheckState(def.ID); state != nil {
				status = state.Status
			}
			if r, ok := a.runners[key]; ok {
				if r.def.Equal(def) {
					r.setReported(status)
					continue
				}
				r.stop()
			}
			r := newRunner(a, key, def.Copy(), status)
			a.runners[key] = r
			r.start()
		}
	}
	for key, r := range a.runners {
		if !want[key] {
			r.stop()
			delete(a.runners, key)
		}
	}
}

// report sends the status of a check to the servers.
func (a *Agent) report(key runnerKey, status, output string) error {
	args := &structs.ExternalServiceCheckUpdateRequest{
		ID:         key.id,
		Updates:    []*structs.ExternalCheckUpdate{{CheckID: key.check, Status: status, Output: output}},
		NodeReport: true,
		WriteRequest: structs.WriteRequest{
			Region:    a.cfg.Region(),
			Namespace: key.namespace,
			AuthToken: a.cfg.NodeSecret(),
		},
	}
	var reply structs.ExternalServiceCheckUpdateResponse
	return a.cfg.RPC(structs.ServiceRegistrationUpdateExternalChecksRPCMethod, args, &reply)
}
