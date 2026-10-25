# Pack releases: revisions, server-side apply, hooks, drift

When the Norca servers support it, `norca pack run` does not deploy jobs from the client any more:
it renders the pack and sends the rendered resources to the servers, which apply them in a fixed
order, record an immutable revision of the **release**, and can roll it back, test it, detect drift
and uninstall it. Rendering stays on the client. See the
[package management plan](../../../docs/design/package-management-plan.md) for the design.

- [Quick start](#quick-start)
- [Releases and revisions](#releases-and-revisions)
- [Extended package format](#extended-package-format)
- [Install, upgrade, rollback, uninstall](#install-upgrade-rollback-uninstall)
- [Atomic mode and production namespaces](#atomic-mode-and-production-namespaces)
- [Hooks and tests](#hooks-and-tests)
- [Drift detection](#drift-detection)
- [Ownership](#ownership)
- [Signatures in protected namespaces](#signatures-in-protected-namespaces)
- [Permissions](#permissions)
- [Older servers and client-side apply](#older-servers-and-client-side-apply)
- [HTTP API](#http-api)

## Quick start

```shell
norca pack plan redis --registry=community --name=cache               # what would change
norca pack run redis --registry=community --name=cache                # install: revision 1
norca pack upgrade redis --registry=community --name=cache --var=image=redis:7   # revision 2
norca pack history cache                                              # revisions
norca pack rollback cache 1                                           # revision 3 = content of 1
norca pack status redis --registry=community --name=cache             # details, conditions, resources
norca pack diff cache --live                                          # drift check (exit code 2 on drift)
norca pack test cache                                                 # run the test hooks
norca pack uninstall cache                                            # delete everything, in reverse order
```

The release name is the deployment name with characters other than letters, digits, `.`, `_` and
`-` replaced by `-`: `pack@ref` gives `pack-ref`, a local pack directory gives the pack name, and
`--name` sets it. Use `--name` for a release whose pack version changes over time, as the default
deployment name contains the version.

## Releases and revisions

A release is a custom object of the built-in kind `packs.norca.io/Release` in the namespace of the
command (`--namespace`); its revisions are objects of `packs.norca.io/ReleaseRevision` named
`<release>.<n>`, owned by the release. The kinds are registered by the first release operation.
They can be read with the object API (`norca object list packs.norca.io/Release`), but only the
release API writes them.

| Release field | Content |
| --- | --- |
| `spec.pack` | name, version, registry, path, OCI digest, signer reported by the client, signatures, signer verified by the servers, requirements |
| `spec.revision`, `spec.maxHistory`, `spec.atomic` | newest revision, history limit (default 10), mode of the last operation |
| `status.phase` | `pending`, `applying`, `deployed`, `failed`, `rolling-back`, `uninstalling` |
| `status.revision` | the deployed revision (after a failed atomic upgrade: the previous one) |
| `status.resources` | the resources the release owns, with the modify index, version and content hash after the apply |
| `status.conditions` | `Ready`, `HooksSucceeded`, `Drifted` |
| `status.lastOperation`, `status.lastTest` | the last operation (action, user, error, rollback) and test run |

A revision holds the rendered resources (msgpack, gzip, base64; at most about 224 KiB compressed),
the variable overrides (values of variables whose name looks secret, e.g. `password` or `token`,
are recorded as `(redacted)`), a hash of all overrides and var files, the action (`install`,
`upgrade`, `rollback` with `rollbackOf`) and its phase (`deployed`, `superseded`, `failed`). The
oldest revisions beyond `--max-history` are deleted; the deployed one is always kept.
Deployments, failures, rollbacks, tests and drift are written to the object event log of the
release (`norca object events packs.norca.io/Release <name>`).

## Extended package format

Besides jobs (`*.nomad.tpl`), templates in `templates/` can render other resources, identified by
their extension. The format of each file is that of the corresponding `norca ... apply` command:

| Template | Resource | Format | Order |
| --- | --- | --- | --- |
| `*.namespace.hcl.tpl` | Namespace | `norca namespace apply` | 1 |
| `*.quota.hcl.tpl` | Quota | `norca quota apply` | 2 |
| `*.acl.hcl.tpl` | ACL policy | `policy "<name>" { description, rules, job_acl { ... } }` | 3 |
| `*.var.hcl.tpl` | Variables entry | `norca var put` file, plus `secret_items = [...]` | 4 |
| `*.kind.hcl.tpl` | Object kind | `norca object-kind apply` | 5 |
| `*.admission.hcl.tpl` | Admission policy | `norca admission-policy apply` | 6 |
| `*.object.hcl.tpl` | Custom object | `norca object apply` | 7 |
| `*.nomad.tpl` | Job | job specification | 8 |

Deletion uses the reverse order. A namespace that references a quota of the same pack is created
first and attached to the quota once it exists. `*.netpol.hcl.tpl` (network rules) is reserved and
rejected until the servers support network rules.

**Secrets:** a Variables template lists the names of secret items in `secret_items` and must not
give them values. Their values come from `--var-secret=<variable path>:<item>=<value>` (repeatable)
and are written to the Variable but never stored in the release; an upgrade or rollback without
`--var-secret` keeps the stored values.

**Requirements:** `metadata.hcl` can state what the pack needs from the servers; the servers reject
the release (also in `plan`) if they can't provide it:

```hcl
pack {
  name    = "postgres-operator"
  version = "1.4.0"
  requires {
    norca    = ">= 0.1.0"            # nomad_fork is accepted as an alias
    features = ["custom-objects", "admission-cel"]
  }
}
```

Known features: `releases`, `release-hooks`, `custom-objects`, `admission-cel`, `admission-webhooks`,
`admission-release-signatures`, `quotas`, `multiregion-deployments`, `native-service-catalog`,
`backups`.

Jobs without `namespace` or `region` belong to the namespace and region of the release. All
resources of a release must be in its region; jobs with a `multiregion` block use the multiregion
deployment as usual.

## Install, upgrade, rollback, uninstall

The leader runs one operation per release at a time (a second one gets `409 Conflict`):

1. validation: requirements, admission (signatures), ownership conflicts;
2. `pre-install` or `pre-upgrade` hooks;
3. every resource in the fixed order, through the normal endpoint of its type with the caller's
   token (admission, quotas and ACL apply), then the resources of the previous revision that are
   no longer in the pack are deleted in reverse order;
4. waiting for the deployments of the jobs (unless `--detach`), up to `--timeout` (default 5m);
5. `post-install` or `post-upgrade` hooks;
6. the revision becomes `deployed`, the previous one `superseded`.

`norca pack plan` shows per resource whether it is created, updated, unchanged or deleted, with the
job diffs, without writing. `rollback <release> [revision]` applies the content of a kept revision
(default: the one deployed before the current one) as a new revision. `uninstall` (also `destroy`
for packs installed as releases) runs the `pre-delete` hooks, deletes the resources in reverse
order, runs the `post-delete` hooks and deletes the release; `--orphan` keeps the resources and
`--no-hooks` skips the hooks.

An operation interrupted by a leader change is marked `failed` by the new leader.

## Atomic mode and production namespaces

With `--atomic=true`, a failure of any step (a resource, a deployment, a hook) restores every
resource the operation changed to its state before the operation, in reverse order; jobs are
re-registered as a new version, like `norca job revert`. The release becomes `failed`, its
deployed revision stays the previous one, and the failed revision is kept for inspection. With
`--atomic=false` the release stays as far as it got.

Without `--atomic`, the namespace decides: **a namespace is a production namespace when its meta
`environment` is `production` or `prod`** (case insensitive), and operations on releases there are
atomic. The namespace meta `pack.atomic = "true"` or `"false"` overrides this for any namespace:

```hcl
name = "payments"
meta {
  environment = "production"
}
```

## Hooks and tests

A hook is a batch (or sysbatch) job of the pack with the meta `pack.hook`: `pre-install`,
`post-install`, `pre-upgrade`, `post-upgrade`, `pre-delete`, `post-delete` or `test` (several,
comma separated). Hooks of a phase run one after the other ordered by `pack.hook_weight` (then
job ID); an earlier run of a hook job is purged first. A hook succeeds when all its allocations
complete; it fails on a failed allocation or after `pack.hook_timeout` (default 5m). The deletion
policy `pack.hook_delete` is `on-success` (default: purge after success, keep failed runs for
debugging), `always` or `never`. Hook jobs run with the caller's token, like all resources.

`norca pack test <release>` runs the `test` hooks of the deployed revision and records the result
in `status.lastTest` and the event log.

## Drift detection

After every apply the servers record a hash of each resource's content (without indexes, times and
status fields). `norca pack diff <release> --live` and a check on the leader every 5 minutes compare
the live resources with these hashes: deleted or changed resources set the `Drifted` condition and
publish a `ReleaseDrifted` event on the `CustomObject` event topic. Drift is never corrected
automatically; upgrade or roll back to restore the revision.

## Ownership

A resource belongs to the release whose `status.resources` lists it. Installing a pack whose
resources belong to another release fails with `409 Conflict` unless `--take-ownership` is given,
which moves them to the new release. As before, jobs of another pack deployment and jobs not
deployed by a pack conflict too (`--deploy-override` takes them over). Jobs of a release carry the
meta `pack.release`, `pack.release_namespace` and the owner reference of the operator framework
(`norca.owner`, `norca.owner.kind`); custom objects get an owner reference to the release. Releases
delete their resources themselves, so the object garbage collector never deletes them, and
`--orphan` keeps them.

## Signatures in protected namespaces

An admission policy with the resource `release` and `trusted_keys` admits, in its namespaces, only
releases of packs with a valid cosign compatible signature of their OCI digest by one of the keys.
The client sends the signatures kept with the cached pack (`.norca-origin.json`); the servers verify
them themselves. Unsigned packs, packs from git registries or directories (no digest) and packs
signed by other keys are rejected (or only warned about with `action = "warn"`):

```hcl
name   = "signed-packs"
action = "deny"
match {
  namespaces = ["prod-*"]
  resources  = ["release"]
}
trusted_keys = [file("cosign.pub")]   # in a file for norca admission-policy apply: the PEM text

rule {
  expression = "release.pack.registry == 'internal'"
  message    = "Only packs of the internal registry"
}
```

CEL rules of such policies see `release` (`name`, `namespace`, `action`, `pack.{name, version,
registry, path, digest, signer, signed, trustedSigner}`, `resources`) and `request`. The key ID of
the verifying key is recorded as `spec.pack.trustedSigner`.

## Permissions

Release operations need the object capabilities of the kind at namespace level:

```hcl
namespace "apps" {
  object "packs.norca.io/Release" {
    capabilities = ["read", "write"]
  }
}
```

Applying a release grants nothing else: every resource is written with the caller's own token, so
someone who can't write ACL policies can't install a pack containing one. Reading the content of a
revision (`/revision/:n`) also needs `read-job` in the namespace, as it contains job
specifications.

## Older servers and client-side apply

Against servers without the release API, `run`, `plan` and `destroy` behave as Nomad Pack always
did (jobs only; packs with other resource templates are rejected). `--client-side` (or
`NORCA_PACK_CLIENT_SIDE=1`) forces that path on new servers too; `run --client-side` then records
the deployed jobs as a revision of the release (`record_only`), so that history, status, drift
and rollback work for client-side deployments as well.

## HTTP API

| Operation | HTTP |
| --- | --- |
| List, details | `GET /v1/releases?namespace=`, `GET /v1/release/:name` |
| Install, upgrade | `PUT /v1/release/:name` (body: `Pack`, `Resources`, `Secrets`, `Atomic`, `Wait`, `Timeout`, `TakeOwnership`, `MaxHistory`, `RecordOnly`) |
| Plan | `POST /v1/release/:name/plan` |
| Revisions | `GET /v1/release/:name/history`, `GET /v1/release/:name/revision/:n` |
| Rollback | `POST /v1/release/:name/rollback` (`Revision`, `Atomic`, `Wait`, `Timeout`, `DryRun`) |
| Tests | `POST /v1/release/:name/test?timeout=` |
| Drift | `GET /v1/release/:name/drift` |
| Uninstall | `DELETE /v1/release/:name?propagation=background\|orphan&no_hooks=&timeout=` |

The Go client is `client.Releases()` in `github.com/hashicorp/nomad/api`.
