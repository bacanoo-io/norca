# SPDX-FileCopyrightText: 2026 Norca contributors
# SPDX-License-Identifier: MPL-2.0

# ---------------------------------------------------------------------------
# License: Consul is not part of Norca
# ---------------------------------------------------------------------------

variable "consul_license_accepted" {
  description = "Consul is licensed by HashiCorp under the Business Source License 1.1 (https://github.com/hashicorp/consul/blob/main/LICENSE). Norca does not distribute it: the jobs pull the official image or download chosen below. Set to true to confirm that you have read and accept the Consul license for your use; the pack does not render otherwise."
  type        = bool
  default     = false
}

# ---------------------------------------------------------------------------
# Placement
# ---------------------------------------------------------------------------

variable "job_name" {
  description = "Name of the server job. Empty uses the pack name (\"consul\"). The client job is <job>-clients, the snapshot job <job>-snapshot; it also prefixes the service names and the Variable paths nomad/jobs/<job>/..."
  type        = string
  default     = ""
}

variable "region" {
  description = "Region of the jobs. Empty leaves it to the region the pack is submitted to. One Consul datacenter per Norca region."
  type        = string
  default     = ""
}

variable "namespace" {
  description = "Namespace of the jobs. Empty uses the namespace of the submitter (default)."
  type        = string
  default     = ""
}

variable "datacenters" {
  description = "Norca datacenters eligible for placement."
  type        = list(string)
  default     = ["*"]
}

variable "node_pool" {
  description = "Node pool of the jobs. Empty uses the default node pool. The client job runs on every node of the pool except the server nodes."
  type        = string
  default     = ""
}

variable "server_count" {
  description = "Number of Consul servers (bootstrap_expect): 3 or 5 in production (1 only for development). Each needs its own node with the server host volume."
  type        = number
  default     = 3
}

variable "server_node_meta" {
  description = "Node meta key marking the nodes reserved for Consul servers: servers are placed only where $${meta.<key>} is \"true\", clients everywhere else (a server already is the local agent of its node). Empty removes both constraints (then keep servers and clients apart with node pools)."
  type        = string
  default     = "consul_server"
}

variable "zone_attribute" {
  description = "Topology attribute the servers are spread over (spread block), by default the node meta zone filled in by the installer. Empty renders no spread."
  type        = string
  default     = "$${meta.zone}"
}

variable "zone_separate_required" {
  description = "Also make one server per zone mandatory with the fork's separate block on zone_attribute. With fewer zones than servers the placement then blocks."
  type        = bool
  default     = false
}

variable "host_network" {
  description = "Client host_network of the private network (e.g. the WireGuard interface of the VPN). Gossip, RPC and the TLS API ports bind to its address; the plain HTTP/gRPC ports bind to localhost only when TLS is on. Empty uses the default network of the node."
  type        = string
  default     = ""
}

# ---------------------------------------------------------------------------
# Consul release: referenced by name, never shipped by Norca
# ---------------------------------------------------------------------------

variable "consul_version" {
  description = "Consul release to run (image tag and download version). Provisional default: the current release when the pack was written; pin the release you validated."
  type        = string
  default     = "2.1.0"
}

variable "driver" {
  description = "Task driver of the Consul agents: \"docker\" (official image), \"exec\" or \"raw_exec\" (official release archive downloaded with an artifact block, or consul_binary)."
  type        = string
  default     = "docker"
}

variable "image_registry" {
  description = "Registry of the image. The default is the public registry of the official image; air-gapped installs mirror the image into the internal OCI registry and set its host here (e.g. registry.internal:5000)."
  type        = string
  default     = "docker.io"
}

variable "image_repository" {
  description = "Repository of the official image in image_registry."
  type        = string
  default     = "hashicorp/consul"
}

variable "image_tag" {
  description = "Image tag. Empty uses consul_version."
  type        = string
  default     = ""
}

variable "image_digest" {
  description = "Optional image digest (sha256:<hex>) appended to the reference: the node pulls exactly the image you verified, also from a mirror. Recommended in production."
  type        = string
  default     = ""
}

