job "nats" {
  id          = local.job
  name        = local.job
  type        = "service"
  datacenters = module.placement.datacenters
  node_pool   = module.placement.node_pool

  update {
    max_parallel     = 1
    min_healthy_time = "15s"
    healthy_deadline = "5m"
  }

  group "nats" {
    count = var.count

    # One server at a time may be down for a drain (a block of the group,
    # its limit a string: "1", "25%").
    disruption {
      max_unavailable = "1"
    }

    dynamic "spread" {
      for_each = module.placement.spread
      content {
        attribute = spread.value.attribute
        weight    = spread.value.weight
      }
    }

    # Two servers of one cluster never share a node.
    constraint {
      operator = "distinct_hosts"
      value    = "true"
    }

    network {
      port "client" {
        static = var.ports.client
      }

      port "monitor" {
        static = var.ports.monitor
      }

      dynamic "port" {
        for_each = local.clustered ? ["cluster"] : []
        labels   = [port.value]
        content {
          static = var.ports.cluster
        }
      }
    }

    # What the task may read from the secret store.
    dynamic "secret_access" {
      for_each = concat(
        [for u in var.users : "${var.users_secret_prefix}/${u.name}"],
        local.tls ? [var.tls_secret] : [],
        var.norca_bus == null ? [] : [var.norca_bus.secret],
      )
      content {
        path = secret_access.value
      }
    }

    # The route addresses: registered before the tasks start and without
    # checks, so the servers find each other on their first start.
    dynamic "service" {
      for_each = local.clustered ? [1] : []
      content {
        name     = "${local.job}-cluster"
        port     = "cluster"
        provider = "nomad"
      }
    }

    service {
      name     = local.job
      port     = "client"
      provider = "nomad"

      check {
        name     = "started"
        role     = "startup"
        type     = "http"
        port     = "monitor"
        path     = var.jetstream.enabled ? "/healthz?js-enabled-only=true" : "/healthz"
        interval = "5s"
        timeout  = "2s"
      }

      check {
        name     = "ready"
        role     = "readiness"
        type     = "http"
        port     = "monitor"
        path     = "/healthz"
        interval = "10s"
        timeout  = "2s"
      }

      check {
        name     = "alive"
        role     = "liveness"
        type     = "http"
        port     = "monitor"
        path     = "/healthz?js-server-only=true"
        interval = "30s"
        timeout  = "3s"
      }
    }

    task "nats" {
      driver = "docker"

      config {
        image = module.placement.image
        ports = local.clustered ? ["client", "cluster", "monitor"] : ["client", "monitor"]
        args  = ["-c", "${local.secrets}/nats.conf"]
      }

      # The whole server configuration, written from data.
      template {
        destination   = "secrets/nats.conf"
        perms         = "0600"
        change_mode   = "signal"
        change_signal = "SIGHUP"
        data          = natsconf(local.config)
      }

      # TLS material, one file per item of the secret.
      dynamic "template" {
        for_each = local.tls ? ["ca", "cert", "key"] : []
        content {
          destination   = "secrets/tls/${template.value}.pem"
          perms         = "0600"
          change_mode   = "signal"
          change_signal = "SIGHUP"
          data          = secret_ref(var.tls_secret, template.value, { quote = false })
        }
      }

      # The CA of the Norca servers for the leaf node connection.
      dynamic "template" {
        for_each = var.norca_bus == null ? [] : (var.norca_bus.tls ? [1] : [])
        content {
          destination = "secrets/norca_bus/ca.pem"
          perms       = "0600"
          data        = secret_ref(var.norca_bus.secret, "ca", { quote = false })
        }
      }

      # Lame duck: clients move to the other servers before the task stops.
      kill_signal  = "SIGUSR2"
      kill_timeout = "90s"

      resources {
        cpu    = var.resources.cpu
        memory = var.resources.memory
      }
    }
  }
}
