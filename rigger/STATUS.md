# State of Rigger

2026-11-21: Rig is renamed Rigger and moved into `rigger/` of the norca
repository with its history (step 4.0 of norca's `design/rig-integration.md`,
phase 7). Nothing else changed; the state below is that of 2026-11-18.

2026-11-22: the findings of the review of 2026-11-22 are fixed; what
changed holds over what is below. A credential of the rigger program goes
to its host only: `rigger login`, or `RIGGER_OCI_*` and `RIGGER_GIT_*`
with their `_HOST`. The owner of a release is a field (the namespace and
name of a stack), and `-adopt` takes one over. The servers fetch only from
registered sources and bundles, with credentials that go with the right to
read their Variable. Hidden paths are no content of a module. A plan
checks `required_version` and `requires { module }`. Archives, syntax,
values, loads and evaluations are bounded; what those bounds can't hold (a
stack overflow, the memory of the process) waits for evaluation in a
process of its own.

2026-11-18. Phases 1 to 4 and 6 of the design (`design/rigger-design.md`,
section 15) are done, and phase 5 but for its lab case: modules load,
validate, evaluate and are tested; stacks are planned and applied as
releases on a Norca cluster through today's release API; modules come from
git registries, OCI registries, directories, the global registry or the
program itself, in versions a lock file pins; modules are signed, a stack
trusts keys, and bundles carry a stack's modules to a machine without a
network. Every pack of norca-registry is a module of norca-rigger-registry
that renders what the pack renders, and norca-rigger-deployer applies a stack.
The global registry lists signed modules and stacks by namespace, Rigger
installs from it, and rigger itself keeps it (`rigger index`); stacks are
published and taken as parts of other stacks. Next: the lab run of phase 5,
then phase 7, which changes norca.

## What works

