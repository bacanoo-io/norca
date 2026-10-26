[[- /*
SPDX-FileCopyrightText: 2026 Norca contributors
SPDX-License-Identifier: MPL-2.0
*/ -]]
[[- $v := vars . ]]
[[- if $v.admission_policy_enabled ]]
name        = "[[ $v.job_name ]]-service-marks"
description = "Services marked for the LoadBalancer operator must name a valid frontend port (norca_lb_port)"
action      = [[ $v.admission_policy_action | quote ]]

match {
  resources = ["job"]
  [[- if ne $v.namespace "*" ]]
  namespaces = [[ list $v.namespace | toStringList ]]
  [[- end ]]
}

rule {
  expression = "job.TaskGroups.all(g, (!has(g.Services) || g.Services == null || g.Services.all(s, [[ template "lb.port_mark_ok" . ]])) && (!has(g.Tasks) || g.Tasks == null || g.Tasks.all(t, !has(t.Services) || t.Services == null || t.Services.all(s, [[ template "lb.port_mark_ok" . ]]))))"
  message    = "the norca_lb_port meta of a service must be a port number between 1 and 65535"
}
[[- end ]]
