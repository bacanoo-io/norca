# SPDX-FileCopyrightText: 2026 Norca contributors
# SPDX-License-Identifier: MPL-2.0

# ---------------------------------------------------------------------------
# Placement
# ---------------------------------------------------------------------------

variable "job_name" {
  description = "Name of the NATS job. Empty uses the pack name (\"nats\"). It also prefixes the service names (<job>, <job>-cluster, <job>-gateway, <job>-monitor, <job>-leafnode) and sets the default Variable path nomad/jobs/<job>."
  type        = string
  default     = ""
}

variable "region" {
  description = "Region of the jobs. Empty leaves it to the region the pack is submitted to. Deploy the pack once per region (one region per cloud)."
  type        = string
  default     = ""
}

variable "namespace" {
  description = "Namespace of the jobs. Empty uses the namespace of the submitter (default)."
  type        = string
  default     = ""
}

variable "datacenters" {
  description = "Datacenters eligible for placement."
  type        = list(string)
  default     = ["*"]
}

variable "node_pool" {
  description = "Node pool of the jobs. Empty uses the default node pool."
  type        = string
  default     = ""
}

variable "count" {
  description = "Number of NATS servers in the region. Use an odd number of at least 3: JetStream needs a majority of the servers for its Raft groups."
  type        = number
  default     = 3
}

variable "distinct_hosts" {
  description = "Place every NATS server on a different node (distinct_hosts constraint). Disable it only for tests on a single node."
  type        = bool
  default     = true
}

variable "separate_scope" {
  description = "Optional topology domain for the fork's separate block, e.g. \"$${attr.platform.aws.placement.availability-zone}\" or \"$${meta.zone}\": the servers avoid sharing a domain. Empty renders no separate block."
  type        = string
  default     = ""
}

variable "separate_required" {
  description = "Make the separate rule mandatory instead of a preference. With fewer domains than servers the placement then blocks."
  type        = bool
  default     = false
}

variable "host_network" {
  description = "Client host_network to bind the ports to, e.g. the WireGuard interface of the VPN. Empty uses the default network."
  type        = string
  default     = ""
}

# ---------------------------------------------------------------------------
# Runtime: Docker or a nats-server binary
# ---------------------------------------------------------------------------

variable "driver" {
  description = "Task driver: \"docker\" (official nats image), \"raw_exec\" or \"exec\" (a nats-server binary on the node or downloaded with artifact_url)."
  type        = string
  default     = "docker"
}

variable "image" {
  description = "Docker image of the server (driver = docker). The official image is Apache-2.0 licensed."
  type        = string
  default     = "nats:2.15.0"
}

variable "binary" {
  description = "Path of nats-server for raw_exec/exec: absolute on the node, or relative to the task directory when downloaded with artifact_url (e.g. local/nats-server-v2.15.0-linux-amd64/nats-server)."
  type        = string
  default     = "/usr/local/bin/nats-server"
}

variable "artifact_url" {
  description = "Optional URL of a nats-server release archive for raw_exec/exec, unpacked into the task directory, e.g. https://github.com/nats-io/nats-server/releases/download/v2.15.0/nats-server-v2.15.0-linux-amd64.tar.gz. Empty uses binary as is."
  type        = string
  default     = ""
}

variable "artifact_checksum" {
  description = "Checksum of the artifact, e.g. \"sha256:<hex>\" (recommended). Empty skips the check."
  type        = string
  default     = ""
}

variable "resources" {
  description = "CPU (MHz) and memory (MB) of a NATS server."
  type = object({
    cpu    = number
    memory = number
  })
  default = {
    cpu    = 500
    memory = 512
  }
}

# ---------------------------------------------------------------------------
# Ports (0 = dynamic port)
# ---------------------------------------------------------------------------

variable "client_port" {
  description = "Client port. Static by default so clients and leaf nodes have a fixed address; 0 picks a dynamic port."
  type        = number
  default     = 4222
}

variable "cluster_port" {
  description = "Cluster (route) port between the servers of the region; 0 picks a dynamic port (routes are discovered through the <job>-cluster service)."
  type        = number
  default     = 6222
}

