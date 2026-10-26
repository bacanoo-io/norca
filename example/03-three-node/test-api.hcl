# SPDX-FileCopyrightText: 2026 Norca contributors
# SPDX-License-Identifier: MPL-2.0

# A second release of the test-app pack, behind an Envoy sidecar: reachable
# only over native mTLS by the services the intentions allow.
#   norca pack run ../packs/test-app --name=test-api --namespace=apps -f test-api.hcl
job_name     = "test-api"
count        = 2
mtls_sidecar = true

# This instance reads no secrets.
secrets_enabled = false

separate_scope = "$${node.unique.id}"

scaling_enabled = false
