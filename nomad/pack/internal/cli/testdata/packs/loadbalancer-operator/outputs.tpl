[[- /*
SPDX-FileCopyrightText: 2026 Norca contributors
SPDX-License-Identifier: MPL-2.0
*/ -]]
[[- $v := vars . ]]
LoadBalancer operator "[[ $v.job_name ]]" in namespace [[ $v.namespace ]]: [[ $v.count ]] controller instance(s), driver [[ $v.driver ]], proxies with [[ $v.proxy_driver ]].

Create a load balancer:
  cat > web-lb.hcl <<EOF
  kind      = "norca.io/LoadBalancer"
  name      = "web"
  namespace = "[[ $v.namespace ]]"
  spec {
    service = "web"
    port    = 8080
  }
  EOF
  norca object apply web-lb.hcl
  norca object list norca.io/LoadBalancer -namespace [[ $v.namespace ]]
  norca object events norca.io/LoadBalancer web -namespace [[ $v.namespace ]]

or mark a service with the norca_lb_class / norca_lb_port meta keys.
[[- if $v.webhook_enabled ]]

Webhooks: [[ $v.webhook_url ]]/default and /validate. The kind's webhooks are
approved when the pack is installed with a management token; otherwise run
  norca object-kind webhook approve norca.io/LoadBalancer default
  norca object-kind webhook approve norca.io/LoadBalancer validate
The webhook task reads its certificate from the Variable nomad/jobs/[[ $v.job_name ]]
(items webhook_cert, webhook_key).
[[- end ]]
