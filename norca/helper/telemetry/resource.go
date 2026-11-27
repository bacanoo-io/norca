// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package telemetry

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sync"
	"sync/atomic"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/resource"
)

// ServiceName is the service.name of every signal of the agent.
const ServiceName = "norca"

// Roles of an agent, the values of norca.agent.roles.
const (
	RoleServer = "server"
	RoleClient = "client"
)

// Identity describes the agent for the resource attributes. It is complete
// only once the agent runs: the client loads or creates its node ID at
// start, and the server its Raft ID. Before that, Start gets what is known
// from the configuration (version, host, region, datacenter); Identify
// completes it.
type Identity struct {
	// Version is the agent version (service.version).
	Version string

	// HostName is host.name: the hostname, or the node name with
	// telemetry.use_node_name.
	HostName string

	Region     string
	Datacenter string

	// NodeID, NodeName and NodePool are those of the client; empty on a
	// server-only agent.
	NodeID   string
	NodeName string
	NodePool string

	// ServerID is the Raft server ID; empty on a client-only agent.
	ServerID string
}

// roles returns norca.agent.roles in a stable order.
func (id Identity) roles() []string {
	var roles []string
	if id.ServerID != "" {
		roles = append(roles, RoleServer)
	}
	if id.NodeID != "" {
		roles = append(roles, RoleClient)
	}
	return roles
}

// instanceID is service.instance.id: the client node ID, or the server's
// node ID on a server-only agent.
func (id Identity) instanceID() string {
	if id.NodeID != "" {
		return id.NodeID
	}
	return id.ServerID
}

// newResource builds the resource of every signal: the attributes of the
// agent, os.type and process.pid from the SDK detectors, and the user's
// resource_attributes on top (Resolve keeps them out of norca.*). It reads
// no environment variable: OTEL_RESOURCE_ATTRIBUTES and OTEL_SERVICE_NAME
// have no effect.
func newResource(ctx context.Context, id Identity, user map[string]string) (*resource.Resource, error) {
	attrs := []attribute.KeyValue{
		attribute.String("service.name", ServiceName),
	}
	add := func(key, value string) {
		if value != "" {
			attrs = append(attrs, attribute.String(key, value))
		}
	}
	add("service.version", id.Version)
	add("service.instance.id", id.instanceID())
	add("host.name", id.HostName)
	add("norca.region", id.Region)
	add("norca.datacenter", id.Datacenter)
	add("norca.node.id", id.NodeID)
	add("norca.node.name", id.NodeName)
	add("norca.node_pool", id.NodePool)
	add("norca.server.id", id.ServerID)
	if roles := id.roles(); len(roles) > 0 {
		attrs = append(attrs, attribute.StringSlice("norca.agent.roles", roles))
	}

	userAttrs := make([]attribute.KeyValue, 0, len(user))
	for _, k := range slices.Sorted(maps.Keys(user)) {
		userAttrs = append(userAttrs, attribute.String(k, user[k]))
	}

	// later options win on a conflicting key
	res, err := resource.New(ctx,
		resource.WithOSType(),
		resource.WithProcessPID(),
		resource.WithAttributes(attrs...),
		resource.WithAttributes(userAttrs...),
	)
	if err != nil {
		return nil, fmt.Errorf("building the telemetry resource: %w", err)
	}
	return res, nil
}

// agentResource is the resource the exporters put on what they export, read
// at export time.
//
// The providers start before the agent (Start), so the setup of the server
// and the client and their first work (a new leader's evaluations) are
// traced and measured, while the resource is complete only once the agent
// runs (Identify). Until then the exports are held: the spans in the span
// queue, the metrics in the export of the periodic reader (up to its
// timeout), the log lines in the log sink. Nothing goes out with an
// incomplete resource, which would show the agent as another instance for
// a while; what ended before Identify goes out with the complete one. Only
// a shutdown before Identify (a failed startup) lets them go, with the
// resource known then.
type agentResource struct {
	current atomic.Pointer[resource.Resource]

	// complete is closed by Identify, or by a shutdown before it.
	complete  chan struct{}
	closeOnce sync.Once
}

func newAgentResource() *agentResource {
	return &agentResource{complete: make(chan struct{})}
}

// set makes res the resource of the exports from now on.
func (r *agentResource) set(res *resource.Resource) {
	r.current.Store(res)
}

// load returns the resource of the exports: nil before Start, or when r is
// nil (an exporter built without one, in tests).
func (r *agentResource) load() *resource.Resource {
	if r == nil {
		return nil
	}
	return r.current.Load()
}

// release lets the held exports go on, with the resource set by then.
func (r *agentResource) release() {
	r.closeOnce.Do(func() { close(r.complete) })
}

// wait blocks until release or the end of ctx, and returns the resource. A
// nil r does not wait.
func (r *agentResource) wait(ctx context.Context) (*resource.Resource, error) {
	if r == nil {
		return nil, nil
	}
	select {
	case <-r.complete:
		return r.load(), nil
	case <-ctx.Done():
		return nil, fmt.Errorf("waiting for the identity of the agent: %w", context.Cause(ctx))
	}
}
