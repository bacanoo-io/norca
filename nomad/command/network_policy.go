// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package command

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsimple"
	"github.com/hashicorp/nomad/api"
	"github.com/mitchellh/cli"
	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/convert"
)

type NetworkPolicyCommand struct {
	Meta
}

func (c *NetworkPolicyCommand) Name() string {
	return "network-policy"
}

func (c *NetworkPolicyCommand) Synopsis() string {
	return "Interact with network policies"
}

func (c *NetworkPolicyCommand) Help() string {
	helpText := `
Usage: norca network-policy <subcommand> [options] [args]

  This command groups subcommands for interacting with network policies.
  A network policy restricts the ingress and egress traffic of the
  allocations of a namespace selected by their job and task group meta. The
  rules are enforced with nftables in the network namespace of the
  allocation (bridge and CNI network modes). With -admin, the subcommands
  manage the cluster level admin network policies, whose allow and deny
  rules namespace policies cannot override.

  Create or update a network policy:

    $ norca network-policy apply <path>

  List the network policies of a namespace:

    $ norca network-policy list -namespace prod

  Show a network policy and the allocations it selects:

    $ norca network-policy status -namespace prod <name>

  Delete a network policy:

    $ norca network-policy delete -namespace prod <name>

  Please refer to individual subcommand help for detailed usage information.
`
	return strings.TrimSpace(helpText)
}

func (c *NetworkPolicyCommand) Run(args []string) int {
	return cli.RunResultHelp
}

// networkPolicyFile is the HCL form of network policies and admin network
// policies.
type networkPolicyFile struct {
	Name        string                   `hcl:"name"`
	Namespace   string                   `hcl:"namespace,optional"`
	Description string                   `hcl:"description,optional"`
	Selector    *networkPolicyFileMap    `hcl:"selector,block"`
	Ingress     []*networkPolicyFileRule `hcl:"ingress,block"`
	Egress      []*networkPolicyFileRule `hcl:"egress,block"`
	DefaultDeny []string                 `hcl:"default_deny,optional"`
	Enforcement string                   `hcl:"enforcement,optional"`
	Priority    *int                     `hcl:"priority,optional"`
	Namespaces  []string                 `hcl:"namespaces,optional"`
}

type networkPolicyFileRule struct {
	Name   string                   `hcl:"name,optional"`
	Action string                   `hcl:"action,optional"`
	From   []*networkPolicyFilePeer `hcl:"from,block"`
	To     []*networkPolicyFilePeer `hcl:"to,block"`
	Ports  []string                 `hcl:"ports,optional"`
}

type networkPolicyFilePeer struct {
	CIDR      []string              `hcl:"cidr,optional"`
	Namespace string                `hcl:"namespace,optional"`
	Selector  *networkPolicyFileMap `hcl:"selector,block"`
}

// networkPolicyFileMap is a block of arbitrary string attributes, e.g.
// selector { app = "postgres" }.
type networkPolicyFileMap struct {
	Body hcl.Body `hcl:",remain"`
}

func (m *networkPolicyFileMap) decode() (map[string]string, error) {
	if m == nil {
		return nil, nil
	}
	attrs, diags := m.Body.JustAttributes()
	if diags.HasErrors() {
		return nil, diags
	}
	out := make(map[string]string, len(attrs))
	for name, attr := range attrs {
		val, diags := attr.Expr.Value(nil)
		if diags.HasErrors() {
			return nil, diags
		}
		str, err := convert.Convert(val, cty.String)
		if err != nil || str.IsNull() {
			return nil, fmt.Errorf("selector %q: value must be a string", name)
		}
		out[name] = str.AsString()
	}
	return out, nil
}

func (r *networkPolicyFileRule) toAPI(direction string) (*api.NetworkPolicyRule, error) {
	peers, other := r.From, r.To
	block, otherBlock := "from", "to"
	if direction == "egress" {
		peers, other = r.To, r.From
		block, otherBlock = "to", "from"
	}
	if len(other) > 0 {
		return nil, fmt.Errorf("%s rules use %q blocks, not %q", direction, block, otherBlock)
	}
	rule := &api.NetworkPolicyRule{
		Name:   r.Name,
		Action: r.Action,
		Ports:  r.Ports,
	}
	for _, p := range peers {
		selector, err := p.Selector.decode()
		if err != nil {
			return nil, err
		}
		rule.Peers = append(rule.Peers, &api.NetworkPolicyPeer{
			CIDR:      p.CIDR,
			Namespace: p.Namespace,
			Selector:  selector,
		})
	}
	return rule, nil
}

func (f *networkPolicyFile) rules() (ingress, egress []*api.NetworkPolicyRule, err error) {
	for _, r := range f.Ingress {
		rule, err := r.toAPI("ingress")
		if err != nil {
			return nil, nil, err
		}
		ingress = append(ingress, rule)
	}
	for _, r := range f.Egress {
		rule, err := r.toAPI("egress")
		if err != nil {
			return nil, nil, err
		}
		egress = append(egress, rule)
	}
	return ingress, egress, nil
}

