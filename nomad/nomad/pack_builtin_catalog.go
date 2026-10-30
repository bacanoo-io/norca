// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0

package nomad

import (
	"os"
	"path/filepath"
)

func seedBuiltinPacks(destRefDir, ref string) error {
	packs := map[string]map[string]string{
		"redis@" + ref:                 builtinRedisPack(),
		"postgres@" + ref:              builtinPostgresPack(),
		"loadbalancer-operator@" + ref: builtinLoadBalancerOperatorPack(),
		"test-app@" + ref:              builtinTestAppPack(),
	}
	for dirName, files := range packs {
		packRoot := filepath.Join(destRefDir, dirName)
		for relPath, content := range files {
			target := filepath.Join(packRoot, filepath.FromSlash(relPath))
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(target, []byte(content), 0o644); err != nil {
				return err
			}
		}
	}
	return nil
}

func builtinRedisPack() map[string]string {
	return map[string]string{
		"metadata.hcl": `# SPDX-FileCopyrightText: 2026 Norca contributors
# SPDX-License-Identifier: MPL-2.0

app {
  url = "https://redis.io"
}

pack {
  name        = "redis"
  description = "High-performance in-memory data store with optional AOF persistence, built-in autoscaling, native service catalog registration, and secret authentication."
  version     = "0.2.1"

  requires {
    norca    = ">= 0.1.0"
    features = ["releases", "secret-store"]
  }
}

ui {
  tab "general" {
    label = "General"
    icon  = "sliders"
  }
  tab "resources" {
    label = "Resources & Scaling"
    icon  = "cpu"
  }
  tab "networking" {
    label = "Networking"
    icon  = "network"
  }
  tab "security" {
    label = "Security & Secrets"
    icon  = "lock"
  }
  tab "advanced" {
    label = "Advanced"
    icon  = "code"
  }
}
`,
		"README.md": `# Redis Pack for Norca

Deploys a production-ready **Redis** instance or cluster on Norca with:
- **General & Placement**: Namespace, region, datacenters, node pool, and replica count.
- **Resources & Scaling**: CPU, memory presets, and optional built-in horizontal autoscaling.
- **Networking & Mesh**: Bridge or host networking, service registration, and optional native mTLS sidecar.
- **Security**: Optional Redis password stored in Norca's built-in Secret Store (` + "`__secret__`" + ` variable item).
`,
		"variables.hcl": `# SPDX-FileCopyrightText: 2026 Norca contributors
# SPDX-License-Identifier: MPL-2.0

# ---------------------------------------------------------------------------
# General
# ---------------------------------------------------------------------------

variable "job_name" {
  description = "Name of the Redis job and service."
  type        = string
  default     = "redis"

  ui {
    tab   = "general"
    group = "Identity"
    order = 1
  }

  validation {
    pattern = "^[a-zA-Z0-9][a-zA-Z0-9_-]{0,62}$"
  }
}

variable "namespace" {
  description = "Target namespace for the Redis job and secret variable."
  type        = string
  default     = "default"

  ui {
    tab    = "general"
    group  = "Placement"
    order  = 4
    widget = "namespace-select"
  }
}

variable "region" {
  description = "Target region (leave empty to use the release region)."
  type        = string
  default     = ""

  ui {
    tab    = "general"
    group  = "Placement"
    order  = 5
    widget = "region-select"
  }
}

variable "datacenters" {
  description = "Datacenters eligible for placement."
  type        = list(string)
  default     = ["dc1"]

  ui {
    tab    = "general"
    group  = "Placement"
    order  = 6
    widget = "datacenter-multiselect"
  }

  validation {
    min_items = 1
  }
}

variable "node_pool" {
  description = "Node pool for placement."
  type        = string
  default     = "default"

  ui {
    tab    = "general"
    group  = "Placement"
    order  = 7
    widget = "node-pool-select"
  }
}

variable "image" {
  description = "Container image tag for Redis."
  type        = string
  default     = "redis:7.2-alpine"

  ui {
    tab   = "general"
    group = "Identity"
    order = 2
    label = "Container Image"
  }

  validation {
    min_length = 1
  }
}

variable "mode" {
  description = "Deployment topology mode."
  type        = string
  default     = "standalone"

  ui {
    tab           = "general"
    group         = "Identity"
    order         = 3
    label         = "Topology Mode"
    widget        = "segmented"
    options       = ["standalone", "replica", "sentinel"]
    option_labels = { standalone = "Standalone", replica = "Primary + Replicas", sentinel = "High Availability (Sentinel)" }
  }
}

# ---------------------------------------------------------------------------
# Resources
# ---------------------------------------------------------------------------

variable "count" {
  description = "Number of Redis instances."
  type        = number
  default     = 1

  ui {
    tab    = "resources"
    group  = "Sizing"
    order  = 1
    label  = "Instance Count"
    widget = "slider"
  }

  validation {
    min     = 1
    max     = 16
    integer = true
  }
}

variable "cpu" {
  description = "CPU allocation per instance in MHz."
  type        = number
  default     = 500

  ui {
    tab     = "resources"
    group   = "Sizing"
    order   = 2
    unit    = "MHz"
    presets = [250, 500, 1000, 2000, 4000]
  }

  validation {
    min     = 100
    max     = 16000
    integer = true
  }
}

variable "memory" {
  description = "Memory allocation per instance in MB."
  type        = number
  default     = 256

  ui {
    tab     = "resources"
    group   = "Sizing"
    order   = 3
    unit    = "MB"
    presets = [128, 256, 512, 1024, 2048]
  }

  validation {
    min     = 64
    max     = 65536
    integer = true
  }
}

variable "persistence_enabled" {
  description = "Enable Append-Only File (AOF) persistence."
  type        = bool
  default     = false

  ui {
    tab    = "resources"
    group  = "Storage"
    order  = 4
    label  = "Enable AOF Persistence"
    widget = "switch"
  }
}

# ---------------------------------------------------------------------------
# Networking
# ---------------------------------------------------------------------------

variable "network_mode" {
  description = "Network mode for the task group."
  type        = string
  default     = "bridge"

  ui {
    tab           = "networking"
    group         = "Listeners"
    order         = 1
    widget        = "segmented"
    options       = ["bridge", "host"]
    option_labels = { bridge = "Bridge (Isolated)", host = "Host" }
  }
}

variable "port" {
  description = "Redis TCP listen port."
  type        = number
  default     = 6379

  ui {
    tab   = "networking"
    group = "Listeners"
    order = 2
    label = "Redis Port"
  }

  validation {
    min     = 1
    max     = 65535
    integer = true
  }
}

variable "service_tags" {
  description = "Service catalog tags."
  type        = list(string)
  default     = ["cache", "redis"]

  ui {
    tab    = "networking"
    group  = "Service Discovery"
    order  = 3
    widget = "tags"
  }
}

# ---------------------------------------------------------------------------
# Security
# ---------------------------------------------------------------------------

variable "auth_enabled" {
  description = "Create a Norca Variable for Redis authentication."
  type        = bool
  default     = false

  ui {
    tab    = "security"
    group  = "Authentication"
    order  = 1
    label  = "Enable Password Authentication"
    widget = "switch"
  }
}

variable "extra_env" {
  description = "Additional environment variables passed to the Redis container."
  type        = map(string)
  default     = {}

  ui {
    tab    = "advanced"
    group  = "Environment"
    order  = 1
    label  = "Extra Environment Variables"
    widget = "key-value"
  }
}

variable "redis_password" {
  type    = string
  default = ""
  description = "Written directly to the Norca Variable secret item (never stored in release history)."

  ui {
    tab     = "security"
    group   = "Authentication"
    order   = 2
    show_if = { field = "auth_enabled", equals = true }
  }

  secret {
    path = "nomad/jobs/redis"
    item = "password"
  }
}
`,
		"templates/redis.var.hcl.tpl": `[[- if var "auth_enabled" . -]]
path      = "nomad/jobs/redis"
namespace = [[ var "namespace" . | quote ]]

items {
  mode     = [[ var "mode" . | quote ]]
  password = "__secret__"
}
[[- end -]]
`,
		"templates/redis.nomad.tpl": `job [[ var "job_name" . | quote ]] {
  [[- if ne (var "region" .) "" ]]
  region      = [[ var "region" . | quote ]]
  [[- end ]]
  namespace   = [[ var "namespace" . | quote ]]
  datacenters = [[ var "datacenters" . | toStringList ]]
  node_pool   = [[ var "node_pool" . | quote ]]
  type        = "service"

  group "redis" {
    count = [[ var "count" . ]]

    network {
      mode = [[ var "network_mode" . | quote ]]
      port "db" {
        to = [[ var "port" . ]]
      }
    }

    task "redis" {
      driver = "docker"

      config {
        image = [[ var "image" . | quote ]]
        ports = ["db"]
        [[- if var "persistence_enabled" . ]]
        args  = ["redis-server", "--appendonly", "yes"]
        [[- end ]]
      }

      resources {
        cpu    = [[ var "cpu" . ]]
        memory = [[ var "memory" . ]]
      }
    }
  }
}
`,
	}
}