variable "download_base_url" {
  description = "Base URL of the release archives for exec/raw_exec: <base>/<version>/consul_<version>_linux_<arch>.zip. The default is the official release site; set an internal mirror for air-gapped installs."
  type        = string
  default     = "https://releases.hashicorp.com/consul"
}

variable "download_arch" {
  description = "Architecture of the release archive (amd64, arm64)."
  type        = string
  default     = "amd64"
}

variable "download_checksum" {
  description = "Checksum of the release archive, e.g. \"sha256:<hex>\" from the official SHA256SUMS file. Empty skips the check (not recommended)."
  type        = string
  default     = ""
}

variable "consul_binary" {
  description = "exec/raw_exec: path of a consul binary already installed on the nodes. Empty downloads the release archive (download_base_url) into the task directory."
  type        = string
  default     = ""
}

# ---------------------------------------------------------------------------
# Consul datacenter
# ---------------------------------------------------------------------------

variable "datacenter" {
  description = "Consul datacenter name. It is part of the server certificate name (server.<datacenter>.<domain>), so changing it later needs new certificates."
  type        = string
  default     = "dc1"
}

variable "domain" {
  description = "Consul DNS domain."
  type        = string
  default     = "consul"
}

variable "ui_enabled" {
  description = "Serve the Consul web UI on the servers' API port."
  type        = bool
  default     = true
}

variable "log_level" {
  description = "Consul log level."
  type        = string
  default     = "INFO"
}

variable "tls_enabled" {
  description = "TLS for RPC (verified server names, auto_encrypt certificates for the clients), HTTPS and gRPC-TLS on the private network, from the CA and server certificate the bootstrap task generates. Without TLS the plain HTTP/gRPC ports also listen on the private network (the minimum: never on a public one). Disable only for development."
  type        = bool
  default     = true
}

variable "dns_address" {
  description = "Address(es) the DNS interface listens on (space separated). Localhost by default; the native Norca DNS is the recommended resolver."
  type        = string
  default     = "127.0.0.1"
}

variable "server_extra_config" {
  description = "Raw Consul HCL appended to the server configuration (e.g. connect, config_entries bootstrap). It is part of a Nomad template: escape {{ as {{\"{{\"}}."
  type        = string
  default     = ""
}

variable "client_extra_config" {
  description = "Raw Consul HCL appended to the client configuration. It is part of a Nomad template: escape {{ as {{\"{{\"}}."
  type        = string
  default     = ""
}

# ---------------------------------------------------------------------------
# Ports (static: every agent of the datacenter uses the same ones)
# ---------------------------------------------------------------------------

variable "port_server" {
  description = "Server RPC port."
  type        = number
  default     = 8300
}

variable "port_serf_lan" {
  description = "LAN gossip port (TCP and UDP)."
  type        = number
  default     = 8301
}

variable "port_serf_wan" {
  description = "WAN gossip port of the servers (TCP and UDP)."
  type        = number
  default     = 8302
}

variable "port_http" {
  description = "Plain HTTP API port (localhost only when TLS is on)."
  type        = number
  default     = 8500
}

variable "port_https" {
  description = "HTTPS API port (TLS only)."
  type        = number
  default     = 8501
}

variable "port_grpc" {
  description = "Plain gRPC port (xDS for local Envoy sidecars; localhost only when TLS is on)."
  type        = number
  default     = 8502
}

variable "port_grpc_tls" {
  description = "gRPC TLS port (TLS only)."
  type        = number
  default     = 8503
}

variable "port_dns" {
  description = "DNS port."
  type        = number
  default     = 8600
}

# ---------------------------------------------------------------------------
# Servers
# ---------------------------------------------------------------------------

variable "server_host_volume" {
  description = "Client host volume holding the server data (Raft log and snapshots); docker and exec. Configure it on the server nodes: client { host_volume \"consul-server\" { path = \"/srv/consul\" } }."
  type        = string
  default     = "consul-server"
}

