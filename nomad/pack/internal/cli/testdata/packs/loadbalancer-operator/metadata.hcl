# SPDX-FileCopyrightText: 2026 Norca contributors
# SPDX-License-Identifier: MPL-2.0

app {
  url = "https://github.com/bacanoo-io/norca/tree/main/nomad/controller/examples/loadbalancer"
}

pack {
  name        = "loadbalancer-operator"
  description = "The LoadBalancer operator of Norca as one release: the norca.io/LoadBalancer object kind (with its optional webhooks and limits), the ACL policy of the controller, an admission policy for service marks and the controller job."
  version     = "0.1.0"

  requires {
    norca    = ">= 0.1.0"
    features = ["releases", "custom-objects", "admission-cel", "object-webhooks", "object-limits"]
  }
}

variables_schema = file("values.schema.json")
