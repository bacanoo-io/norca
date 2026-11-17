test "the server alone" {
  values = { node = "worker-1" }

  assert {
    condition     = keys(resource.job) == ["postgres"]
    error_message = "without databases and backups the module is one job"
  }

  assert {
    condition     = length(resource.acl) == 0
    error_message = "no policy without the backup job"
  }
}

test "databases get a hook and their secrets" {
  values = {
    node      = "worker-1"
    databases = [{ name = "shop" }, { name = "reports", owner = "reporting" }]
  }

  assert {
    condition     = resource.job.databases.hook.on == ["post-install", "post-upgrade"]
    error_message = "the databases are made after every install and upgrade"
  }

  assert {
    condition     = output.databases.reports.secret == "postgres/apps/reports"
    error_message = "the output names the secret of each database"
  }

  assert {
    condition     = resource.var.info.items.databases == "shop,reports"
    error_message = "the info variable lists the databases"
  }
}

test "backups bring a periodic job and its policy" {
  values = {
    node   = "worker-1"
    backup = { enabled = true, s3 = { service = "rustfs", bucket = "backups" } }
  }

  assert {
    condition     = resource.job.backup.periodic.crons == ["15 2 * * *"]
    error_message = "the default schedule"
  }

  assert {
    condition     = resource.acl.backup.job_acl.job_id == "${rig.release}-backup"
    error_message = "the policy is bound to the dump job"
  }
}

test "backups without an S3 store are refused" {
  values = {
    node   = "worker-1"
    backup = { enabled = true }
  }
  expect_error = "backup.enabled needs backup.s3"
}

test "a reserved database name is refused" {
  values = {
    node      = "worker-1"
    databases = [{ name = "postgres" }]
  }
  expect_error = "belong to the server"
}
