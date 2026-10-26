# SPDX-FileCopyrightText: 2026 Norca contributors
# SPDX-License-Identifier: MPL-2.0

# A LoadBalancer object of the LoadBalancer operator (the
# loadbalancer-operator pack of the Norca pack registry): the operator runs a
# TCP proxy job that listens on port 8080 and forwards to the healthy
# instances of test-app.
#   norca object apply lb-test-app.hcl
kind      = "norca.io/LoadBalancer"
name      = "test-app"
namespace = "apps"
labels    = { team = "demo" }

spec {
  service  = "test-app"
  port     = 8080
  replicas = 2
}
