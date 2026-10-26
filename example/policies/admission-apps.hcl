# SPDX-FileCopyrightText: 2026 Norca contributors
# SPDX-License-Identifier: MPL-2.0

# CEL admission policy of the "apps" namespace, checked on job registration
# and plan (norca job run, job plan, pack run):
#   norca admission-policy apply admission-apps.hcl
#   norca admission-policy list
name        = "apps-baseline"
description = "Baseline rules for the jobs of the apps namespace"
action      = "deny" # or "warn": report the violations without rejecting

match {
  namespaces = ["apps"]
}

rule {
  expression = "!job.TaskGroups.exists(g, g.Tasks.exists(t, t.Driver == 'docker' && has(t.Config.privileged) && t.Config.privileged))"
  message    = "Privileged containers are not allowed in the apps namespace."
}

rule {
  expression = "job.TaskGroups.all(g, g.Count <= 10)"
  message    = "A task group of the apps namespace may have at most 10 instances."
}

# Secret blocks may only reference secrets below the name of their job.
rule {
  expression = "request.secrets.all(s, s.source != 'secret' || s.path.startsWith(job.ID + '/'))"
  message    = "Secret blocks may only read secrets below <job ID>/."
}
