[[- /*
SPDX-FileCopyrightText: 2026 Norca contributors
SPDX-License-Identifier: MPL-2.0
*/ -]]
[[- $v := vars . ]]
[[- $job := $v.job_name ]][[ if eq $job "" ]][[ $job = (meta "pack.name" .) ]][[ end ]]
[[- $ns := $v.namespace ]][[ if eq $ns "" ]][[ $ns = "default" ]][[ end ]]
[[- $api := "http" ]][[ if $v.tls_enabled ]][[ $api = "https" ]][[ end ]]
Consul [[ $v.consul_version ]] (official HashiCorp release, BSL 1.1 - accepted by you, not distributed by Norca):
  [[ $job ]]            [[ $v.server_count ]] server(s), datacenter [[ $v.datacenter ]], driver [[ $v.driver ]], TLS [[ if $v.tls_enabled ]]on[[ else ]]off[[ end ]], ACL default deny
[[- if $v.clients_enabled ]]
  [[ $job ]]-clients    a client agent on every other node (system job)
[[- end ]]
[[- if $v.snapshot_enabled ]]
  [[ $job ]]-snapshot   snapshots to s3://[[ $v.snapshot_s3_bucket ]]/[[ $v.snapshot_s3_prefix ]]/[[ $v.datacenter ]]/ ([[ $v.snapshot_cron ]] UTC)
[[- end ]]

With Norca ACLs enabled, the bootstrap task needs this policy bound to it
(the pack cannot create policies; this is the only manual step). Save it as
[[ $job ]]-bootstrap.hcl and apply it before the first run:
  norca acl policy apply -namespace [[ $ns ]] -job [[ $job ]] -group servers -task bootstrap \
    [[ $job ]]-bootstrap [[ $job ]]-bootstrap.hcl

namespace "[[ $ns ]]" {
  variables {
    path "nomad/jobs/[[ $job ]]/servers/bootstrap" {
      capabilities = ["read", "write"]
    }
    path "nomad/jobs/[[ $job ]]/servers" {
      capabilities = ["write"]
    }
    path "nomad/jobs/[[ $job ]]-clients" {
      capabilities = ["write"]
    }
    path "nomad/jobs/[[ $job ]]-snapshot/snapshot" {
      capabilities = ["write"]
    }
  }
}

Mark the server nodes ([[ $v.server_count ]], one per zone[[ if ne $v.server_node_meta "" ]], meta [[ $v.server_node_meta ]] = "true"[[ end ]])
[[- if ne $v.driver "raw_exec" ]] and give them the host volume "[[ $v.server_host_volume ]]"[[ end ]].

The management token (keep it safe; the CA key is in the same Variable):
  norca var get -namespace [[ $ns ]] -item=management_token nomad/jobs/[[ $job ]]/servers/bootstrap

Check the cluster (any server allocation):
  norca service info -namespace [[ $ns ]] [[ $job ]]
  norca alloc exec -namespace [[ $ns ]] -task server <alloc id> \
    consul operator raft list-peers -token=<management token>
[[- if and $v.snapshot_enabled $v.snapshot_s3_credentials ]]

The snapshot job reads its S3 credentials from a Variable:
  norca var put -namespace [[ $ns ]] nomad/jobs/[[ $job ]]-snapshot aws_access_key_id=<id> aws_secret_access_key=-
[[- end ]]

Connecting the Norca agents to this Consul (optional, not automatic): see the
"Wiring Norca's consul block" section of the pack README.
