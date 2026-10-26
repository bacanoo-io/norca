# SPDX-FileCopyrightText: 2026 Norca contributors
# SPDX-License-Identifier: MPL-2.0

# packs/nats in eu-1, part of a supercluster with eu-2. The gateway
# addresses of the other region are reached over the VPN between the
# regions (CHANGE_ME: the node addresses of eu-2).
#   norca pack run ../../packs/nats --name=nats --region=eu-1 -f nats-eu-1.hcl
region            = "eu-1"
count             = 3
separate_scope    = "$${meta.zone}"
separate_required = true
gateway_enabled   = true
remote_gateways = [
  { name = "eu-2", urls = ["nats://10.0.1.11:7222", "nats://10.0.1.12:7222"] },
]
