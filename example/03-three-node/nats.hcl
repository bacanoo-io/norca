# SPDX-FileCopyrightText: 2026 Norca contributors
# SPDX-License-Identifier: MPL-2.0

# Variables of packs/nats on the three-node cluster: three NATS servers, one
# per node and zone, with JetStream on the ephemeral disk.
#   norca pack run ../../packs/nats --name=nats -f nats.hcl
count             = 3
separate_scope    = "$${meta.zone}"
separate_required = true
