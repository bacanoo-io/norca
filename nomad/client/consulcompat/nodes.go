// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package consulcompat

import (
	"net"
	"sync"
	"time"

	consulapi "github.com/hashicorp/consul/api"
	"github.com/hashicorp/nomad/nomad/structs"
)

// nodeTTL is how long the details of a node are cached.
const nodeTTL = 30 * time.Second

// nodeCache caches the details of the nodes running service instances. They
// are read with the identity of the local node, which may read any node: the
// name, address and meta of the node of an instance are part of every
// catalog answer, as in Consul.
type nodeCache struct {
	backend Backend

	lock  sync.Mutex
	nodes map[string]cachedNode
}

type cachedNode struct {
	node    *structs.Node
	fetched time.Time
}

func newNodeCache(backend Backend) *nodeCache {
	return &nodeCache{backend: backend, nodes: make(map[string]cachedNode)}
}

// get returns the node of the given ID in region. If it cannot be read, a
// node carrying only its ID is returned.
func (c *nodeCache) get(region, id string) *structs.Node {
	key := region + "/" + id

	c.lock.Lock()
	cached, ok := c.nodes[key]
	c.lock.Unlock()
	if ok && time.Since(cached.fetched) < nodeTTL {
		return cached.node
	}

	args := &structs.NodeSpecificRequest{
		NodeID: id,
		QueryOptions: structs.QueryOptions{
			Region:     region,
			AuthToken:  c.backend.NodeSecret(),
			AllowStale: true,
		},
	}
	var reply structs.SingleNodeResponse
	if err := c.backend.RPC("Node.GetNode", args, &reply); err != nil || reply.Node == nil {
		if ok {
			return cached.node
		}
		return &structs.Node{ID: id, Name: id, Status: structs.NodeStatusReady}
	}

	c.lock.Lock()
	c.nodes[key] = cachedNode{node: reply.Node, fetched: time.Now()}
	c.lock.Unlock()
	return reply.Node
}

// externalNode is the node shown for external services that no client node
// owns, following the convention of external services in Consul.
var externalNode = &structs.Node{
	Name:   "external",
	Status: structs.NodeStatusReady,
	Meta:   map[string]string{"external-node": "true", "external-probe": "false"},
}

// nodeAddress returns the address of a node.
func nodeAddress(node *structs.Node) string {
	if addr := node.Attributes["unique.network.ip-address"]; addr != "" {
		return addr
	}
	if host, _, err := net.SplitHostPort(node.HTTPAddr); err == nil {
		return host
	}
	return node.HTTPAddr
}

// consulNode converts a node.
func consulNode(node *structs.Node, dc string) *consulapi.Node {
	addr := nodeAddress(node)
	meta := make(map[string]string, len(node.Meta))
	for k, v := range node.Meta {
		meta[k] = v
	}
	return &consulapi.Node{
		ID:              node.ID,
		Node:            node.Name,
		Address:         addr,
		Datacenter:      dc,
		TaggedAddresses: map[string]string{"lan": addr, "wan": addr},
		Meta:            meta,
		CreateIndex:     node.CreateIndex,
		ModifyIndex:     node.ModifyIndex,
	}
}

// nodeMetaMatches returns whether node has every key and value of filter.
func nodeMetaMatches(node *structs.Node, filter map[string]string) bool {
	for k, v := range filter {
		if node.Meta[k] != v {
			return false
		}
	}
	return true
}

// serfHealth returns the node check Consul reports with every instance: it
// passes while the Nomad node is up.
func serfHealth(node *structs.Node) *consulapi.HealthCheck {
	check := &consulapi.HealthCheck{
		Node:    node.Name,
		CheckID: "serfHealth",
		Name:    "Serf Health Status",
		Status:  consulapi.HealthPassing,
		Output:  "Agent alive and reachable",
	}
	switch node.Status {
	case structs.NodeStatusDown, structs.NodeStatusDisconnected:
		check.Status = consulapi.HealthCritical
		check.Output = "Agent not live or unreachable"
	}
	return check
}
