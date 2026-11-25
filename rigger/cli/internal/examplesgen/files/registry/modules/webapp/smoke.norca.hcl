# A hook: a job with a hook block runs at a phase of the release instead of
# staying deployed. on is one phase or a list of them: pre-install,
# post-install, pre-upgrade, post-upgrade, pre-delete, post-delete, test.
# weight orders the hooks of one phase; timeout bounds the run; delete is
# on-success, always or never. A failed post-install hook fails the release
# (and an atomic one rolls back).

job "smoke" {
  # enabled = false leaves the resource out of the release.
  enabled = var.smoke_test

  id   = "${local.name}-smoke"
  name = "${local.name}-smoke"
  type = "batch"

  hook {
    on      = ["post-install", "post-upgrade"]
    weight  = 10
    timeout = "2m"
    delete  = "on-success"
  }

  group "smoke" {
    restart {
      attempts = 3
      delay    = "10s"
      mode     = "fail"
    }

    task "check" {
      driver = "docker"

      config {
        image   = "curlimages/curl:8.11.1"
        command = "sh"
        # A plain file of the module, read with file().
        args = ["-ec", file("files/smoke.sh")]
      }

      # The address of one instance, from the service catalog when the task
      # starts.
      template {
        destination = "local/target.env"
        env         = true
        data        = <<-EOT
          TARGET=http://${service_address(local.name)}${var.health_path}
        EOT
      }

      resources {
        cpu    = 50
        memory = 32
      }
    }
  }
}