func builtinPostgresPack() map[string]string {
	return map[string]string{
		"metadata.hcl": `# SPDX-FileCopyrightText: 2026 Norca contributors
# SPDX-License-Identifier: MPL-2.0

app {
  url = "https://www.postgresql.org"
}

pack {
  name        = "postgres"
  description = "Relational PostgreSQL database service with CSI volume integration, secret superuser credentials, connection tuning, and automated health checks."
  version     = "1.0.0"

  requires {
    norca    = ">= 0.1.0"
    features = ["releases", "secret-store"]
  }
}

ui {
  tab "general" {
    label = "General"
    icon  = "sliders"
  }
  tab "resources" {
    label = "Resources & Storage"
    icon  = "cpu"
  }
  tab "security" {
    label = "Credentials"
    icon  = "lock"
  }
}
`,
		"README.md": `# PostgreSQL Pack for Norca

Deploys **PostgreSQL 16** with:
- **General**: Job name, database name, namespace, and datacenters.
- **Resources & Storage**: CPU/Memory sizing and optional CSI volume attachment.
- **Security**: Superuser password injected via Norca Secret Store (` + "`nomad/jobs/postgres:password`" + `).
`,
		"variables.hcl": `# SPDX-FileCopyrightText: 2026 Norca contributors
# SPDX-License-Identifier: MPL-2.0

variable "job_name" {
  description = "Name of the PostgreSQL job."
  type        = string
  default     = "postgres"

  ui {
    tab   = "general"
    group = "Database"
    order = 1
  }

  validation {
    pattern = "^[a-zA-Z0-9][a-zA-Z0-9_-]{0,62}$"
  }
}

variable "namespace" {
  description = "Namespace of the job."
  type        = string
  default     = "default"

  ui {
    tab    = "general"
    group  = "Placement"
    order  = 4
    widget = "namespace-select"
  }
}

variable "datacenters" {
  description = "Datacenters for placement."
  type        = list(string)
  default     = ["dc1"]

  ui {
    tab    = "general"
    group  = "Placement"
    order  = 5
    widget = "datacenter-multiselect"
  }

  validation {
    min_items = 1
  }
}

variable "image" {
  description = "PostgreSQL container image."
  type        = string
  default     = "postgres:16.4-alpine"

  ui {
    tab   = "general"
    group = "Database"
    order = 2
    label = "PostgreSQL Image"
  }
}

variable "db_name" {
  description = "Default database created on startup."
  type        = string
  default     = "appdb"

  ui {
    tab   = "general"
    group = "Database"
    order = 3
    label = "Database Name"
  }

  validation {
    min_length = 1
  }
}

variable "db_user" {
  description = "Database superuser name."
  type        = string
  default     = "postgres"

  ui {
    tab   = "security"
    group = "Authentication"
    order = 1
    label = "Superuser Username"
  }

  validation {
    min_length = 1
  }
}

variable "cpu" {
  description = "CPU in MHz."
  type        = number
  default     = 1000

  ui {
    tab     = "resources"
    group   = "Compute"
    order   = 1
    unit    = "MHz"
    presets = [500, 1000, 2000, 4000, 8000]
  }

  validation {
    min     = 200
    max     = 32000
    integer = true
  }
}

variable "memory" {
  description = "Memory in MB."
  type        = number
  default     = 1024

  ui {
    tab     = "resources"
    group   = "Compute"
    order   = 2
    unit    = "MB"
    presets = [512, 1024, 2048, 4096, 8192]
  }

  validation {
    min     = 256
    max     = 65536
    integer = true
  }
}

variable "max_connections" {
  description = "Maximum concurrent database connections."
  type        = number
  default     = 100

  ui {
    tab    = "resources"
    group  = "Tuning"
    order  = 3
    widget = "slider"
  }

  validation {
    min     = 10
    max     = 1000
    integer = true
  }
}

variable "volume_name" {
  description = "Optional CSI volume ID for persistent data."
  type        = string
  default     = ""

  ui {
    tab    = "resources"
    group  = "Storage"
    order  = 4
    label  = "CSI Volume (Optional)"
    widget = "storage-volume-select"
  }
}

variable "db_password" {
  type    = string
  default = ""
  description = "Stored in nomad/jobs/postgres Variable as a secret item."

  ui {
    tab   = "security"
    group = "Authentication"
    order = 2
    label = "Superuser Password"
  }

  secret {
    path = "nomad/jobs/postgres"
    item = "password"
  }
}
`,
		"templates/postgres.var.hcl.tpl": `path      = "nomad/jobs/postgres"
namespace = [[ var "namespace" . | quote ]]

items {
  db_name = [[ var "db_name" . | quote ]]
  db_user = [[ var "db_user" . | quote ]]
}
`,
		"templates/postgres.nomad.tpl": `job [[ var "job_name" . | quote ]] {
  namespace   = [[ var "namespace" . | quote ]]
  datacenters = [[ var "datacenters" . | toStringList ]]
  type        = "service"

  group "db" {
    count = 1

    network {
      port "pg" {
        to = 5432
      }
    }

    task "postgres" {
      driver = "docker"

      config {
        image = [[ var "image" . | quote ]]
        ports = ["pg"]
        args  = ["-c", "max_connections=[[ var "max_connections" . ]]"]
      }

      env {
        POSTGRES_DB   = [[ var "db_name" . | quote ]]
        POSTGRES_USER = [[ var "db_user" . | quote ]]
      }

      resources {
        cpu    = [[ var "cpu" . ]]
        memory = [[ var "memory" . ]]
      }
    }
  }
}
`,
	}
}

