# State of Rig

2026-11-18. Phases 1 and 2 of the design (`design/rig-design.md`, section
15) are done, and phase 3: modules load, validate, evaluate and are
tested; stacks are planned and applied as releases on a Norca cluster
through today's release API; modules come from git registries, directories
or the program itself, in versions a lock file pins. Next: phase 4, the
supply chain (signatures, trust, bundles, OCI).

## What works

| Area | State |
| --- | --- |
| Library interface (design 12) | `rig.New` with functional options; the ports in `host/` (cluster, job decoder, releases as three small interfaces, secrets, store, clock) and `source/`; a `*slog.Logger` wrapped so that the log is a tree of operations (`oplog`) |
| Loading (design 4.5, 7.4) | files classified by name; one resource per file, labelled like the file; limits on module size, file size, nesting; child modules from local sources inside the root they were loaded from |
| Values (design 6.1, 6.2) | types with optional attributes and defaults; validation (`pattern`, lengths, ranges, `integer`, items, `unique`/`unique_by`, `one_of`, `key_pattern`, `attribute`/`item` blocks); `rule` blocks; sensitive values shown as `(sensitive)`; hints for misspelled names |
| Evaluation (design 4.2) | `rig.*` facts; locals and child modules in the order of their references; `check`; resources with `enabled`, `for_each`, `count`; outputs; `Engine.Evaluate` returns plain data |
| Jobs | decoded by Norca's own decoder (a copy of `jobspec2` with `DecodeJob` in `cli/internal/jobspec2` until phase 7); meta-arguments and the `hook` block taken off first |
| Runtime values (design 4.2) | `runtime`, `runtime_env`, `secret_ref`, `variable_ref`, `service_address`, `service_addresses` and the runtime references of a job are placeholders (package `runref`); the host writes them as `${...}` in a job's attributes and as `{{ env }}` / `norcaSecret` / `nomadVar` / `nomadService` in task templates |
| Secrets (design 6.3) | a sensitive value goes only into an item of a Variable, where it becomes a secret item; anywhere else it is an error |
| Functions | the HCL standard set under the usual names (`coalesce` skips empty strings, `length` counts objects), `issensitive`, `file`, `templatefile` (inside the module only), `image_ref`; the encoders `jsonencode`, `yamlencode`, `tomlencode`, `natsconf` written for Rig, runtime values quoted where they sit |
| Hooks (design 5) | `hook { on, weight, timeout, delete }`, written by the Norca host as the `pack.hook*` meta keys of today's release API |
| Tests (design 7.4) | `test` blocks of `tests/*.rigtest.hcl`: values in, assertions over `resource.<block>.<name>` (the block as written) and `output.<name>`, or an expected error |
| Stacks (design 6.4, 9) | `stack.hcl` with releases of local modules, values, `depends_on`, `enabled`; plan by digest (unchanged, install, upgrade, remove), apply in dependency order with waiting, prune, status with drift, destroy; generated values (`!generate:`) and secrets created once in the cluster's secret store |
| Sources (design 7, 8.3) | `<registry>/<name>`, git addresses, `builtin/<name>`, `dir://`; versions by tags and constraints (`~> 1.2`); `rig stack sync [-upgrade]` writes `rig.lock.hcl`; moved tags and other content refused; a cache of mirrors and trees |
| Norca host (`cli/norcaapi`) | release API (pack fields, digest in `ValuesHash`, owner in the description), secret store, cluster facts; every kind mapped onto Norca's API type of the kind |
| CLI | `rig stack sync|validate|plan|apply|status|destroy`, `rig install`, `rig release list|status|history|rollback|uninstall`; `rig fmt [-check]`; `rig module validate [dir]` (files, icon, tests parsed, example values rendered, a warning for a variable that changes nothing); `rig module render [dir] -f -var -release -namespace -region` (JSON; secret items named, never shown); `rig module test [dir]`; log as a tree or JSON |

## What is checked

- Every module of `examples/` (web, redis, nats, postgres, the placement
  library) loads, validates and renders with each of its example value
  files, and its own tests pass (`rig module test`); the Go tests keep it
  so.
- The rendered jobs of those modules pass a Norca server's
  `/v1/validate/job` (dev agent, 2026-11-18).
- Against a dev agent (`make integration`): a stack planned, applied,
  unchanged, upgraded, pruned and destroyed; a generated token in the
  secret store and the Variable but never in a revision; runtime references
  in the job as written; one release installed, listed and uninstalled;
  and phase 0's spike of the release API.
- `make vet race lint check-copy check-licenses` is clean; golangci-lint v2
  with the configuration of the Go skills. No dependency is under the
  Business Source License: Rig takes over no code that is not open source.

## What is not there yet

- Phase 1: a schema per resource kind (the kinds other than jobs are the
  value of their block as JSON until their kind is implemented).
- Phase 4 on: signatures and trust, bundles and OCI, the conversion of the
  other packs, the deployer with stacks, the global registry, the embedding
  into Norca (phase 7, which changes norca).
- `norca-rig-registry` (next to this repository) holds the first modules,
  tagged; it is not pushed.
- Open for phase 4: the digest of a module whose local child module lies
  outside its directory must cover the child.

## Trying it

```shell
make build
bin/rig module render examples/registry/modules/nats \
    -f examples/registry/modules/nats/examples/cluster-with-users.vars.hcl \
    -release bus
```

The CLI module builds against the norca checkout next to this repository
(`../norca`); see `README.md` and `CLAUDE.md` for the build.
