# State of Rig

2026-11-18. Phase 1 of the design (`design/rig-design.md`, section 15) is
under way: modules load, validate and evaluate. Nothing is installed on a
cluster yet; that is phase 2.

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
| CLI | `rig fmt [-check]`; `rig module validate [dir]` (files, icon, tests parsed, example values rendered, a warning for a variable that changes nothing); `rig module render [dir] -f -var -release -namespace -region` (JSON; secret items named, never shown); `rig module test [dir]`; log as a tree or JSON |

## What is checked

- Every module of `examples/` (web, redis, nats, postgres, the placement
  library) loads, validates and renders with each of its example value
  files, and its own tests pass (`rig module test`); the Go tests keep it
  so.
- The rendered jobs of those modules pass a Norca server's
  `/v1/validate/job` (dev agent, 2026-11-18).
- Phase 0's spike against the release API of a dev agent: what Rig
  evaluates fits today's API (`make integration`).
- `make vet race lint check-copy check-licenses` is clean; golangci-lint v2
  with the configuration of the Go skills. No dependency is under the
  Business Source License: Rig takes over no code that is not open source.

## What is not there yet

- Phase 1: a schema per resource kind (the kinds other than jobs are the
  value of their block as JSON until their kind is implemented).
- Phase 2 on: releases (install, plan, apply, status, destroy), stacks,
  registries and versions, the lock file, signatures and trust, the global
  registry, the embedding into Norca.
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
