// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"fmt"
	"sort"
	"strings"

	"github.com/hashicorp/nomad/nomad/structs"
)

// jobConsulProviderHook warns when a job registers services in Consul but no
// node of the region runs a Consul agent (a region without nodes yet gets no
// warning). Such services get the implicit ${attr.consul.version}
// constraint, so the job is not placed anywhere; the warning names the
// services and the way out instead of leaving the operator with a placement
// failure.
type jobConsulProviderHook struct {
	srv *Server
}

func (jobConsulProviderHook) Name() string {
	return "consul-provider"
}

func (h jobConsulProviderHook) Validate(job *structs.Job) ([]error, error) {
	var names []string
	for _, tg := range job.TaskGroups {
		for _, s := range tg.ConsulServices() {
			names = append(names, s.Name)
		}
	}
	if len(names) == 0 {
		return nil, nil
	}

	found, err := h.consulNodeExists()
	if err != nil {
		return nil, err
	}
	if found {
		return nil, nil
	}

	sort.Strings(names)
	return []error{fmt.Errorf(
		"services %s use provider \"consul\" (set, or the default of a Connect block), but no node of region %q runs a Consul agent (attribute consul.version): the job cannot be placed. Use provider = \"nomad\" for the native catalog, or install Consul (the consul pack of the pack registry)",
		strings.Join(names, ", "), h.srv.Region())}, nil
}

// consulNodeExists reports whether a node of the region has fingerprinted a
// Consul agent, or the region has no nodes yet. The search stops at the
// first one.
func (h jobConsulProviderHook) consulNodeExists() (bool, error) {
	iter, err := h.srv.State().Nodes(nil)
	if err != nil {
		return false, err
	}
	nodes := 0
	for raw := iter.Next(); raw != nil; raw = iter.Next() {
		nodes++
		if node := raw.(*structs.Node); node.Attributes["consul.version"] != "" {
			return true, nil
		}
	}
	return nodes == 0, nil
}
