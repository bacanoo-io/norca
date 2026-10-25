// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"fmt"
	"strings"

	"github.com/hashicorp/nomad/nomad/structs"
)

// jobNetworkPolicyHook warns when network policies select a task group
// whose network mode cannot enforce them. With enforcement "required" the
// scheduler will not place such groups; with "best_effort" they run
// without the rules.
type jobNetworkPolicyHook struct {
	srv *Server
}

func (jobNetworkPolicyHook) Name() string {
	return "network-policy"
}

func (h jobNetworkPolicyHook) Validate(job *structs.Job) ([]error, error) {
	store := h.srv.State()
	var warnings []error
	for _, tg := range job.TaskGroups {
		mode := structs.NetworkPolicyGroupMode(tg)
		if structs.NetworkPolicyModeEnforceable(mode) {
			continue
		}
		policies, admin, err := store.NetworkPoliciesForGroup(nil, job.Namespace, structs.NetworkPolicyMeta(job, tg))
		if err != nil {
			return nil, err
		}
		var required, bestEffort []string
		for _, p := range policies {
			if p.Required() {
				required = append(required, p.Name)
			} else {
				bestEffort = append(bestEffort, p.Name)
			}
		}
		for _, p := range admin {
			if p.Required() {
				required = append(required, "admin/"+p.Name)
			} else {
				bestEffort = append(bestEffort, "admin/"+p.Name)
			}
		}
		if len(required) > 0 {
			warnings = append(warnings, fmt.Errorf(
				"task group %q is selected by network policies requiring enforcement (%s) but network mode %q cannot enforce them; it will not be placed",
				tg.Name, strings.Join(required, ", "), mode))
		}
		if len(bestEffort) > 0 {
			warnings = append(warnings, fmt.Errorf(
				"task group %q is selected by network policies (%s) but network mode %q cannot enforce them; they will have no effect",
				tg.Name, strings.Join(bestEffort, ", "), mode))
		}
	}
	return warnings, nil
}
