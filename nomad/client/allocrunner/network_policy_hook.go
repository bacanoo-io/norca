// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package allocrunner

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"sync"
	"time"

	metrics "github.com/armon/go-metrics"
	hclog "github.com/hashicorp/go-hclog"
	"github.com/hashicorp/nomad/client/allocrunner/interfaces"
	"github.com/hashicorp/nomad/client/allocrunner/netpolicy"
	"github.com/hashicorp/nomad/client/config"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/nomad/plugins/drivers"
)

const (
	// TaskNetworkPolicyNotEnforced is the task event emitted when network
	// policies select an allocation but cannot be enforced.
	TaskNetworkPolicyNotEnforced = "Network Policy Not Enforced"

	// networkPolicyQueryWait is the maximum wait of the blocking query of
	// the effective rules.
	networkPolicyQueryWait = 5 * time.Minute

	// networkPolicyCounterInterval is the interval of the drop counter
	// metrics.
	networkPolicyCounterInterval = 10 * time.Second
)

// networkSpecGetter returns the network isolation spec of the allocation
// once the network hook has created it.
type networkSpecGetter interface {
	networkSpec() *drivers.NetworkIsolationSpec
}

// taskEventEmitter emits an event on every task of the allocation.
type taskEventEmitter interface {
	EmitTaskEvents(*structs.TaskEvent)
}

// networkPolicyBackend programs the rules; it is replaced in tests.
type networkPolicyBackend interface {
	Apply(nsPath string, rs *netpolicy.Ruleset) error
	ReadCounters(nsPath string) (map[string]uint64, error)
	Gateways(nsPath string) ([]netip.Addr, error)
}

// networkSpec returns the network isolation spec created by the network
// hook. It is set by Prerun, which runs before the Prerun of the network
// policy hook.
func (h *networkHook) networkSpec() *drivers.NetworkIsolationSpec {
	return h.spec
}

// allocTaskEvents emits task events on every task of an allocation.
type allocTaskEvents struct {
	ar *allocRunner
}

func (a allocTaskEvents) EmitTaskEvents(event *structs.TaskEvent) {
	for _, tr := range a.ar.tasks {
		tr.EmitEvent(event.Copy())
	}
}

type nftBackend struct{}

func (nftBackend) Apply(nsPath string, rs *netpolicy.Ruleset) error {
	return netpolicy.Apply(nsPath, rs)
}
func (nftBackend) ReadCounters(nsPath string) (map[string]uint64, error) {
	return netpolicy.ReadCounters(nsPath)
}
func (nftBackend) Gateways(nsPath string) ([]netip.Addr, error) { return netpolicy.Gateways(nsPath) }

