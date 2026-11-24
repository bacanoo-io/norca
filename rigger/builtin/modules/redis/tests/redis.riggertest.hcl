# What the module renders, as assertions over the evaluated resources.

test "defaults" {
  release = "cache"

  assert {
    condition     = resource.job.redis.id == "redis"
    error_message = "the job is named by job_name, not by the release"
  }

  assert {
    condition     = resource.job.redis.group.redis.count == 1
    error_message = "the module runs one server"
  }

  assert {
    condition     = resource.job.redis.group.redis.service.name == "redis" && resource.job.redis.group.redis.service.tags == ["cache", "redis"]
    error_message = "the service is registered under job_name with service_tags"
  }

  assert {
    condition     = resource.job.redis.group.redis.task.redis.config.args == ["redis-server", "/secrets/redis.conf"]
    error_message = "the server reads the configuration file of the module"
  }

  assert {
    condition     = length(split("appendonly no\nsave \"\"\n", resource.job.redis.group.redis.task.redis.template.data)) > 1
    error_message = "without persistence nothing is written to disk"
  }

  assert {
    condition     = length(split("requirepass", resource.job.redis.group.redis.task.redis.template.data)) == 1
    error_message = "without authentication the configuration has no password"
  }

  assert {
    condition     = length(resource.variable) == 0
    error_message = "without authentication there is no Variable"
  }
}

test "persistence" {
  values = {
    persistence_enabled = true
    host_volume         = "redis-data"
  }

  assert {
    condition     = length(split("dir /data\nappendonly yes\n", resource.job.redis.group.redis.task.redis.template.data)) > 1
    error_message = "AOF is turned on in the configuration, in /data"
  }

  assert {
    condition     = resource.job.redis.group.redis.volume.data.source == "redis-data"
    error_message = "the data is on the host volume"
  }

  assert {
    condition     = resource.job.redis.group.redis.task.redis.volume_mount[0].destination == "/data"
    error_message = "the host volume is mounted at /data"
  }
}

test "authentication" {
  values = {
    job_name       = "kv"
    auth_enabled   = true
    redis_password = "test"
  }

  assert {
    condition     = resource.variable.auth.path == "nomad/jobs/kv"
    error_message = "the Variable is at the path the job kv reads by its workload identity"
  }

  assert {
    condition     = issensitive(resource.variable.auth.items.password)
    error_message = "the password is a secret item, not a value of the revision"
  }

  assert {
    condition     = length(split("requirepass \"{{ with nomadVar \"nomad/jobs/kv\" }}", resource.job.redis.group.redis.task.redis.template.data)) > 1
    error_message = "the server requires the password of the Variable"
  }

  assert {
    condition     = length(split("test", resource.job.redis.group.redis.task.redis.template.data)) == 1
    error_message = "the password is not in the job"
  }
}

test "environment" {
  values = {
    extra_env = { TZ = "UTC" }
  }

  assert {
    condition     = resource.job.redis.group.redis.task.redis.env.TZ == "UTC"
    error_message = "extra_env is the environment of the task"
  }
}

test "host network" {
  values = {
    network_mode = "host"
  }

  assert {
    condition     = resource.job.redis.group.redis.network.port.db.to == null
    error_message = "in host mode no port is mapped"
  }
}

test "a password without authentication is refused" {
  values = {
    redis_password = "test"
  }

  expect_error = "redis_password is written only with auth_enabled = true"
}

test "authentication without a password is refused" {
  values = {
    auth_enabled = true
  }

  expect_error = "auth_enabled = true needs redis_password"
}

test "an unknown network mode is refused" {
  values = {
    network_mode = "overlay"
  }

  expect_error = "not one of \"bridge\", \"host\""
}