variable "gateway_port" {
  description = "Gateway port for the supercluster. Keep it static: the other regions reach it over the VPN at a fixed address."
  type        = number
  default     = 7222
}

variable "monitor_port" {
  description = "HTTP monitoring port (/varz, /routez, /gatewayz, /jsz, /healthz); also used by the health checks."
  type        = number
  default     = 8222
}

variable "leafnode_port" {
  description = "Leaf node port (only with leafnodes_enabled)."
  type        = number
  default     = 7422
}

# ---------------------------------------------------------------------------
# Cluster, gateways, leaf nodes
# ---------------------------------------------------------------------------

variable "cluster_name" {
  description = "Name of the cluster of this region, which is also its gateway name in the supercluster. Empty uses the Norca region of the job (NOMAD_REGION). Must be unique per region."
  type        = string
  default     = ""
}

variable "gateway_enabled" {
  description = "Enable the gateway listener to join a supercluster with the other regions."
  type        = bool
  default     = false
}

variable "remote_gateways" {
  description = "The gateways of the other regions: name (their cluster_name) and URLs reachable over the VPN, e.g. [{name = \"gcp\", urls = [\"nats://10.20.0.11:7222\", \"nats://10.20.0.12:7222\"]}]. One URL per remote cluster is enough, the rest is discovered."
  type = list(object({
    name = string
    urls = list(string)
  }))
  default = []
}

variable "leafnodes_enabled" {
  description = "Enable the leaf node listener, for edge servers that connect to this region as leaf nodes."
  type        = bool
  default     = false
}

# ---------------------------------------------------------------------------
# JetStream
# ---------------------------------------------------------------------------

variable "jetstream_enabled" {
  description = "Enable JetStream (durable streams)."
  type        = bool
  default     = true
}

variable "jetstream_domain" {
  description = "Optional JetStream domain. All clusters of a supercluster share one domain; leave it empty unless leaf nodes run their own domains."
  type        = string
  default     = ""
}

variable "jetstream_max_memory" {
  description = "max_memory_store of a server, e.g. \"256MB\"."
  type        = string
  default     = "256MB"
}

variable "jetstream_max_file" {
  description = "max_file_store of a server, e.g. \"1GB\". Keep it below the size of the storage (ephemeral_disk_size)."
  type        = string
  default     = "1GB"
}

variable "storage" {
  description = "Where JetStream keeps its data: \"ephemeral_disk\" (sticky, migrated ephemeral disk), \"host_volume\" (the client host volume host_volume; docker and exec) or \"path\" (store_path on the node, for raw_exec)."
  type        = string
  default     = "ephemeral_disk"
}

variable "ephemeral_disk_size" {
  description = "Size of the ephemeral disk in MB (storage = ephemeral_disk)."
  type        = number
  default     = 2048
}

variable "host_volume" {
  description = "Name of the client host volume (storage = host_volume). Every server keeps its data in a subdirectory named after the server."
  type        = string
  default     = "nats"
}

variable "store_path" {
  description = "Directory on the node for the data (storage = path). Every server keeps its data in a subdirectory named after the server."
  type        = string
  default     = ""
}

# ---------------------------------------------------------------------------
# Secrets: TLS and client authentication from a Variable
# ---------------------------------------------------------------------------

variable "secrets_variable" {
  description = "Path of the Variable holding the TLS certificates and the client credentials. Empty uses nomad/jobs/<job>, which the job reads with its workload identity without any policy."
  type        = string
  default     = ""
}

variable "tls_enabled" {
  description = "TLS on the client, route, gateway and leaf node ports, from the items tls_ca, tls_cert and tls_key of the secrets Variable. Routes and gateways verify each other's certificates (mutual TLS). Disable only for development or when the VPN is the only transport protection."
  type        = bool
  default     = false
}

variable "tls_verify_clients" {
  description = "Require client certificates on the client and leaf node ports as well."
  type        = bool
  default     = false
}

