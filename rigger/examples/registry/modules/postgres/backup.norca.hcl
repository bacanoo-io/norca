# Periodic dumps to an S3 store, and the policy that lets the dump job read
# its own secrets and nothing else.

job "backup" {
  enabled = var.backup.enabled

  id   = "${local.job}-backup"
  name = "${local.job}-backup"
  type = "batch"

  periodic {
    crons            = [var.backup.schedule]
    prohibit_overlap = true
  }

  group "backup" {
    secret_access {
      path = local.superuser_secret
    }

    secret_access {
      path = var.backup.s3_secret
    }

    task "dump" {
      driver = "docker"

      config {
        image   = local.image
        command = "sh"
        args    = ["-ec", file("files/backup.sh")]
      }

      env = {
        PGPORT    = tostring(var.port)
        PGUSER    = "postgres"
        S3_BUCKET = var.backup.enabled ? var.backup.s3.bucket : ""
        KEEP_DAYS = tostring(var.backup.keep_days)
        PREFIX    = "${rig.region}/${local.job}"
      }

      template {
        destination = "secrets/backup.env"
        env         = true
        data        = <<-EOT
          PGPASSWORD=${secret_ref(local.superuser_secret, "password", { quote = false })}
          AWS_ACCESS_KEY_ID=${secret_ref(var.backup.s3_secret, "access_key", { quote = false })}
          AWS_SECRET_ACCESS_KEY=${secret_ref(var.backup.s3_secret, "secret_key", { quote = false })}
          PGHOST=${service_address(local.job)}
          S3_ENDPOINT=http://${service_address(var.backup.enabled ? var.backup.s3.service : "")}
        EOT
      }

      resources {
        cpu    = 200
        memory = 256
      }
    }
  }
}