// decodeNetworkPolicyFile decodes the HCL form of a policy.
func decodeNetworkPolicyFile(path string, content []byte) (*networkPolicyFile, error) {
	if !strings.HasSuffix(path, ".hcl") && !strings.HasSuffix(path, ".json") {
		path += ".hcl"
	}
	var file networkPolicyFile
	if err := hclsimple.Decode(path, content, nil, &file); err != nil {
		return nil, err
	}
	return &file, nil
}

// parseNetworkPolicy parses a network policy specification in HCL or, if
// jsonInput is set, in the API JSON form.
func parseNetworkPolicy(path string, content []byte, jsonInput bool) (*api.NetworkPolicy, error) {
	if jsonInput {
		var policy api.NetworkPolicy
		if err := json.Unmarshal(content, &policy); err != nil {
			return nil, err
		}
		return &policy, nil
	}
	file, err := decodeNetworkPolicyFile(path, content)
	if err != nil {
		return nil, err
	}
	if file.Priority != nil || len(file.Namespaces) > 0 {
		return nil, errors.New(`"priority" and "namespaces" are only valid for admin network policies (-admin)`)
	}
	selector, err := file.Selector.decode()
	if err != nil {
		return nil, err
	}
	ingress, egress, err := file.rules()
	if err != nil {
		return nil, err
	}
	return &api.NetworkPolicy{
		Name:        file.Name,
		Namespace:   file.Namespace,
		Description: file.Description,
		Selector:    selector,
		Ingress:     ingress,
		Egress:      egress,
		DefaultDeny: file.DefaultDeny,
		Enforcement: file.Enforcement,
	}, nil
}

// parseAdminNetworkPolicy parses an admin network policy specification in
// HCL or, if jsonInput is set, in the API JSON form.
func parseAdminNetworkPolicy(path string, content []byte, jsonInput bool) (*api.AdminNetworkPolicy, error) {
	if jsonInput {
		var policy api.AdminNetworkPolicy
		if err := json.Unmarshal(content, &policy); err != nil {
			return nil, err
		}
		return &policy, nil
	}
	file, err := decodeNetworkPolicyFile(path, content)
	if err != nil {
		return nil, err
	}
	if file.Namespace != "" || len(file.DefaultDeny) > 0 {
		return nil, errors.New(`"namespace" and "default_deny" are not valid for admin network policies; use "namespaces" and a deny rule`)
	}
	selector, err := file.Selector.decode()
	if err != nil {
		return nil, err
	}
	ingress, egress, err := file.rules()
	if err != nil {
		return nil, err
	}
	policy := &api.AdminNetworkPolicy{
		Name:        file.Name,
		Description: file.Description,
		Namespaces:  file.Namespaces,
		Selector:    selector,
		Ingress:     ingress,
		Egress:      egress,
		Enforcement: file.Enforcement,
	}
	if file.Priority != nil {
		policy.Priority = *file.Priority
	}
	return policy, nil
}

// formatNetworkPolicySelector formats a selector as k=v pairs.
func formatNetworkPolicySelector(selector map[string]string) string {
	if len(selector) == 0 {
		return "<all>"
	}
	keys := make([]string, 0, len(selector))
	for k := range selector {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + "=" + selector[k]
	}
	return strings.Join(parts, ",")
}

// formatNetworkPolicyRules formats the rules of a direction, one per line.
func formatNetworkPolicyRules(direction string, rules []*api.NetworkPolicyRule) []string {
	var out []string
	for i, rule := range rules {
		name := rule.Name
		if name == "" {
			name = fmt.Sprintf("#%d", i)
		}
		action := rule.Action
		if action == "" {
			action = api.NetworkPolicyActionAllow
		}
		peers := make([]string, 0, len(rule.Peers))
		for _, p := range rule.Peers {
			switch {
			case len(p.CIDR) > 0:
				peers = append(peers, strings.Join(p.CIDR, ","))
			default:
				ns := p.Namespace
				if ns == "" {
					ns = "<same>"
				}
				peers = append(peers, fmt.Sprintf("namespace=%s selector=%s", ns, formatNetworkPolicySelector(p.Selector)))
			}
		}
		peerText := "<any>"
		if len(peers) > 0 {
			peerText = strings.Join(peers, "; ")
		}
		ports := "<any>"
		if len(rule.Ports) > 0 {
			ports = strings.Join(rule.Ports, ",")
		}
		out = append(out, fmt.Sprintf("%s|%s|%s|%s|%s", direction, name, action, peerText, ports))
	}
	return out
}

// formatNetworkPolicyAllocs formats the allocation status list.
func formatNetworkPolicyAllocs(allocs []*api.NetworkPolicyAllocStatus, length int) string {
	if len(allocs) == 0 {
		return "No allocations selected"
	}
	rows := []string{"ID|Namespace|Job|Group|Node|Mode|Enforceable|Reason"}
	for _, a := range allocs {
		node := a.NodeName
		if node == "" {
			node = limit(a.NodeID, length)
		}
		rows = append(rows, fmt.Sprintf("%s|%s|%s|%s|%s|%s|%t|%s",
			limit(a.ID, length), a.Namespace, a.JobID, a.TaskGroup, node, a.NetworkMode, a.Enforceable, a.Reason))
	}
	return formatList(rows)
}
