# `rig module test` evaluates the module with the values of each test and
# checks the assertions against the resources it gives. No cluster is needed.

test "defaults" {
  release = "web"

  assert {
    condition     = resource.job.web.group.web.count == 2
    error_message = "the default is two instances"
  }

  assert {
    condition     = length(resource.job.web.group.web.secret_access) == 0
    error_message = "without a database the task reads no secret"
  }
}

test "with a database" {
  release = "shop"

  values = {
    database        = { service = "postgres", name = "shop" }
    database_secret = "postgres/apps/shop"
  }

  assert {
    condition     = resource.job.web.name == "shop"
    error_message = "the job is named after the release"
  }

  assert {
    condition     = resource.job.web.group.web.task.web.env.DB_NAME == "shop"
    error_message = "the database name reaches the environment"
  }

  assert {
    condition     = resource.job.web.group.web.secret_access[0].path == "postgres/apps/shop"
    error_message = "the task may read the secret of the database"
  }
}

test "a database without its secret is refused" {
  values = {
    database = { service = "postgres", name = "shop" }
  }

  expect_error = "database needs database_secret"
}