// networkPolicyHook enforces the network policies selecting the allocation
// (Kubernetes features spec, item 7, levels A and B). It writes the compiled
// rule set into the network namespace of the allocation with nftables and
// keeps it up to date with a blocking query of the effective rules. It does
// nothing for allocations without a network namespace (host mode) and
// writes no rules when no policy selects the allocation.
type networkPolicyHook struct {
	logger     hclog.Logger
	rpc        config.RPCer
	region     string
	nodeSecret string
	spec       networkSpecGetter
	events     taskEventEmitter
	backend    networkPolicyBackend

	publishMetrics bool

	mu    sync.Mutex
	alloc *structs.Allocation

	// applied is the rendered rule set in the namespace, counters its drop
	// counters and counts their last read packet counts.
	applied  string
	counters []*netpolicy.Counter
	counts   map[string]uint64
	warned   string

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

func newNetworkPolicyHook(logger hclog.Logger, alloc *structs.Allocation, rpc config.RPCer,
	clientConfig *config.Config, spec networkSpecGetter, events taskEventEmitter) *networkPolicyHook {
	ctx, cancel := context.WithCancel(context.Background())
	h := &networkPolicyHook{
		logger:  logger.Named("network_policy"),
		rpc:     rpc,
		spec:    spec,
		events:  events,
		backend: nftBackend{},
		alloc:   alloc,
		counts:  make(map[string]uint64),
		ctx:     ctx,
		cancel:  cancel,
	}
	if clientConfig != nil {
		h.region = clientConfig.Region
		h.publishMetrics = clientConfig.PublishAllocationMetrics
		if clientConfig.Node != nil {
			h.nodeSecret = clientConfig.Node.SecretID
		}
	}
	return h
}

func (h *networkPolicyHook) Name() string {
	return "network_policy"
}

// netnsPath returns the path of the network namespace of the allocation or
// "" if it has none or the group network mode cannot enforce rules.
func (h *networkPolicyHook) netnsPath() string {
	h.mu.Lock()
	alloc := h.alloc
	h.mu.Unlock()
	if alloc.Job == nil {
		return ""
	}
	mode := structs.NetworkPolicyGroupMode(alloc.Job.LookupTaskGroup(alloc.TaskGroup))
	if !structs.NetworkPolicyModeEnforceable(mode) || !structs.NetworkPolicyModeNeedsNftables(mode) {
		return ""
	}
	if h.spec == nil {
		return ""
	}
	spec := h.spec.networkSpec()
	if spec == nil || spec.Path == "" {
		return ""
	}
	return spec.Path
}

func (h *networkPolicyHook) Prerun() error {
	nsPath := h.netnsPath()
	if nsPath == "" {
		return nil
	}

	rules, index, err := h.fetch(0)
	if err != nil {
		// The rules programmed before a client restart stay in the network
		// namespace; keep them and retry in the background.
		h.logger.Warn("failed to fetch network policies, retrying in the background", "error", err)
	} else if err := h.sync(nsPath, rules); err != nil {
		if requiresEnforcement(rules) {
			return fmt.Errorf("failed to enforce network policies: %w", err)
		}
		h.logger.Warn("failed to enforce best effort network policies", "error", err)
	}

	h.wg.Add(1)
	go h.watch(nsPath, index)
	if h.publishMetrics {
		h.wg.Add(1)
		go h.collectCounters(nsPath)
	}
	return nil
}

// Update records allocation updates; the effective rules follow from the
// blocking query.
func (h *networkPolicyHook) Update(req *interfaces.RunnerUpdateRequest) error {
	h.mu.Lock()
	h.alloc = req.Alloc
	h.mu.Unlock()
	return nil
}

func (h *networkPolicyHook) stop() {
	h.cancel()
	h.wg.Wait()
}

// PreKill stops following rule changes before the tasks are killed. The
// rules stay in place until the network namespace is destroyed.
func (h *networkPolicyHook) PreKill() { h.stop() }

func (h *networkPolicyHook) Postrun() error {
	h.stop()
	return nil
}

func (h *networkPolicyHook) Destroy() error {
	h.stop()
	return nil
}

// Shutdown stops the hook when the client shuts down; the allocation keeps
// running with its rules.
func (h *networkPolicyHook) Shutdown() { h.stop() }

// fetch queries the effective rules of the allocation. A server without
// network policy support returns an empty rule set.
func (h *networkPolicyHook) fetch(minIndex uint64) (*structs.NetworkPolicyAllocRules, uint64, error) {
	h.mu.Lock()
	allocID := h.alloc.ID
	h.mu.Unlock()

	req := structs.NetworkPolicyAllocRequest{
		AllocID: allocID,
		QueryOptions: structs.QueryOptions{
			Region:        h.region,
			AllowStale:    true,
			MinQueryIndex: minIndex,
			MaxQueryTime:  networkPolicyQueryWait,
			AuthToken:     h.nodeSecret,
		},
	}
	var resp structs.NetworkPolicyAllocResponse
	if err := h.rpc.RPC("NetworkPolicy.GetAllocRules", &req, &resp); err != nil {
		if isUnknownRPCMethod(err) {
			return &structs.NetworkPolicyAllocRules{}, minIndex, nil
		}
		return nil, minIndex, err
	}
	return resp.Rules, resp.Index, nil
}

func isUnknownRPCMethod(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "can't find service") || strings.Contains(msg, "can't find method")
}

func requiresEnforcement(rules *structs.NetworkPolicyAllocRules) bool {
	if rules == nil {
		return false
	}
	for _, p := range rules.Policies {
		if p.Required() {
			return true
		}
	}
	for _, p := range rules.AdminPolicies {
		if p.Required() {
			return true
		}
	}
	return false
}