| Area | State |
| --- | --- |
| Library interface (design 12) | `rigger.New` with functional options; the ports in `host/` (cluster, job decoder, releases as three small interfaces, secrets, store, clock) and `source/`; a `*slog.Logger` wrapped so that the log is a tree of operations (`oplog`) |
| Loading (design 4.5, 7.4) | files classified by name; one resource per file, labelled like the file; limits on module size, file size, nesting; child modules from local sources inside the root they were loaded from |
| Values (design 6.1, 6.2) | types with optional attributes and defaults; validation (`pattern`, lengths, ranges, `integer`, items, `unique`/`unique_by`, `one_of`, `key_pattern`, `attribute`/`item` blocks); `rule` blocks; sensitive values shown as `(sensitive)`; hints for misspelled names |
| Evaluation (design 4.2) | `rigger.*` facts; locals and child modules in the order of their references; `check`; resources with `enabled`, `for_each`, `count`; outputs; `Engine.Evaluate` returns plain data |
| Jobs | decoded by Norca's own decoder (`DecodeJob` of the module `norca/jobspec2`); meta-arguments and the `hook` block taken off first |
| Runtime values (design 4.2) | `runtime`, `runtime_env`, `secret_ref`, `variable_ref`, `service_address`, `service_addresses` and the runtime references of a job are placeholders (package `runref`); the host writes them as `${...}` in a job's attributes and as `{{ env }}` / `norcaSecret` / `nomadVar` / `nomadService` in task templates |
| Secrets (design 6.3) | a sensitive value goes only into an item of a Variable, where it becomes a secret item; anywhere else it is an error |
| Functions | the HCL standard set under the usual names (`coalesce` skips empty strings, `length` counts objects), `issensitive`, `file`, `templatefile` (inside the module only), `image_ref`; the encoders `jsonencode`, `yamlencode`, `tomlencode`, `natsconf` written for Rigger, runtime values quoted where they sit |
| Hooks (design 5) | `hook { on, weight, timeout, delete }`, written by the Norca host as the `pack.hook*` meta keys of today's release API |
| Tests (design 7.4) | `test` blocks of `tests/*.riggertest.hcl`: values in, assertions over `resource.<block>.<name>` (the block as written) and `output.<name>`, or an expected error |
| Stacks (design 6.4, 9) | `stack.hcl` with releases of local modules, values, `depends_on`, `enabled`; plan by digest (unchanged, install, upgrade, remove), apply in dependency order with waiting, prune, status with drift, destroy; generated values (`!generate:`) and secrets created once in the cluster's secret store |
| Sources (design 7, 8.3) | `<registry>/<name>`, git addresses, `builtin/<name>`, `dir://`; versions by tags and constraints (`~> 1.2`); `rigger stack sync [-upgrade]` writes `rigger.lock.hcl`; moved tags and other content refused; a cache of mirrors and trees; git through go-git (no git program; credentials from the host, the rigger program: ssh agent or `RIGGER_GIT_USERNAME`/`RIGGER_GIT_PASSWORD`) |
| Global registry (design 7.3) | package `index` (entries, namespaces, keys, advisories); `<namespace>/<name>` resolved through `github.com/bacanoo-io/norca-global-registry` or `RIGGER_GLOBAL_REGISTRY`: the listed commit, digest and a signature of a key of the namespace required, blocked versions refused, warnings shown by sync and plan; `rigger index update` lists the new tags of the repositories of the namespaces, `rigger index check` checks the files, append-only against a base, and versions against their sources |
| Stacks in a registry (design 7.5) | `metadata.hcl` with a `stack` block; the tree digest of the stack directory; `rigger key sign|verify` of a stack; `rigger stack publish`; the stack commands on a published stack by its source with `-version`; parts (`stack "<part>" { source, version, values }`, releases `<part>-<release>`, outputs `stack.<part>.<output>`, a `stack` entry in the lock file) |
| Supply chain (design 8) | `rigger key generate|sign|verify` (cosign-format keys, the statement Norca's admission verifies); signatures in `.rigger-signatures/` of a module; `trust` per registry; signers in the lock; `rigger bundle create|inspect|import`; `oci://` sources and `rigger module publish` |
| Norca host (`cli/norcaapi`) | release API (pack fields, digest in `ValuesHash`, owner as a field), secret store, cluster facts; object kinds and objects decoded by Norca's parser (a copy of `resourcespec/object.go`) from the block written as HCL in module order; the other kinds mapped onto Norca's API type of the kind |
| Conversion (design 14) | `rigger convert pack` writes the metadata, variables and examples of a pack as a module, its templates as comments to rewrite |
| CLI | `rigger convert pack`, `rigger stack sync|validate|plan|apply|status|destroy|publish`, `rigger index update|check`, `rigger module publish [-key | -to oci://]`, `rigger install`, `rigger release list|status|history|rollback|uninstall`; `rigger fmt [-check]`; `rigger module validate [dir]` (files, icon, tests parsed, example values rendered, a warning for a variable that changes nothing); `rigger module render [dir] -f -var -release -namespace -region` (JSON; secret items named, never shown); `rigger module test [dir]`; log as a tree or JSON |

## What is checked

- The global registry end to end (Go test of the rigger program): a module
  published with a key, listed by `rigger index update`, checked, installed by
  a stack from `<namespace>/<name>`; a warning shown, a block refusing, a
  changed entry refused by the client and by `rigger index check`; a stack
  published over it, listed with the modules its lock pins, and validated
  from the registry by its source. A part of a stack planned with its
  releases, dependencies, generated values and outputs. On a copy of
  norca-rigger-registry with a key of its own: nats 2.0.0 signed, listed, and
  installed through the registry; every other tag validates.
- Every module of `examples/` (web, redis, nats, postgres, the placement
  library) loads, validates and renders with each of its example value
  files, and its own tests pass (`rigger module test`); the Go tests keep it
  so.
- The rendered jobs of those modules pass a Norca server's
  `/v1/validate/job` (dev agent, 2026-11-18).
- norca-rigger-registry: every module of the 16 packs renders the jobs, ACL
  policies, kinds and admission policies of its pack for the defaults and
  every example (85 cases, `scripts/compare-packs.sh`), and all 104
  rendered jobs pass `/v1/validate/job` (`scripts/check-modules-server.sh`).
  The kind of norca-dns applies to a dev agent with its columns in order.
- norca-rigger-deployer: the `rigger:` section applies a stack through the CLI of
  a server against a dev agent (install, unchanged, destroy).
- Against a dev agent (`make integration`): a stack planned, applied,
  unchanged, upgraded, pruned and destroyed; a generated token in the
  secret store and the Variable but never in a revision; runtime references
  in the job as written; one release installed, listed and uninstalled;
  and phase 0's spike of the release API.
- `make vet race lint check-licenses` is clean; golangci-lint v2
  with the configuration of the Go skills. No dependency is under the
  Business Source License: Rigger takes over no code that is not open source.

## What is not there yet

- Phase 1: a schema per resource kind for the kinds the host does not
  decode (namespaces, quotas, policies, admission policies, external
  services and Variables are the value of their block, mapped onto the API
  type by field names).
- Phase 5: the lab case of a stack is not written yet (norca-lab runs on
  the Hyper-V host). A variable only rules read is reported as unused, also
  when it is a gate the installer must set.
- Phase 6: icons in the user interface belong to norca (phase 7). The
  namespace `norca` of the global registry has no signing key yet: the
  maintainers make it (`rigger key generate`), and until then nothing of it is
  listed. `norca-global-registry` is committed, not pushed.
- Phase 7 on: the embedding into Norca, which changes norca. For later:
  the user configuration of registries (`rigger registry add`), OCI
  credentials from docker's configuration, `rigger login`, keyless signing.
- `norca-rigger-registry` and `norca-rigger-deployer` (next to this repository,
  branch `rigger` of each) are not pushed.
- Open for phase 4: the digest of a module whose local child module lies
  outside its directory must cover the child.

## Trying it

```shell
make build
bin/rigger module render examples/registry/modules/nats \
    -f examples/registry/modules/nats/examples/cluster-with-users.vars.hcl \
    -release bus
```

The CLI module builds against the norca checkout next to this repository
(`../norca`); see `README.md` and `CLAUDE.md` for the build.
