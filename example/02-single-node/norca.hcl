# SPDX-FileCopyrightText: 2026 Norca contributors
# SPDX-License-Identifier: MPL-2.0

# One machine running the server and the client: persistent data, TLS,
# ACLs, gossip encryption, the shares seal, scheduled encrypted backups,
# the built-in autoscaler, the secret store audit file and the built-in DNS.
# Install as /etc/norca.d/norca.hcl and check it with
#   norca config validate /etc/norca.d/
# Replace every CHANGE_ME first.

name       = "norca-1"
region     = "global"
datacenter = "dc1"
data_dir   = "/opt/norca/data"
bind_addr  = "0.0.0.0"

advertise {
  http = "10.0.0.10" # CHANGE_ME: the address of the machine
  rpc  = "10.0.0.10" # CHANGE_ME
  serf = "10.0.0.10" # CHANGE_ME
}

acl {
  enabled = true
}

# TLS on HTTP and RPC with the certificates of "norca tls ca create" and
# "norca tls cert create -server". API clients authenticate with ACL
# tokens, so HTTPS client certificates are not required.
tls {
  http = true
  rpc  = true

  ca_file   = "/etc/norca.d/tls/nomad-agent-ca.pem"
  cert_file = "/etc/norca.d/tls/global-server-nomad.pem"
  key_file  = "/etc/norca.d/tls/global-server-nomad-key.pem"

  verify_server_hostname = true
  verify_https_client    = false
}

server {
  enabled          = true
  bootstrap_expect = 1

  # Gossip encryption key: norca operator gossip keyring generate
  encrypt = "CHANGE_ME"

  # The root keys of the keyring (and the key of the secret store) are
  # wrapped by an unseal key split into Shamir shares. The first
  # "norca operator keyring rewrap -to shares" generates the shares; after
  # every restart the server stays sealed until "threshold" shares are
  # entered with "norca operator unseal".
  keyring {
    seal "shares" {
      shares    = 5
      threshold = 3
    }
  }

  # Built-in autoscaler; scaling policies without an engine use it.
  autoscaler {
    enabled        = true
    default_engine = "builtin"
  }

  # Audit file of the built-in secret store: every read and write of a
  # secret value, never the value itself.
  secrets {
    audit_file             = "/var/log/norca/secrets-audit.log"
    audit_rotate_duration  = "24h"
    audit_rotate_max_files = 30
  }

  # Scheduled backups of the Raft state and the keyring, encrypted with age
  # before they leave the server, into the RustFS store of this example (the
  # rustfs pack of the Norca pack registry, rustfs.hcl). Keep the age identity
  # (private key) offline: it is only needed to browse and restore backups.
  backup {
    enabled  = true
    schedule = "0 * * * *" # hourly, cron syntax, UTC

    destination "rustfs" {
      type             = "s3"
      endpoint         = "http://10.0.0.10:9000" # CHANGE_ME: the RustFS job on this machine
      bucket           = "norca-backup"          # created by the pack (buckets in rustfs.hcl)
      prefix           = "norca/${region}"
      region           = "us-east-1" # s3_region of the rustfs pack
      force_path_style = true

      # AWS shared credentials file ([default] aws_access_key_id,
      # aws_secret_access_key), see backup-credentials.example.
      credentials_file = "/etc/norca.d/backup-credentials"
    }

    encryption {
      # The public key printed by: age-keygen -o backup-identity.txt
      recipients = ["age1CHANGE_ME"]
    }

    retention {
      keep_hourly  = 24
      keep_daily   = 7
      keep_weekly  = 4
      keep_monthly = 3
    }
  }
}

client {
  enabled = true

  # Data directory of the RustFS job (owned by its container user 10001).
  host_volume "rustfs" {
    path      = "/srv/rustfs"
    read_only = false
  }

  # Observability (step 11): the data of OpenObserve, and the host's root
  # directory, read-only, for the host metrics of the OpenTelemetry Collector.
  host_volume "openobserve" {
    path      = "/srv/openobserve"
    read_only = false
  }

  host_volume "hostfs" {
    path      = "/"
    read_only = true
  }

  native_service_discovery {
    default_healthy_only = true
  }

  # Built-in DNS on the host: dig @127.0.0.1 -p 8600 test-app.service.apps.nomad
  dns {
    enabled    = true
    namespaces = ["default", "apps"]
  }
}

# Norca does not need Consul.
consul {
  auto_advertise   = false
  server_auto_join = false
  client_auto_join = false
}

telemetry {
  prometheus_metrics         = true
  publish_allocation_metrics = true
  publish_node_metrics       = true
}
