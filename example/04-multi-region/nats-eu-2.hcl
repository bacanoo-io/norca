# SPDX-FileCopyrightText: 2026 Norca contributors
# SPDX-License-Identifier: MPL-2.0

# packs/nats in eu-2 (see nats-eu-1.hcl).
#   norca pack run ../../packs/nats --name=nats --region=eu-2 -f nats-eu-2.hcl
region            = "eu-2"
count             = 3
separate_scope    = "$${meta.zone}"
separate_required = true
gateway_enabled   = true
remote_gateways = [
  { name = "eu-1", urls = ["nats://10.0.0.11:7222", "nats://10.0.0.12:7222"] },
]