variable "server_data_path" {
  description = "raw_exec only (host volumes cannot be mounted): directory on the node for the server data."
  type        = string
  default     = ""
}

variable "server_resources" {
  description = "CPU (MHz) and memory (MB) of a server."
  type = object({
    cpu    = number
    memory = number
  })
  default = {
    cpu    = 500
    memory = 512
  }
}

variable "healthy_deadline" {
  description = "healthy_deadline of the rolling update and migration of the servers."
  type        = string
  default     = "10m"
}

variable "min_healthy_time" {
  description = "min_healthy_time of a server before the next one is replaced: longer than the autopilot server stabilization time, so the restarted server is a voter again."
  type        = string
  default     = "30s"
}

# ---------------------------------------------------------------------------
# Bootstrap: secrets generated once and kept in Variables
# ---------------------------------------------------------------------------

variable "bootstrap_driver" {
  description = "Driver of the bootstrap task that generates the gossip key, the CA, the server certificate and the ACL tokens with the norca CLI and stores them in Variables through the task API: \"exec\" or \"raw_exec\"."
  type        = string
  default     = "exec"
}

variable "norca_binary" {
  description = "Path of the norca binary on the nodes (used by the bootstrap task)."
  type        = string
  default     = "/usr/local/bin/norca"
}

variable "ca_days" {
  description = "Validity of the generated Consul CA in days."
  type        = number
  default     = 1825
}

variable "cert_days" {
  description = "Validity of the generated server certificate in days. Renewal is a documented manual step (see the README)."
  type        = number
  default     = 365
}

# ---------------------------------------------------------------------------
# Clients
# ---------------------------------------------------------------------------

variable "clients_enabled" {
  description = "Deploy the client job: a Consul client agent on every eligible node (system job)."
  type        = bool
  default     = true
}

variable "client_resources" {
  description = "CPU (MHz) and memory (MB) of a client agent."
  type = object({
    cpu    = number
    memory = number
  })
  default = {
    cpu    = 200
    memory = 256
  }
}

# ---------------------------------------------------------------------------
# Snapshots to S3 (optional)
# ---------------------------------------------------------------------------

variable "snapshot_enabled" {
  description = "Deploy the periodic snapshot job: consul snapshot save on a server node, uploaded to an S3-compatible store (the one of the Norca backups, e.g. MinIO)."
  type        = bool
  default     = false
}

variable "snapshot_cron" {
  description = "Schedule of the snapshot job (cron)."
  type        = string
  default     = "0 * * * *"
}

variable "snapshot_s3_bucket" {
  description = "Bucket of the snapshots."
  type        = string
  default     = ""
}

variable "snapshot_s3_prefix" {
  description = "Key prefix of the snapshots; the object name is <prefix>/<datacenter>/<UTC timestamp>.snap."
  type        = string
  default     = "consul"
}

variable "snapshot_s3_endpoint" {
  description = "Endpoint of an S3-compatible store (e.g. https://minio.internal:9000). Empty uses AWS S3."
  type        = string
  default     = ""
}

variable "snapshot_s3_region" {
  description = "Region of the bucket."
  type        = string
  default     = "us-east-1"
}

variable "snapshot_s3_path_style" {
  description = "Use path-style addressing (MinIO and most S3-compatible stores)."
  type        = bool
  default     = true
}

variable "snapshot_s3_sse" {
  description = "Server-side encryption of the objects (\"AES256\" or \"aws:kms\"); empty sends none. Snapshots contain ACL tokens: encrypt them at rest."
  type        = string
  default     = ""
}

variable "snapshot_s3_credentials" {
  description = "Read the S3 credentials (items aws_access_key_id, aws_secret_access_key) from the Variable nomad/jobs/<job>-snapshot. Disable to use the node's environment (instance profile)."
  type        = bool
  default     = true
}

variable "snapshot_upload_image" {
  description = "Image with the AWS CLI used to upload the snapshot (Apache-2.0). Mirror it like the Consul image for air-gapped installs."
  type        = string
  default     = "docker.io/amazon/aws-cli:2.27.0"
}
