# Copyright (c) HashiCorp, Inc.
# Modifications Copyright (c) 2026 Norca contributors
# SPDX-License-Identifier: MPL-2.0

# Increase log verbosity
log_level = "DEBUG"

region = "foo"

# Setup data dir
data_dir = "/tmp/client1"

# Enable the client
client {
  enabled = true

  # For demo assume we are talking to server1. For production,
  # this should be like "nomad.service.consul:4647" and a system
  # like Consul used for service discovery.
  servers = ["127.0.0.1:4647"]
}

# Modify our port to avoid a collision with server1
ports {
  http = 5656
}

tls {
  http = true
  rpc  = true

  ca_file   = "certs/norca-agent-ca.pem"
  cert_file = "certs/foo-client-norca.pem"
  key_file  = "certs/foo-client-norca-key.pem"

  verify_server_hostname = true
  verify_https_client    = true
}
