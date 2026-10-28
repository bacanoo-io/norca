# SPDX-FileCopyrightText: 2026 Norca contributors
# SPDX-License-Identifier: MPL-2.0

# Variables of the nats pack (Norca pack registry) on the three-node cluster:
# three NATS servers, one per node and datacenter, with JetStream on the
# ephemeral disk.
#   norca pack run nats --registry=norca --name=nats -f nats.hcl
count             = 3
separate_scope    = "$${node.datacenter}"
separate_required = true
