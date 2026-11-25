# SPDX-FileCopyrightText: 2026 Norca contributors
# SPDX-License-Identifier: MPL-2.0

# What the module renders, as assertions over the evaluated resources.
# `rigger module test` runs them without a cluster.

test "defaults" {
  release = "web"

  assert {
    condition     = resource.job.test-app.id == "test-app" && resource.job.test-app.name == "test-app"
    error_message = "the job is named by job_name, not by the release"
  }

  assert {
    condition     = resource.job.test-app.meta.app == "test-app" && resource.job.test-app.meta.team == "demo"
    error_message = "the job meta carries app and team, which the policies of the examples select by"
  }

  assert {
    condition     = resource.job.test-app.group.web.count == 2
    error_message = "two instances by default"
  }

  assert {
    condition     = resource.job.test-app.group.web.network.mode == "bridge" && resource.job.test-app.group.web.network.port.http.to == 80
    error_message = "bridge networking, a port of the node mapped to port 80 of nginx"
  }

  assert {
    condition     = resource.job.test-app.group.web.disruption[0].max_unavailable == "1"
    error_message = "the disruption budget allows one instance down"
  }

  assert {
    condition     = resource.job.test-app.group.web.scaling[0].min == 1 && resource.job.test-app.group.web.scaling[0].max == 4
    error_message = "the scaling policy has the default bounds"
  }

  assert {
    condition     = resource.job.test-app.group.web.scaling[0].policy.check[0].cpu[0].target == 70
    error_message = "the cpu check of the typed policy aims at scaling_cpu_target"
  }

  assert {
    condition     = resource.job.test-app.group.web.secret_access[0].path == "${runtime("job")}/*"
    error_message = "the tasks may read the secrets below the ID of the job"
  }

  assert {
    condition     = resource.job.test-app.group.web.task.nginx.secret.db.path == "${runtime("job")}/db"
    error_message = "the secret block delivers <job>/db"
  }

  assert {
    condition     = [for c in resource.job.test-app.group.web.service.check : c.role] == ["startup", "readiness", "liveness"]
    error_message = "the service has a check of each role"
  }

  assert {
    condition     = length(split("{{ with secret \"kv/data/test-app/db\" }}", resource.job.test-app.group.web.task.nginx.template[0].data)) > 1
    error_message = "the page reads the version of the secret with a Vault-style template"
  }

  assert {
    condition     = length(resource.job.test-app.multiregion) == 0 && length(resource.job.test-app.group.web.separate) == 0
    error_message = "no multiregion and no separate block by default"
  }
}

test "host networking of a development agent" {
  values = {
    count            = 1
    network_mode     = "host"
    discovery_prefer = "datacenter"
  }

  assert {
    condition     = resource.job.test-app.group.web.network.mode == "host"
    error_message = "the group uses the network mode of the values"
  }

  assert {
    condition     = resource.job.test-app.group.web.task.nginx.config.ports == ["http"]
    error_message = "in host mode Docker maps the port"
  }

  assert {
    condition     = resource.job.test-app.group.web.service.discovery[0].prefer == "datacenter"
    error_message = "the service has a discovery block"
  }
}

test "behind an mTLS sidecar, without secrets and scaling" {
  values = {
    job_name        = "test-api"
    mtls_sidecar    = true
    secrets_enabled = false
    scaling_enabled = false
    separate_scope  = "$${node.unique.id}"
  }

  assert {
    condition     = resource.job.test-app.id == "test-api" && resource.job.test-app.group.web.service.name == "test-api"
    error_message = "job_name names the job and the service"
  }

  assert {
    condition     = resource.job.test-app.group.web.service.mtls[0].mode == "sidecar" && resource.job.test-app.group.web.service.port == "80"
    error_message = "the service is behind a sidecar and names the port of nginx"
  }

  assert {
    condition     = length(resource.job.test-app.group.web.network.port) == 0
    error_message = "in sidecar mode no port is mapped to the host"
  }

  assert {
    condition     = alltrue([for c in resource.job.test-app.group.web.service.check : c.address_mode == "alloc"])
    error_message = "the checks address the allocation"
  }

  assert {
    condition     = resource.job.test-app.group.web.separate[0].scope == "$${node.unique.id}" && resource.job.test-app.group.web.separate[0].selector.app == "test-api"
    error_message = "the separate block keeps the instances of the job on different nodes"
  }

  assert {
    condition     = length(resource.job.test-app.group.web.scaling) == 0 && length(resource.job.test-app.group.web.secret_access) == 0 && length(resource.job.test-app.group.web.task.nginx.secret) == 0
    error_message = "no scaling policy and no secret without them"
  }
}

test "a multiregion deployment" {
  values = {
    multiregion = [
      { name = "eu-1", count = 3 },
      { name = "eu-2", count = 2 },
    ]
    scaling_enabled    = false
    discovery_prefer   = "datacenter"
    discovery_failover = ["eu-1", "eu-2"]
  }

  assert {
    condition     = resource.job.test-app.multiregion[0].region.eu-1.count == 3 && resource.job.test-app.multiregion[0].region.eu-2.count == 2
    error_message = "every region of the values has its count"
  }

  assert {
    condition     = resource.job.test-app.multiregion[0].strategy.max_parallel == 1 && resource.job.test-app.multiregion[0].strategy.on_failure == "fail_all"
    error_message = "the strategy has its defaults"
  }

  assert {
    condition     = resource.job.test-app.group.web.count == 0
    error_message = "the group count is 0: every region uses its own"
  }

  assert {
    condition     = resource.job.test-app.group.web.service.discovery[0].failover == ["eu-1", "eu-2"]
    error_message = "lookups fail over to the regions of the values"
  }
}

test "a count outside the scaling bounds is refused" {
  values = {
    count = 6
  }

  expect_error = "count must be between scaling_min and scaling_max"
}

test "an image without a pinned tag is refused" {
  values = {
    image = "nginx:latest"
  }

  expect_error = "pin a tag or a digest"
}

test "a sidecar without bridge networking is refused" {
  values = {
    network_mode = "host"
    mtls_sidecar = true
  }

  expect_error = "mtls_sidecar needs network_mode"
}

test "multiregion with the autoscaler is refused" {
  values = {
    multiregion = [{ name = "eu-1", count = 1 }]
  }

  expect_error = "scaling_enabled must be false with multiregion"
}

test "an unknown network mode is refused" {
  values = {
    network_mode = "overlay"
  }

  expect_error = "not one of \"bridge\", \"host\""
}
