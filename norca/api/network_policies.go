// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package api

import (
	"errors"
	"net/url"
)

const (
	// TopicNetworkPolicy is the event stream topic of network policy and
	// admin network policy changes.
	TopicNetworkPolicy Topic = "NetworkPolicy"

	// NetworkPolicyEnforcementRequired rejects placements on nodes that
	// cannot enforce the rules; it is the default.
	NetworkPolicyEnforcementRequired = "required"

	// NetworkPolicyEnforcementBestEffort places allocations anyway, with a
	// warning.
	NetworkPolicyEnforcementBestEffort = "best_effort"

	// Actions of admin network policy rules.
	NetworkPolicyActionAllow = "allow"
	NetworkPolicyActionDeny  = "deny"
	NetworkPolicyActionPass  = "pass"
)

// NetworkPolicy restricts the ingress and egress traffic of the allocations
// of a namespace selected by their job and task group meta.
type NetworkPolicy struct {
	Name        string
	Namespace   string
	Description string
	Selector    map[string]string
	Ingress     []*NetworkPolicyRule
	Egress      []*NetworkPolicyRule
	DefaultDeny []string
	Enforcement string
	CreateIndex uint64
	ModifyIndex uint64
}

// AdminNetworkPolicy is a cluster level network policy whose allow and deny
// rules namespace policies cannot override.
type AdminNetworkPolicy struct {
	Name        string
	Description string
	Priority    int
	Namespaces  []string
	Selector    map[string]string
	Ingress     []*NetworkPolicyRule
	Egress      []*NetworkPolicyRule
	Enforcement string
	CreateIndex uint64
	ModifyIndex uint64
}

// NetworkPolicyRule is an ingress or egress rule.
type NetworkPolicyRule struct {
	Name   string
	Action string
	Peers  []*NetworkPolicyPeer
	Ports  []string
}

// NetworkPolicyPeer is an address block or a set of allocations on the same
// node.
type NetworkPolicyPeer struct {
	CIDR      []string
	Namespace string
	Selector  map[string]string
}

// NetworkPolicyAllocStatus describes an allocation selected by a policy.
type NetworkPolicyAllocStatus struct {
	ID          string
	Namespace   string
	JobID       string
	TaskGroup   string
	NodeID      string
	NodeName    string
	NetworkMode string
	Enforceable bool
	Reason      string
}

// NetworkPolicies is used to access the network policy endpoints.
type NetworkPolicies struct {
	client *Client
}

// NetworkPolicies returns a handle on the network policy endpoints.
func (c *Client) NetworkPolicies() *NetworkPolicies {
	return &NetworkPolicies{client: c}
}

// List lists the network policies of the namespace of the query options
// ("*" for all namespaces).
func (n *NetworkPolicies) List(q *QueryOptions) ([]*NetworkPolicy, *QueryMeta, error) {
	var resp []*NetworkPolicy
	qm, err := n.client.query("/v1/network-policies", &resp, q)
	if err != nil {
		return nil, nil, err
	}
	return resp, qm, nil
}

// Info returns a network policy.
func (n *NetworkPolicies) Info(name string, q *QueryOptions) (*NetworkPolicy, *QueryMeta, error) {
	if name == "" {
		return nil, nil, errors.New("missing network policy name")
	}
	var resp NetworkPolicy
	qm, err := n.client.query("/v1/network-policy/"+url.PathEscape(name), &resp, q)
	if err != nil {
		return nil, nil, err
	}
	return &resp, qm, nil
}

// Register creates or updates a network policy.
func (n *NetworkPolicies) Register(policy *NetworkPolicy, w *WriteOptions) (*WriteMeta, error) {
	if policy == nil || policy.Name == "" {
		return nil, errors.New("missing network policy name")
	}
	if policy.Namespace != "" {
		if w == nil {
			w = &WriteOptions{}
		}
		if w.Namespace == "" {
			w.Namespace = policy.Namespace
		}
	}
	return n.client.put("/v1/network-policy/"+url.PathEscape(policy.Name), policy, nil, w)
}

// Delete deletes a network policy of the namespace of the write options.
func (n *NetworkPolicies) Delete(name string, w *WriteOptions) (*WriteMeta, error) {
	if name == "" {
		return nil, errors.New("missing network policy name")
	}
	return n.client.delete("/v1/network-policy/"+url.PathEscape(name), nil, nil, w)
}

// Status lists the allocations a network policy selects.
func (n *NetworkPolicies) Status(name string, q *QueryOptions) ([]*NetworkPolicyAllocStatus, *QueryMeta, error) {
	if name == "" {
		return nil, nil, errors.New("missing network policy name")
	}
	var resp []*NetworkPolicyAllocStatus
	qm, err := n.client.query("/v1/network-policy/"+url.PathEscape(name)+"/status", &resp, q)
	if err != nil {
		return nil, nil, err
	}
	return resp, qm, nil
}

// AdminList lists the admin network policies in evaluation order.
func (n *NetworkPolicies) AdminList(q *QueryOptions) ([]*AdminNetworkPolicy, *QueryMeta, error) {
	var resp []*AdminNetworkPolicy
	qm, err := n.client.query("/v1/admin-network-policies", &resp, q)
	if err != nil {
		return nil, nil, err
	}
	return resp, qm, nil
}

// AdminInfo returns an admin network policy.
func (n *NetworkPolicies) AdminInfo(name string, q *QueryOptions) (*AdminNetworkPolicy, *QueryMeta, error) {
	if name == "" {
		return nil, nil, errors.New("missing admin network policy name")
	}
	var resp AdminNetworkPolicy
	qm, err := n.client.query("/v1/admin-network-policy/"+url.PathEscape(name), &resp, q)
	if err != nil {
		return nil, nil, err
	}
	return &resp, qm, nil
}

// AdminRegister creates or updates an admin network policy.
func (n *NetworkPolicies) AdminRegister(policy *AdminNetworkPolicy, w *WriteOptions) (*WriteMeta, error) {
	if policy == nil || policy.Name == "" {
		return nil, errors.New("missing admin network policy name")
	}
	return n.client.put("/v1/admin-network-policy/"+url.PathEscape(policy.Name), policy, nil, w)
}

// AdminDelete deletes an admin network policy.
func (n *NetworkPolicies) AdminDelete(name string, w *WriteOptions) (*WriteMeta, error) {
	if name == "" {
		return nil, errors.New("missing admin network policy name")
	}
	return n.client.delete("/v1/admin-network-policy/"+url.PathEscape(name), nil, nil, w)
}

// AdminStatus lists the allocations an admin network policy selects.
func (n *NetworkPolicies) AdminStatus(name string, q *QueryOptions) ([]*NetworkPolicyAllocStatus, *QueryMeta, error) {
	if name == "" {
		return nil, nil, errors.New("missing admin network policy name")
	}
	var resp []*NetworkPolicyAllocStatus
	qm, err := n.client.query("/v1/admin-network-policy/"+url.PathEscape(name)+"/status", &resp, q)
	if err != nil {
		return nil, nil, err
	}
	return resp, qm, nil
}