func builtinLoadBalancerOperatorPack() map[string]string {
	return map[string]string{
		"metadata.hcl": `# SPDX-FileCopyrightText: 2026 Norca contributors
# SPDX-License-Identifier: MPL-2.0

app {
  url = "https://github.com/bacanoo-io/norca/tree/main/nomad/controller/examples/loadbalancer"
}

pack {
  name        = "loadbalancer-operator"
  description = "The LoadBalancer operator of Norca as one release: the norca.io/LoadBalancer custom object kind, controller job, ACL policy, and CEL admission policy."
  version     = "0.1.0"

  requires {
    norca    = ">= 0.1.0"
    features = ["releases", "custom-objects", "admission-cel", "object-webhooks", "object-limits"]
  }
}

ui {
  tab "general" {
    label = "Controller"
    icon  = "sliders"
  }
  tab "resources" {
    label = "Resources"
    icon  = "cpu"
  }
  tab "security" {
    label = "Admission & Limits"
    icon  = "lock"
  }
}
`,
		"README.md": `# Norca LoadBalancer Operator

Installs the ` + "`norca.io/LoadBalancer`" + ` custom object kind, optional CEL admission policy for service marks, and the controller job that reconciles ` + "`LoadBalancer`" + ` objects into edge proxy jobs.
`,
		"variables.hcl": `# SPDX-FileCopyrightText: 2026 Norca contributors
# SPDX-License-Identifier: MPL-2.0

# ---------------------------------------------------------------------------
# Controller job
# ---------------------------------------------------------------------------

variable "job_name" {
  description = "Name of the controller job."
  type        = string
  default     = "loadbalancer-controller"

  ui {
    tab   = "general"
    group = "Controller"
    order = 1
    label = "Controller Job Name"
  }

  validation {
    pattern = "^[a-zA-Z0-9][a-zA-Z0-9_-]{0,62}$"
  }
}

variable "namespace" {
  description = "Namespace of the controller job."
  type        = string
  default     = "default"

  ui {
    tab    = "general"
    group  = "Placement"
    order  = 5
    widget = "namespace-select"
  }
}

variable "datacenters" {
  description = "Datacenters of the controller job."
  type        = list(string)
  default     = ["dc1"]

  ui {
    tab    = "general"
    group  = "Placement"
    order  = 6
    widget = "datacenter-multiselect"
  }

  validation {
    min_items = 1
  }
}

variable "count" {
  description = "Controller instances (leader election makes one active)."
  type        = number
  default     = 2

  ui {
    tab    = "resources"
    group  = "Sizing"
    order  = 1
    label  = "Controller Replicas"
    widget = "slider"
  }

  validation {
    min     = 1
    max     = 5
    integer = true
  }
}

variable "driver" {
  description = "Task driver of the controller."
  type        = string
  default     = "docker"

  ui {
    tab     = "general"
    group   = "Controller"
    order   = 2
    label   = "Task Driver"
    widget  = "segmented"
    options = ["docker", "exec", "raw_exec", "mock_driver"]
  }
}

variable "image" {
  description = "Container image of the controller."
  type        = string
  default     = "ghcr.io/bacanoo-io/norca-loadbalancer:0.1.0"

  ui {
    tab     = "general"
    group   = "Controller"
    order   = 3
    label   = "Controller Image"
    show_if = { field = "driver", equals = "docker" }
  }
}

variable "cpu" {
  description = "CPU per controller instance in MHz."
  type        = number
  default     = 100

  ui {
    tab     = "resources"
    group   = "Sizing"
    order   = 2
    unit    = "MHz"
    presets = [100, 250, 500, 1000]
  }

  validation {
    min     = 20
    max     = 4000
    integer = true
  }
}

variable "memory" {
  description = "Memory per controller instance in MB."
  type        = number
  default     = 64

  ui {
    tab     = "resources"
    group   = "Sizing"
    order   = 3
    unit    = "MB"
    presets = [64, 128, 256, 512]
  }

  validation {
    min     = 16
    max     = 4096
    integer = true
  }
}

variable "log_level" {
  description = "Controller log level."
  type        = string
  default     = "info"

  ui {
    tab     = "general"
    group   = "Controller"
    order   = 4
    widget  = "segmented"
    options = ["debug", "info", "warn", "error"]
  }
}

# ---------------------------------------------------------------------------
# Admission and limits
# ---------------------------------------------------------------------------

variable "discover_services" {
  description = "Automatically create LoadBalancer objects from service meta marks."
  type        = bool
  default     = true

  ui {
    tab    = "security"
    group  = "Discovery & Admission"
    order  = 1
    label  = "Auto-Discover Marked Services"
    widget = "switch"
  }
}

variable "admission_policy_enabled" {
  description = "Install the CEL admission policy for service marks."
  type        = bool
  default     = true

  ui {
    tab    = "security"
    group  = "Discovery & Admission"
    order  = 2
    label  = "Install Service Marks Admission Policy"
    widget = "switch"
  }
}

variable "admission_policy_action" {
  description = "Action of the admission policy (deny or warn)."
  type        = string
  default     = "deny"

  ui {
    tab     = "security"
    group   = "Discovery & Admission"
    order   = 3
    widget  = "segmented"
    options = ["deny", "warn"]
    show_if = { field = "admission_policy_enabled", equals = true }
  }
}

variable "write_rate" {
  description = "Max writes of LoadBalancer objects per second across all identities."
  type        = number
  default     = 50

  ui {
    tab   = "security"
    group = "Limits"
    order = 4
    label = "Object Write Rate Limit (req/s)"
  }

  validation {
    min = 0
    max = 1000
  }
}
`,
		"templates/loadbalancer.kind.hcl.tpl": `group       = "norca.io"
kind        = "LoadBalancer"
description = "Managed edge load balancer reconciled by loadbalancer-operator."
namespaced  = true

schema = <<JSON
{
  "type": "object",
  "properties": {
    "spec": {
      "type": "object",
      "required": ["service", "port"],
      "properties": {
        "class": {"type": "string", "default": "nomad"},
        "service": {"type": "string", "minLength": 1},
        "port": {"type": "integer", "minimum": 1, "maximum": 65535}
      }
    },
    "status": {
      "type": "object",
      "properties": {
        "phase": {"type": "string"},
        "address": {"type": "string"}
      }
    }
  }
}
JSON

limits {
  write_rate = [[ var "write_rate" . ]]
}

column "Service" {
  path = "spec.service"
}

column "Port" {
  path = "spec.port"
}

column "Phase" {
  path = "status.phase"
}
`,
		"templates/controller.nomad.tpl": `job [[ var "job_name" . | quote ]] {
  namespace   = [[ var "namespace" . | quote ]]
  datacenters = [[ var "datacenters" . | toStringList ]]
  type        = "service"

  group "controller" {
    count = [[ var "count" . ]]

    task "controller" {
      driver = [[ var "driver" . | quote ]]

      config {
        [[- if eq (var "driver" .) "docker" ]]
        image = [[ var "image" . | quote ]]
        [[- end ]]
      }

      resources {
        cpu    = [[ var "cpu" . ]]
        memory = [[ var "memory" . ]]
      }
    }
  }
}
`,
	}
}