variable "auth_mode" {
  description = "Client authentication: \"none\", \"token\" (item client_token), \"nkey\" (item client_nkeys: public user NKeys, comma or newline separated) or \"config\" (item auth_config: a raw NATS config snippet with authorization/accounts or operator/resolver settings for JWT credentials)."
  type        = string
  default     = "none"
}

# ---------------------------------------------------------------------------
# Operations
# ---------------------------------------------------------------------------

variable "disruption_max_unavailable" {
  description = "max_unavailable of the disruption block (fork feature), limiting node drains and evictions so the JetStream quorum survives. Empty renders no disruption block."
  type        = string
  default     = "1"
}

variable "startup_timeout" {
  description = "startup_timeout of the startup check: the server is restarted if it is not up with JetStream enabled within this time."
  type        = string
  default     = "5m"
}

variable "healthy_deadline" {
  description = "healthy_deadline of the update and migrate blocks. A restarted server is healthy (readiness /healthz) only after its streams caught up; raise it for large streams."
  type        = string
  default     = "10m"
}

variable "lame_duck_duration" {
  description = "Lame duck duration: on stop the server stops accepting clients and moves them away over this time before shutting down (kill_signal SIGUSR2). nats-server requires at least 30s."
  type        = string
  default     = "30s"
}

variable "kill_timeout" {
  description = "kill_timeout of the server task; keep it above lame_duck_duration plus a few seconds for the JetStream shutdown."
  type        = string
  default     = "45s"
}

variable "debug" {
  description = "Enable debug logging of the server."
  type        = bool
  default     = false
}

variable "extra_config" {
  description = "Raw NATS configuration appended to the generated file (e.g. websocket or mqtt blocks). It is part of a Nomad template: escape {{ as {{\"{{\"}}."
  type        = string
  default     = ""
}

# ---------------------------------------------------------------------------
# Event stream -> NATS bridge (optional)
# ---------------------------------------------------------------------------

variable "bridge_enabled" {
  description = "Also deploy the event stream to NATS bridge (norca-natsbridge) of the region. It needs an ACL policy bound to its job, see the README and the output of the pack."
  type        = bool
  default     = false
}

variable "bridge_job_name" {
  description = "Name of the bridge job. Its credentials Variable is nomad/jobs/<bridge_job_name>; its checkpoint and lease are stored below it."
  type        = string
  default     = "natsbridge"
}

variable "bridge_count" {
  description = "Instances of the bridge; one publishes, the others stand by (leader election over a Variable lease)."
  type        = number
  default     = 2
}

variable "bridge_driver" {
  description = "Task driver of the bridge: \"raw_exec\", \"exec\" or \"docker\" (with bridge_image)."
  type        = string
  default     = "raw_exec"
}

variable "bridge_binary" {
  description = "Path of norca-natsbridge for raw_exec/exec: absolute on the node, or relative to the task directory with bridge_artifact_url."
  type        = string
  default     = "/usr/local/bin/norca-natsbridge"
}

variable "bridge_artifact_url" {
  description = "Optional URL of the norca-natsbridge binary or archive for raw_exec/exec."
  type        = string
  default     = ""
}

variable "bridge_image" {
  description = "Docker image containing norca-natsbridge as entrypoint (bridge_driver = docker). No official image is published."
  type        = string
  default     = ""
}

variable "bridge_subject_prefix" {
  description = "Subject prefix; events go to <prefix>.<region>.<topic>."
  type        = string
  default     = "nomad"
}

variable "bridge_topics" {
  description = "Event topics to forward (Topic or Topic:key, comma separated). The defaults are those the bridge policy allows."
  type        = string
  default     = "Job,Deployment,Evaluation,Allocation,Node,Service,Scaling"
}

variable "bridge_jetstream_stream" {
  description = "JetStream stream the bridge publishes into (and creates for <prefix>.<region>.> if missing). Stream names are global in a supercluster, so keep the region in it. Empty publishes with core NATS."
  type        = string
  default     = "NORCA_EVENTS_$${NOMAD_REGION}"
}

variable "bridge_resources" {
  description = "CPU (MHz) and memory (MB) of a bridge instance."
  type = object({
    cpu    = number
    memory = number
  })
  default = {
    cpu    = 100
    memory = 64
  }
}
