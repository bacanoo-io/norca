# SPDX-FileCopyrightText: 2026 Norca contributors
# SPDX-License-Identifier: MPL-2.0

# ---------------------------------------------------------------------------
# Controller job
# ---------------------------------------------------------------------------

variable "job_name" {
  description = "Name of the controller job. The ACL policy of the operator is bound to it."
  type        = string
  default     = "loadbalancer-controller"
}

variable "namespace" {
  description = "Namespace of the controller job and of the LoadBalancer objects it reconciles."
  type        = string
  default     = "default"
}

variable "region" {
  description = "Region of the controller job. Empty leaves it to the region of the release; the operator runs once per region."
  type        = string
  default     = ""
}

variable "datacenters" {
  description = "Datacenters of the controller job."
  type        = list(string)
  default     = ["*"]
}

variable "count" {
  description = "Instances of the controller. Leader election makes one of them reconcile while the others keep a warm cache."
  type        = number
  default     = 2
}

variable "driver" {
  description = "Task driver of the controller: raw_exec or exec (the binary installed on the nodes), docker (an image with the binary), or mock_driver (tests on dev agents only)."
  type        = string
  default     = "raw_exec"
}

variable "binary" {
  description = "Path of the norca-loadbalancer binary on the nodes (raw_exec, exec). Build it in a Norca checkout with: cd nomad/controller && go build -o norca-loadbalancer ./examples/loadbalancer"
  type        = string
  default     = "/usr/local/bin/norca-loadbalancer"
}

variable "image" {
  description = "Container image with the norca-loadbalancer binary as its entrypoint (docker)."
  type        = string
  default     = ""
}

variable "cpu" {
  description = "CPU of a controller instance, in MHz."
  type        = number
  default     = 100
}

variable "memory" {
  description = "Memory of a controller instance, in MB."
  type        = number
  default     = 64
}

variable "log_level" {
  description = "Log level of the controller."
  type        = string
  default     = "info"
}

# ---------------------------------------------------------------------------
# Load balancers
# ---------------------------------------------------------------------------

variable "proxy_driver" {
  description = "Task driver of the proxy jobs of the \"nomad\" class: raw_exec, exec, docker or mock_driver (tests)."
  type        = string
  default     = "raw_exec"
}

variable "proxy_image" {
  description = "Container image of the proxy jobs (proxy_driver docker)."
  type        = string
  default     = ""
}

variable "discover_services" {
  description = "Create LoadBalancer objects from services marked with the norca_lb_* meta keys."
  type        = bool
  default     = true
}

# ---------------------------------------------------------------------------
# Webhooks of the kind
# ---------------------------------------------------------------------------

variable "webhook_enabled" {
  description = "Run the defaulting and validating webhooks of the kind (a webhook task in the controller job) and register them in the kind. They need https: a certificate and key in the job's Variable (items webhook_cert, webhook_key) and the CA in webhook_ca_cert."
  type        = bool
  default     = false
}

variable "webhook_url" {
  description = "Base https URL under which the servers reach the webhook task, e.g. https://lb-webhook.internal:8443 (the paths /default and /validate are appended)."
  type        = string
  default     = ""
}

variable "webhook_ca_cert" {
  description = "PEM bundle of the CA that signs the webhook's certificate. Empty uses the system roots of the servers."
  type        = string
  default     = ""
}

variable "webhook_port" {
  description = "Static port of the webhook task."
  type        = number
  default     = 8443
}

variable "webhook_failure_policy" {
  description = "What happens to writes when the webhook can't be called: fail (reject them) or ignore (let them through with a warning)."
  type        = string
  default     = "fail"
}

variable "default_class" {
  description = "Class the defaulting webhook fills in when a spec names none (without webhooks the schema's default \"nomad\" applies)."
  type        = string
  default     = "nomad"
}

variable "classes" {
  description = "Load balancer classes the providers of the region implement; the validating webhook rejects other classes."
  type        = list(string)
  default     = ["nomad"]
}

# ---------------------------------------------------------------------------
# Admission and limits
# ---------------------------------------------------------------------------

variable "acl_policy_enabled" {
  description = "Install the ACL policy of the controller, bound to its job. Disable it on agents without ACLs (dev agents), where policies can't be written."
  type        = bool
  default     = true
}

variable "admission_policy_enabled" {
  description = "Install the admission policy that checks the norca_lb_port mark of services in job submissions."
  type        = bool
  default     = true
}

variable "admission_policy_action" {
  description = "Action of the admission policy: deny or warn."
  type        = string
  default     = "deny"
}

variable "write_rate" {
  description = "Writes of LoadBalancer objects per second over all identities (0: unlimited)."
  type        = number
  default     = 50
}

variable "identity_write_rate" {
  description = "Writes of LoadBalancer objects per second of one identity, such as the controller (0: unlimited)."
  type        = number
  default     = 20
}

variable "max_objects_per_namespace" {
  description = "LoadBalancer objects per namespace (0: unlimited)."
  type        = number
  default     = 0
}

variable "max_watches_per_identity" {
  description = "Concurrent blocking queries of one identity on LoadBalancer objects, per server (0: unlimited)."
  type        = number
  default     = 16
}
