<!--
SPDX-FileCopyrightText: 2026 Norca contributors
SPDX-License-Identifier: MPL-2.0
-->

# loadbalancer-operator

The LoadBalancer operator of Norca (the sample operator of the controller SDK,
`nomad/controller/examples/loadbalancer`) packaged as an **operator pack**: one
release installs, upgrades, rolls back and uninstalls everything the operator
needs, in the fixed order of the extended pack format.

| Template | Resource |
| --- | --- |
| `controller.acl.hcl.tpl` | ACL policy bound to the controller job (`job_acl`): jobs, the proxy jobs' Variables, the leases, and the `norca.io/LoadBalancer` objects (status, events, finalizers) |
| `loadbalancer.kind.hcl.tpl` | Object kind `norca.io/LoadBalancer`: JSON Schema with defaults, a CEL transition rule, display columns, optional webhooks, limits |
| `service-marks.admission.hcl.tpl` | Admission policy: services marked with `norca_lb_port` must name a valid port (optional) |
| `controller.nomad.tpl` | The controller job (two instances with leader election) and the optional webhook task |

The variables are checked against `values.schema.json` before rendering and
again by the servers.

## Install

Build the binary and install it on the nodes (or build an image with it):

```shell
cd nomad/controller && go build -o /usr/local/bin/norca-loadbalancer ./examples/loadbalancer
norca pack plan ./packs/loadbalancer-operator
norca pack run  ./packs/loadbalancer-operator --name=lb
norca pack status lb
```

On an agent without ACLs (a dev agent), policies can't be written: add
`--var=acl_policy_enabled=false`.

With a management token, the release can register the kind and bind the ACL
policy at once. A token with less rights needs `operator` write (the kind),
`acl` write (the policy) and `admission` write (the admission policy):
applying a release grants no extra permissions.

## Webhooks

`--var=webhook_enabled=true --var=webhook_url=https://<address>:8443` adds a
webhook task to the controller job and registers two webhooks in the kind:

- `default` fills in the class of the region (`default_class`) when a spec names none (the
  schema's static default `nomad` is left out then);
- `validate` rejects classes no provider of the region implements
  (`--var='classes=["nomad","aws-nlb"]'`) and class changes.

The servers call webhooks over https only. Put the certificate and key into
the job's Variable and the CA into `webhook_ca_cert`:

```shell
norca var put nomad/jobs/loadbalancer-controller webhook_cert=@lb.pem webhook_key=@lb-key.pem
norca pack upgrade ./packs/loadbalancer-operator --name=lb --var=webhook_enabled=true \
  --var=webhook_url=https://lb-webhook.internal:8443 --var-file=ca.hcl
```

Webhooks of a kind written with a management token are approved at once;
otherwise an administrator approves them (`norca object-kind webhook approve
norca.io/LoadBalancer validate`). Any change of a webhook's configuration
needs a new approval. With `webhook_failure_policy = "fail"` (the default),
writes fail while the webhook is unreachable or unapproved.

## Limits

The kind carries the server-side load management of its objects:
`write_rate` (all identities), `identity_write_rate` (each token or workload
identity, so a misbehaving controller doesn't use up the budget of users),
`max_objects_per_namespace` and `max_watches_per_identity`. Writes over a
limit get `429 Too Many Requests`; the controller SDK backs off.

## Uninstall

```shell
norca pack uninstall lb
```

deletes the job, the admission policy, the kind and the policy in reverse
order. The kind can only be deleted when no LoadBalancer objects are left:
delete them first (the controller removes their load balancers through the
finalizer).
