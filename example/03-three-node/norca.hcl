# SPDX-FileCopyrightText: 2026 Norca contributors
# SPDX-License-Identifier: MPL-2.0

# Shared configuration of the three nodes: every node runs a server and a
# client. Install as /etc/norca.d/norca.hcl on every node, next to its own
# node-<n>.hcl (name, addresses, zone). Replace every CHANGE_ME first.
#   norca config validate /etc/norca.d/

region     = "global"
datacenter = "dc1"
data_dir   = "/opt/norca/data"
bind_addr  = "0.0.0.0"

acl {
  enabled = true
}

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
  bootstrap_expect = 3

  # Gossip encryption key, the same on every server:
  # norca operator gossip keyring generate
  encrypt = "CHANGE_ME"

  server_join {
    retry_join = ["10.0.0.11", "10.0.0.12", "10.0.0.13"] # CHANGE_ME
  }

  # Every server is unsealed on its own with 3 of the 5 shares.
  keyring {
    seal "shares" {
      shares    = 5
      threshold = 3
    }
  }

  autoscaler {
    enabled        = true
    default_engine = "builtin"
  }

  # Native mTLS: the certificate authority of the region (on by default; it
  # is only used by jobs with an mtls block).
  mtls {
    enabled       = true
    leaf_cert_ttl = "24h"
  }

  # Topology-aware discovery: the overflow fraction the servers fill in when
  # a service's discovery block doesn't set overflow_fraction (0..1, default
  # 0.2; 0 turns the overflow rule off). Reloaded on SIGHUP; jobs keep the
  # value they were registered with until they are submitted again.
  native_service_discovery {
    default_overflow_fraction = 0.2
  }

  secrets {
    audit_file             = "/var/log/norca/secrets-audit.log"
    audit_rotate_duration  = "24h"
    audit_rotate_max_files = 30
  }

  # Backups every 6 hours to an S3-compatible store outside the cluster.
  # Only the leader takes them.
  backup {
    enabled  = true
    schedule = "0 */6 * * *"

    destination "offsite" {
      type             = "s3"
      endpoint         = "https://s3.example.internal" # CHANGE_ME, empty for AWS S3
      region           = "eu-central-1"                # CHANGE_ME
      bucket           = "norca-backup"                # CHANGE_ME
      prefix           = "norca/${region}"
      force_path_style = true
      credentials_file = "/etc/norca.d/backup-credentials"
    }

    encryption {
      recipients = ["age1CHANGE_ME"] # age-keygen -y backup-identity.txt
    }
  }
}

client {
  enabled = true

  native_service_discovery {
    default_healthy_only = true
  }

  dns {
    enabled    = true
    namespaces = ["default", "apps"]
  }
}

# raw_exec runs the LoadBalancer operator (the loadbalancer-operator pack) and
# its proxies from a binary installed on the nodes.
plugin "raw_exec" {
  config {
    enabled = true
  }
}

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
