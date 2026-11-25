# Tests: "rigger module test" evaluates the module with the values of each
# test and checks the assertions over what it renders. No cluster is
# needed. A test takes release, namespace and region (default: the module
# name, default, global), values, and either assert blocks or
# expect_error (a part of the error message).
#
# An assertion reads resource.<kind>.<label> (kinds: job, policy, variable,
# namespace, quota, kind, admission, external, object), a resource as its
# block is written: a block written once is an object, a block written more
# than once or a dynamic block is a list. output.<name> reads an output.

test "defaults" {
  release = "web"

  assert {
    condition     = resource.job.webapp.id == "web" && resource.job.webapp.group.web.count == 2
    error_message = "the job is named after the release and runs two instances"
  }

  assert {
    condition     = resource.job.webapp.group.web.task.web.resources.cpu == 100 && resource.job.webapp.group.web.task.web.resources.memory == 64
    error_message = "the default size is small"
  }

  assert {
    condition     = resource.job.webapp.group.web.task.web.resources.memory_max == null
    error_message = "no memory above the reservation unless oversubscription is asked for"
  }

  assert {
    condition     = resource.job.webapp.group.web.service.tags == []
    error_message = "without a route the service has no tags"
  }

  assert {
    condition     = length(resource.job.webapp.group.web.secret_access) == 1
    error_message = "without a cache the task reads one secret, the session key"
  }

  assert {
    condition     = length(resource.job) == 1
    error_message = "the smoke test is off by default"
  }

  assert {
    condition     = resource.variable.settings.path == "apps/web/settings" && resource.variable.settings.items.log_level == "info"
    error_message = "the settings Variable is the release's"
  }

  assert {
    condition     = resource.policy.webapp.job_acl.job_id == "web"
    error_message = "the policy is bound to the job"
  }

  assert {
    condition     = output.url == null
    error_message = "no route, no url"
  }
}

test "a public route" {
  release = "shop"

  values = {
    public                  = true
    hostname                = "shop.example.com"
    size                    = "large"
    count                   = 3
    memory_oversubscription = true
  }

  assert {
    condition     = contains(resource.job.webapp.group.web.service.tags, "traefik.http.routers.shop.rule=Host(`shop.example.com`)")
    error_message = "the route answers the host name"
  }

  assert {
    condition     = length(resource.job.webapp.group.web.spread) == 1
    error_message = "three instances are spread over the datacenters"
  }

  assert {
    condition     = resource.job.webapp.group.web.task.web.resources.memory_max == 2048
    error_message = "a large instance may use twice its reservation"
  }

  assert {
    condition     = output.url == "https://shop.example.com"
    error_message = "the url is that of the route"
  }
}

test "features in the configuration file" {
  values = {
    features = { new_checkout = true, dark_mode = false }
  }

  assert {
    condition     = can(regex("new_checkout = true", resource.job.webapp.group.web.task.web.template[0].data))
    error_message = "the features are written into app.toml"
  }

  assert {
    condition     = resource.variable.settings.items.features == "new_checkout"
    error_message = "the settings list the features that are on"
  }
}

test "a cache" {
  values = {
    cache_enabled = true
    cache         = { service = "kv", password_secret = "kv/password" }
  }

  assert {
    condition     = resource.job.webapp.group.web.task.web.env.CACHE_SERVICE == "kv"
    error_message = "the service of the cache reaches the environment"
  }

  assert {
    condition     = length(resource.job.webapp.group.web.secret_access) == 2
    error_message = "the task may read the password of the cache"
  }
}

test "the api token is a secret item" {
  values = {
    api_token = "not-a-real-token"
  }

  assert {
    condition     = issensitive(resource.variable.settings.items.api_token)
    error_message = "the token is a secret item, never a value of the revision"
  }
}

test "the smoke test is a hook" {
  values = {
    smoke_test = true
  }

  assert {
    condition     = resource.job.smoke.hook.on == ["post-install", "post-upgrade"]
    error_message = "the smoke test runs after every install and upgrade"
  }
}

test "public without a hostname is refused" {
  values = {
    public = true
  }

  expect_error = "public needs hostname"
}

test "an unknown size is refused" {
  values = {
    size = "huge"
  }

  expect_error = "not one of"
}

test "too much memory is refused" {
  values = {
    size  = "large"
    count = 20
  }

  expect_error = "reserve more than 16 GiB"
}