// sync compiles the rules and applies them if they changed.
func (h *networkPolicyHook) sync(nsPath string, rules *structs.NetworkPolicyAllocRules) error {
	h.mu.Lock()
	alloc := h.alloc
	h.mu.Unlock()

	var ports structs.AllocatedPorts
	if alloc.AllocatedResources != nil {
		ports = alloc.AllocatedResources.Shared.Ports
	}
	var gateways []netip.Addr
	if !rules.Empty() {
		var err error
		gateways, err = h.backend.Gateways(nsPath)
		if err != nil {
			h.logger.Warn("failed to look up the node addresses of the allocation network", "error", err)
		}
	}
	rs, err := netpolicy.Compile(&netpolicy.Input{
		Namespace: alloc.Namespace,
		Rules:     rules,
		Ports:     ports,
		NodeAddrs: gateways,
	})
	if err != nil {
		return err
	}

	rendered := rs.String()
	h.mu.Lock()
	unchanged := rendered == h.applied
	h.mu.Unlock()
	if unchanged {
		return nil
	}

	// Count the drops of the rules about to be replaced.
	h.emitCounters(nsPath)

	if err := h.backend.Apply(nsPath, rs); err != nil {
		h.warn(fmt.Sprintf("network policies could not be enforced: %v", err))
		return err
	}

	h.mu.Lock()
	h.applied = rendered
	h.counts = make(map[string]uint64)
	if rs != nil {
		h.counters = rs.Counters
	} else {
		h.counters = nil
	}
	h.mu.Unlock()

	if rs.Empty() {
		h.logger.Debug("no network policy restricts the allocation")
	} else {
		h.logger.Debug("network policies applied", "rules", rendered)
	}
	return nil
}

// warn emits a task event once per distinct message.
func (h *networkPolicyHook) warn(msg string) {
	h.mu.Lock()
	if h.warned == msg {
		h.mu.Unlock()
		return
	}
	h.warned = msg
	h.mu.Unlock()
	if h.events != nil {
		h.events.EmitTaskEvents(structs.NewTaskEvent(TaskNetworkPolicyNotEnforced).SetDisplayMessage(msg))
	}
}

// watch follows the effective rules with a blocking query.
func (h *networkPolicyHook) watch(nsPath string, index uint64) {
	defer h.wg.Done()

	retry := 0
	for {
		if h.ctx.Err() != nil {
			return
		}
		rules, next, err := h.fetch(index)
		if h.ctx.Err() != nil {
			return
		}
		if err != nil {
			retry++
			backoff := min(time.Minute, time.Second<<min(retry, 6))
			h.logger.Warn("failed to fetch network policies", "error", err, "retry_in", backoff)
			if !h.sleep(backoff) {
				return
			}
			continue
		}
		retry = 0
		if err := h.sync(nsPath, rules); err != nil {
			h.logger.Error("failed to apply network policies", "error", err)
		}
		if next <= index {
			// The query timed out or the index went backwards (e.g. a
			// server restore); keep blocking from the reported index.
			if next < index {
				index = 0
			}
			continue
		}
		index = next
	}
}

func (h *networkPolicyHook) sleep(d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-h.ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// collectCounters periodically publishes the drop counters.
func (h *networkPolicyHook) collectCounters(nsPath string) {
	defer h.wg.Done()
	ticker := time.NewTicker(networkPolicyCounterInterval)
	defer ticker.Stop()
	for {
		select {
		case <-h.ctx.Done():
			return
		case <-ticker.C:
			h.emitCounters(nsPath)
		}
	}
}

// emitCounters reads the drop counters and publishes their increase since
// the last read as the client.allocs.network_policy.dropped metric.
func (h *networkPolicyHook) emitCounters(nsPath string) {
	h.mu.Lock()
	counters := h.counters
	alloc := h.alloc
	h.mu.Unlock()
	if !h.publishMetrics || len(counters) == 0 {
		return
	}

	counts, err := h.backend.ReadCounters(nsPath)
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			h.logger.Debug("failed to read network policy counters", "error", err)
		}
		return
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	for _, c := range counters {
		packets, ok := counts[c.Name]
		if !ok {
			continue
		}
		delta := packets - h.counts[c.Name]
		if packets < h.counts[c.Name] {
			delta = packets
		}
		h.counts[c.Name] = packets
		if delta == 0 {
			continue
		}
		labels := []metrics.Label{
			{Name: "alloc_id", Value: alloc.ID},
			{Name: "namespace", Value: alloc.Namespace},
			{Name: "job", Value: alloc.JobID},
			{Name: "task_group", Value: alloc.TaskGroup},
			{Name: "direction", Value: c.Direction},
			{Name: "kind", Value: c.Kind},
			{Name: "policy", Value: c.Policy},
			{Name: "rule", Value: c.Rule},
		}
		metrics.IncrCounterWithLabels([]string{"client", "allocs", "network_policy", "dropped"}, float32(delta), labels)
	}
}
