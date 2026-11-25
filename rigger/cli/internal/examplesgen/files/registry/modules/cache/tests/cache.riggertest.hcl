test "defaults" {
  release = "cache"

  assert {
    condition     = resource.job.cache.id == "cache" && resource.job.cache.group.cache.count == 1
    error_message = "one server, named after the release"
  }

  assert {
    condition     = can(regex("maxmemory 204mb", resource.job.cache.group.cache.task.valkey.template.data))
    error_message = "Valkey evicts at 80 % of the memory"
  }

  assert {
    condition     = can(regex("appendonly no", resource.job.cache.group.cache.task.valkey.template.data))
    error_message = "without persistence nothing is written to disk"
  }

  assert {
    condition     = length(resource.job.cache.group.cache.volume) == 0
    error_message = "without persistence there is no volume"
  }

  assert {
    condition     = output.connection == { service = "cache", port = 6379, password_secret = "cache/password" }
    error_message = "the connection names the service, the port and the secret"
  }
}

test "persistence" {
  values = {
    persistence = true
    host_volume = "cache-data"
  }

  assert {
    condition     = resource.job.cache.group.cache.volume.data.source == "cache-data"
    error_message = "the data is on the host volume"
  }

  assert {
    condition     = can(regex("appendonly yes", resource.job.cache.group.cache.task.valkey.template.data))
    error_message = "the append-only file is on"
  }
}

test "an unknown eviction is refused" {
  values = {
    eviction = "random"
  }

  expect_error = "not one of"
}
