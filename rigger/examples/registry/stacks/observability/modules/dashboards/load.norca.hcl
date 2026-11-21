# A hook: a batch job that runs after every install and upgrade of the
# release and loads the chosen dashboard sets.

job "load" {
  id   = "${rig.release}-load"
  name = "${rig.release}-load"
  type = "batch"

  hook {
    on      = ["post-install", "post-upgrade"]
    timeout = "3m"
    delete  = "on-success"
  }

  group "load" {
    secret_access {
      path = var.openobserve.secret
    }

    task "load" {
      driver = "docker"

      config {
        image   = var.image
        command = "sh"
        args    = ["-ec", "for f in ${runtime_env("NORCA_TASK_DIR")}/sets/*.json; do curl -fsS -u \"$OO_USER:$OO_PASSWORD\" -H 'Content-Type: application/json' -d @\"$f\" \"http://$OO_ADDR/api/default/dashboards\"; done"]
      }

      template {
        destination = "secrets/openobserve.env"
        env         = true
        data        = <<-EOT
          OO_ADDR=${service_address(var.openobserve.service)}
          OO_USER=${secret_ref(var.openobserve.secret, "user", { quote = false })}
          OO_PASSWORD=${secret_ref(var.openobserve.secret, "password", { quote = false })}
        EOT
      }

      # One file per chosen set, read from the module at install time.
      dynamic "template" {
        for_each = toset(var.sets)
        content {
          destination = "local/sets/${template.value}.json"
          data        = file("files/${template.value}.json")
        }
      }

      resources {
        cpu    = 50
        memory = 64
      }
    }
  }
}
