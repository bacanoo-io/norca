# The application databases: a hook that runs after every install and
# upgrade, and makes what is missing. Running it again changes nothing.
job "databases" {
  enabled = length(var.databases) > 0

  name = "${local.job}-databases"
  type = "batch"

  hook {
    on      = ["post-install", "post-upgrade"]
    weight  = 10
    timeout = "5m"
    delete  = "on-success"
  }

  constraint {
    attribute = runtime("node.unique.name")
    value     = var.node
  }

  group "databases" {
    # The superuser and the owner of each database.
    dynamic "secret_access" {
      for_each = concat([local.superuser_secret], values(local.app_secret))
      content {
        path = secret_access.value
      }
    }

    task "create" {
      driver = "docker"

      config {
        image   = local.image
        command = "sh"
        args    = ["-ec", "for f in ${runtime_env("NORCA_TASK_DIR")}/sql/*.sql; do psql -v ON_ERROR_STOP=1 -f \"$f\"; done"]
      }

      env = {
        PGPORT = tostring(var.port)
        PGUSER = "postgres"
      }

      # The address of the server comes from the native catalog, the
      # password from the secret store, both when the task starts.
      template {
        destination = "secrets/pg.env"
        env         = true
        data        = <<-EOT
          PGHOST=${service_address(local.job)}
          PGPASSWORD=${secret_ref(local.superuser_secret, "password", { quote = false })}
        EOT
      }

      # One SQL file per database; the owner's password is read from the
      # secret store when the file is written on the node.
      dynamic "template" {
        for_each = local.database_sql
        content {
          destination = "local/sql/${template.key}.sql"
          data        = replace(template.value, "@@PASSWORD@@", secret_ref(local.app_secret[template.key], "password", { quote = "sql" }))
        }
      }

      resources {
        cpu    = 100
        memory = 128
      }
    }
  }
}
