[[- /*
SPDX-FileCopyrightText: 2026 Norca contributors
SPDX-License-Identifier: MPL-2.0
*/ -]]
[[- $v := vars . ]]
[[- $job := $v.job_name ]][[ if eq $job "" ]][[ $job = (meta "pack.name" .) ]][[ end ]]
[[- $secrets := $v.secrets_variable ]][[ if eq $secrets "" ]][[ $secrets = printf "nomad/jobs/%s" $job ]][[ end ]]
[[- $ns := $v.namespace ]][[ if eq $ns "" ]][[ $ns = "default" ]][[ end ]]
NATS job "[[ $job ]]": [[ $v.count ]] server(s), driver [[ $v.driver ]], JetStream [[ if $v.jetstream_enabled ]]on ([[ $v.storage ]])[[ else ]]off[[ end ]], TLS [[ if $v.tls_enabled ]]on[[ else ]]off[[ end ]], auth [[ $v.auth_mode ]].

Services (native discovery, namespace [[ $ns ]]):
  [[ $job ]]            clients          norca service info [[ $job ]]
  [[ $job ]]-cluster    routes
[[- if $v.gateway_enabled ]]
  [[ $job ]]-gateway    gateway address the other regions list in remote_gateways
[[- end ]]
  [[ $job ]]-monitor    monitoring       curl http://<addr>/routez /jsz /gatewayz /healthz
[[- if or $v.tls_enabled (ne $v.auth_mode "none") ]]

The servers start only when the Variable [[ $secrets ]] exists, e.g.:
  norca var put -namespace [[ $ns ]] [[ $secrets ]] \
[[- if $v.tls_enabled ]]
    tls_ca=@ca.pem tls_cert=@server.pem tls_key=@server-key.pem \
[[- end ]]
[[- if eq $v.auth_mode "token" ]]
    client_token=<token>
[[- else if eq $v.auth_mode "nkey" ]]
    client_nkeys=<UA...,UB...>
[[- else if eq $v.auth_mode "config" ]]
    auth_config=@auth.conf
[[- end ]]
[[- end ]]
[[- if $v.bridge_enabled ]]
[[- $bridge := $v.bridge_job_name ]]

Bridge job "[[ $bridge ]]": it needs this ACL policy bound to the job (the pack
cannot create policies). Save it as [[ $bridge ]]-policy.hcl and apply it with
  norca acl policy apply -namespace [[ $ns ]] -job [[ $bridge ]] [[ $bridge ]] [[ $bridge ]]-policy.hcl

namespace "*" {
  capabilities = ["read-job"]
}
namespace "[[ $ns ]]" {
  capabilities = ["read-job"]
  variables {
    path "nomad/jobs/[[ $bridge ]]/*" {
      capabilities = ["list", "read", "write"]
    }
  }
}
node {
  policy = "read"
}
[[- if or $v.tls_enabled (ne $v.auth_mode "none") ]]

The bridge's NATS credentials go into its own Variable, e.g.:
  norca var put -namespace [[ $ns ]] nomad/jobs/[[ $bridge ]][[ if $v.tls_enabled ]] nats_ca=@ca.pem[[ end ]][[ if eq $v.auth_mode "token" ]] nats_token=<token>[[ else if eq $v.auth_mode "nkey" ]] nats_nkey_seed=@bridge.nk[[ else if eq $v.auth_mode "config" ]] nats_creds=@bridge.creds[[ end ]]
[[- end ]]
[[- end ]]
