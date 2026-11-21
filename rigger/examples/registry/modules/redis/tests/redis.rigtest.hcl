# What the pack renders, as assertions over the evaluated resources: the
# comparison of design 14 in small.

test "defaults" {
  release = "cache"

  assert {
    condition     = resource.job.redis.id == "redis"
    error_message = "the job is named by job_name, as in the pack, not by the release"
  }

  assert {
    condition     = resource.job.redis.group.redis.task.redis.config.args == null
    error_message = "without persistence the container runs with the arguments of its image"
  }

  assert {
    condition     = length(resource.variable) == 0
    error_message = "without authentication there is no Variable"
  }
}

test "persistence" {
  values = {
    persistence_enabled = true
  }

  assert {
    condition     = resource.job.redis.group.redis.task.redis.config.args == ["redis-server", "--appendonly", "yes"]
    error_message = "AOF is turned on by the arguments"
  }
}

test "authentication" {
  values = {
    auth_enabled   = true
    redis_password = "test"
  }

  assert {
    condition     = resource.variable.auth.path == "nomad/jobs/redis"
    error_message = "the Variable has the path of the pack"
  }

  assert {
    condition     = issensitive(resource.variable.auth.items.password)
    error_message = "the password is a secret item, not a value of the revision"
  }
}

test "a password without authentication is refused" {
  values = {
    redis_password = "test"
  }

  expect_error = "redis_password is written only with auth_enabled = true"
}

test "an unknown mode is refused" {
  values = {
    mode = "cluster"
  }

  expect_error = "not one of \"standalone\", \"replica\", \"sentinel\""
}
