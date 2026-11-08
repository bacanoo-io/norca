# SPDX-FileCopyrightText: 2026 Norca contributors
# SPDX-License-Identifier: MPL-2.0

app {
  url = "https://github.com/bacanoo-io/norca-platform/tree/main/loadbalancer/operator"
}

pack {
  name        = "loadbalancer-operator"
  description = "The LoadBalancer operator of Norca as one release: the norca.io/LoadBalancer object kind (with its optional webhooks and limits), the norca.io/LoadBalancerClass kind of the providers, the ACL policy of the controller, an admission policy for service marks and the controller job."
  version     = "0.3.0"

  requires {
    norca    = ">= 0.1.0"
    features = ["releases", "custom-objects", "admission-cel", "object-webhooks", "object-limits"]
  }
}