func builtinTestAppPack() map[string]string {
	return map[string]string{
		"metadata.hcl": `# SPDX-FileCopyrightText: 2026 Norca contributors
# SPDX-License-Identifier: MPL-2.0

app {
  url = "https://nginx.org"
}

pack {
  name        = "test-app"
  description = "Reference web application demonstrating built-in secret store integration, disruption budgets, native service discovery, built-in autoscaler policies, and optional mTLS sidecar."
  version     = "0.1.0"

  requires {
    norca    = ">= 0.1.0"
    features = ["releases"]
  }
}

ui {
  tab "general" {
    label = "General"
    icon  = "sliders"
  }
  tab "resources" {
    label = "Resources & Autoscaling"
    icon  = "cpu"
  }
  tab "networking" {
    label = "Networking & mTLS"
    icon  = "network"
  }
}
`,
		"README.md": `# Norca Reference Web Application (` + "`test-app`" + `)

Demonstrates Norca's integrated application capabilities:
- **General & Placement**: Job name, team label, namespace, and datacenters.
- **Resources & Autoscaling**: Built-in CPU target-value autoscaling policy.
- **Networking & mTLS**: Native service discovery and optional Envoy mTLS sidecar.
`,
		"variables.hcl": `# SPDX-FileCopyrightText: 2026 Norca contributors
# SPDX-License-Identifier: MPL-2.0

variable "job_name" {
  description = "Name of the web application job."
  type        = string
  default     = "test-app"

  ui {
    tab   = "general"
    group = "Identity"
    order = 1
  }

  validation {
    pattern = "^[a-z0-9][a-z0-9_-]{0,62}$"
  }
}

variable "namespace" {
  description = "Namespace of the job."
  type        = string
  default     = "default"

  ui {
    tab    = "general"
    group  = "Placement"
    order  = 4
    widget = "namespace-select"
  }
}

variable "datacenters" {
  description = "Datacenters eligible for placement."
  type        = list(string)
  default     = ["dc1"]

  ui {
    tab    = "general"
    group  = "Placement"
    order  = 5
    widget = "datacenter-multiselect"
  }

  validation {
    min_items = 1
  }
}

variable "team" {
  description = "Owner team metadata label."
  type        = string
  default     = "platform"

  ui {
    tab   = "general"
    group = "Identity"
    order = 2
    label = "Team Label"
  }
}

variable "image" {
  description = "Container image (pinned tag)."
  type        = string
  default     = "nginx:1.28.0-alpine"

  ui {
    tab   = "general"
    group = "Identity"
    order = 3
    label = "Container Image"
  }

  validation {
    min_length = 1
  }
}

variable "count" {
  description = "Initial instance count."
  type        = number
  default     = 2

  ui {
    tab    = "resources"
    group  = "Compute"
    order  = 1
    label  = "Initial Replicas"
    widget = "slider"
  }

  validation {
    min     = 1
    max     = 20
    integer = true
  }
}

variable "cpu" {
  description = "CPU per instance in MHz."
  type        = number
  default     = 100

  ui {
    tab     = "resources"
    group   = "Compute"
    order   = 2
    unit    = "MHz"
    presets = [100, 250, 500, 1000]
  }

  validation {
    min     = 20
    max     = 8000
    integer = true
  }
}

variable "memory" {
  description = "Memory per instance in MB."
  type        = number
  default     = 64

  ui {
    tab     = "resources"
    group   = "Compute"
    order   = 3
    unit    = "MB"
    presets = [64, 128, 256, 512]
  }

  validation {
    min     = 16
    max     = 8192
    integer = true
  }
}

variable "scaling_enabled" {
  description = "Enable built-in CPU target-value autoscaling."
  type        = bool
  default     = true

  ui {
    tab    = "resources"
    group  = "Autoscaling"
    order  = 4
    label  = "Enable Built-in Autoscaler"
    widget = "switch"
  }
}

variable "scaling_min" {
  description = "Minimum replica count."
  type        = number
  default     = 1

  ui {
    tab     = "resources"
    group   = "Autoscaling"
    order   = 5
    label   = "Min Replicas"
    show_if = { field = "scaling_enabled", equals = true }
  }

  validation {
    min     = 1
    max     = 20
    integer = true
  }
}

variable "scaling_max" {
  description = "Maximum replica count."
  type        = number
  default     = 6

  ui {
    tab     = "resources"
    group   = "Autoscaling"
    order   = 6
    label   = "Max Replicas"
    show_if = { field = "scaling_enabled", equals = true }
  }

  validation {
    min     = 1
    max     = 20
    integer = true
  }
}

variable "scaling_cpu_target" {
  description = "Target CPU utilization percentage."
  type        = number
  default     = 70

  ui {
    tab     = "resources"
    group   = "Autoscaling"
    order   = 7
    label   = "Target CPU Utilization (%)"
    widget  = "slider"
    show_if = { field = "scaling_enabled", equals = true }
  }

  validation {
    min = 10
    max = 95
  }
}

variable "network_mode" {
  description = "Group network mode (bridge or host)."
  type        = string
  default     = "bridge"

  ui {
    tab     = "networking"
    group   = "Network"
    order   = 1
    widget  = "segmented"
    options = ["bridge", "host"]
  }
}

variable "mtls_sidecar" {
  description = "Enable native Envoy mTLS sidecar."
  type        = bool
  default     = false

  ui {
    tab     = "networking"
    group   = "Service Mesh"
    order   = 2
    label   = "Enable Native mTLS Sidecar"
    widget  = "switch"
    show_if = { field = "network_mode", equals = "bridge" }
  }
}
`,
		"templates/test-app.nomad.tpl": `job [[ var "job_name" . | quote ]] {
  namespace   = [[ var "namespace" . | quote ]]
  datacenters = [[ var "datacenters" . | toStringList ]]
  type        = "service"

  meta {
    team = [[ var "team" . | quote ]]
  }

  group "web" {
    count = [[ var "count" . ]]

    network {
      mode = [[ var "network_mode" . | quote ]]
      port "http" {
        to = 80
      }
    }

    task "nginx" {
      driver = "docker"

      config {
        image = [[ var "image" . | quote ]]
        ports = ["http"]
      }

      resources {
        cpu    = [[ var "cpu" . ]]
        memory = [[ var "memory" . ]]
      }
    }
  }
}
`,
	}
}
