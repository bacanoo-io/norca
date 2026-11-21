# The configuration of the NATS server, built as data. nats.norca.hcl writes
# it with natsconf(); nothing here is text that has to be indented or escaped.

locals {
  job       = rig.release
  clustered = var.count > 1
  auth      = length(var.users) > 0
  tls       = var.tls_secret != ""
  secrets   = runtime_env("NORCA_SECRETS_DIR")

  # The accounts in the order of their first user.
  accounts = distinct([for u in var.users : u.account])

  # The account the changes of the Norca region arrive in.
  bus_account = var.norca_bus == null ? "" : (
    var.norca_bus.account != "" ? var.norca_bus.account :
    local.auth ? local.accounts[0] : ""
  )

  tls_block = {
    ca_file   = "${local.secrets}/tls/ca.pem"
    cert_file = "${local.secrets}/tls/cert.pem"
    key_file  = "${local.secrets}/tls/key.pem"
    timeout   = 5
  }

  # Users in accounts. The password is read from the secret store when the
  # task template renders: secret_ref() writes the runtime lookup and quotes
  # the value for the NATS configuration, and fails the template when the
  # item is missing.
  accounts_block = {
    for a in local.accounts : a => {
      jetstream = var.jetstream.enabled ? "enabled" : null
      users = [
        for u in var.users : merge(
          {
            user     = u.name
            password = secret_ref("${var.users_secret_prefix}/${u.name}", "password")
          },
          length(u.publish_allow) + length(u.subscribe_allow) == 0 ? {} : {
            permissions = {
              publish   = length(u.publish_allow) == 0 ? null : { allow = u.publish_allow }
              subscribe = length(u.subscribe_allow) == 0 ? null : { allow = u.subscribe_allow }
            }
          },
        ) if u.account == a
      ]
    }
  }

  # The leaf node connection to the system bus of the Norca servers.
  bus_remote = var.norca_bus == null ? null : {
    urls = [
      for s in var.norca_bus.servers :
      "nats-leaf://${secret_ref(var.norca_bus.secret, "token", { trim = true, quote = false })}@${s}"
    ]
    account = local.bus_account == "" ? null : local.bus_account
    tls     = var.norca_bus.tls ? { ca_file = "${local.secrets}/norca_bus/ca.pem", timeout = 5 } : null
  }

  # null values are left out by the encoder.
  config = {
    server_name = runtime_env("NORCA_ALLOC_NAME")
    listen      = "0.0.0.0:${var.ports.client}"
    http        = "0.0.0.0:${var.ports.monitor}"

    tls      = local.tls ? local.tls_block : null
    accounts = local.auth ? local.accounts_block : null

    jetstream = !var.jetstream.enabled ? null : {
      store_dir        = "${runtime_env("NORCA_ALLOC_DIR")}/data"
      max_memory_store = var.jetstream.max_memory
      max_file_store   = var.jetstream.max_file
      domain           = var.jetstream.domain == "" ? null : var.jetstream.domain
    }

    cluster = !local.clustered ? null : {
      name      = rig.region
      listen    = "0.0.0.0:${var.ports.cluster}"
      advertise = runtime_env("NORCA_ADDR_cluster")
      # The other servers, found in the native catalog when the task
      # template renders.
      routes = service_addresses("${local.job}-cluster", { scheme = "nats-route", exclude_self = true })
      tls    = local.tls ? merge(local.tls_block, { verify = true }) : null
    }

    leafnodes = local.bus_remote == null ? null : {
      remotes = [local.bus_remote]
    }
  }
}

check "bus_needs_an_account_with_users" {
  condition     = var.norca_bus == null || !local.auth || local.bus_account != ""
  error_message = "with users, the changes of the Norca region need an account to arrive in."
}
