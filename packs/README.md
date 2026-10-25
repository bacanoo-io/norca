<!--
SPDX-FileCopyrightText: 2026 Norca contributors
SPDX-License-Identifier: MPL-2.0
-->

# Norca pack registry

This directory is a pack registry for `norca pack` (Nomad Pack format): every
subdirectory of `packs/` is one pack (`metadata.hcl`, `variables.hcl`,
`templates/*.nomad.tpl`, `outputs.tpl`, `README.md`).

| Pack | |
|---|---|
| [consul](consul/README.md) | Installs and manages the official HashiCorp Consul release (BSL 1.1, referenced, never shipped): servers on host volumes over zones, client agents, gossip/TLS/ACL bootstrap from Variables, optional snapshots to S3 |
| [loadbalancer-operator](loadbalancer-operator/README.md) | The LoadBalancer operator as an operator pack: object kind (webhooks, limits), ACL policy bound to the controller job, admission policy, controller job |
| [nats](nats/README.md) | NATS cluster per region (JetStream, supercluster gateways, TLS and auth from Variables) and the event stream → NATS bridge |

## Using it

Add the repository as a registry (any git URL or local path go-getter
understands), then use the packs by name:

```
norca pack registry add norca <git URL of this repository>
norca pack registry add norca <git URL> --ref=<tag or commit>   # pin a version
norca pack registry add norca <git URL> --target=nats           # only one pack
norca pack registry list

norca pack info   nats --registry=norca
norca pack render nats --registry=norca --var-file aws.hcl
norca pack run    nats --registry=norca --var-file aws.hcl
```

From a checkout the packs can also be used by path without a registry:
`norca pack render ./packs/nats`, `norca pack run ./packs/nats`.

## Adding a pack

Create `packs/<name>/` with the files above (see
`nomad/pack/docs/writing-packs.md`). Templates use the `[[ ]]` delimiters and
the `var "name" .` / `vars .` functions to read variables. New files carry the
Norca SPDX header (`# SPDX-...` in HCL, a `[[/* ... */]]` comment in
templates whose output is printed).
