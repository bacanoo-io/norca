# norca-rig

Facts for agents working in this repository. Procedures live in skills; the
design lives in `design/rig-design.md` and is the source of truth, so this
file points to it instead of repeating it.

## The project

- Rig is the package manager of Norca: a Go library (the core) and the `rig`
  program, its first host. Design: `design/rig-design.md`. The examples of
  the language are in `examples/`; every module there loads, validates and
  renders (`rig module render`), and the tests keep it so.
- Rules of the work (the introduction of the design): norca, norca-deployer
  and norca-registry are not changed until Rig is ready; Rig is written from
  scratch and no code is copied from the pack manager; the library has an
  interface Norca can take in (section 12); the design stays in this
  repository.
- Layout and wiring: section 12.1 of the design. Hexagonal architecture
  (ports in `host/` and `source/`), manual constructor injection, no DI
  library. Two Go modules: the core at the root, which imports nothing of
  Norca, and the standalone program in `cli/`, joined by `go.work`.
- Rules the core keeps because a server embeds it: section 12.4 (no global
  state, no environment or working directory, nothing outside the module
  reachable from a module, limits set by the host).
- **No code from source that is not open** (set by the maintainer,
  2026-11-18): nothing is taken over directly from code under the Business
  Source License (Terraform since 1.6, Nomad since 1.7, Vault, Consul) or any
  other license that is not open source, neither copied nor closely
  rewritten. Names of functions and their behaviour may be like those of
  other tools (`coalesce`, `length`); their implementation is Rig's own, or
  comes from an open-source dependency (go-cty: MIT, hcl: MPL-2.0). Code
  from Norca and from Nomad before its license change is MPL-2.0 and keeps
  its headers. `make check-licenses` fails on a dependency or a file under
  the Business Source License; a new dependency is checked by hand too.
- Go path and license: `github.com/bacanoo-io/norca-rig`, MPL-2.0.
- hcl/v2 stays at the commit Norca pins (`go.mod`): Rig's evaluation needs
  `hcl.EvalContext.UndefinedVariable`, which upstream hcl does not have, and
  a newer hcl in Rig would break Norca's decoder when the server embeds Rig
  (design 12.2).
- Build in Claude's Linux VM: `source ../.tools/go-env.sh` first (Go 1.27.3
  built from source, modules from the Windows module cache, `GOSUMDB=off`),
  then the targets of the Makefile. golangci-lint v2.15.0 is installed into
  `~/go/bin` from the module cache with `go install`.
- `make integration` runs the tests tagged `integration` against a running
  agent (`NORCA_ADDR`, default 127.0.0.1:4646). In the VM: build norca from
  `../norca/norca` with `CGO_ENABLED=1 go build -o ~/bin/norca .` (the
  build cache carries it over a few calls), then start
  `~/bin/norca agent -dev -bind 127.0.0.1` in the background and run the
  tests in the same call: a process does not outlive the call. The same
  agent checks rendered jobs: `PUT /v1/validate/job` with `{"Job": <spec>}`
  from `rig module render`; the decoder accepts jobs the server refuses.
- Runtime values (`runtime_env`, `secret_ref`, `service_address`, a runtime
  reference in a job) are placeholders of package `runref`; the job decoder
  of the host resolves them where they land (design 4.2). Template functions
  of Norca: `norcaSecret` (`nomadSecret` is an alias), `nomadVar`,
  `nomadService`, environment `NORCA_*`.
- `cli/internal/jobspec2` is a copy of norca's jobspec2 until phase 7
  (design, question 4): never edit the copied files; Rig's code there is
  `rig_*.go`. `make check-copy` compares the copy with `../norca`; when
  norca changes them, copy them again and update `SOURCE.md`.
- The CLI module imports norca through `replace` to the checkout next to
  this repository (`../../norca/norca`), with norca's own replacements
  repeated in `cli/go.mod`.
- Logging (design 9.5): a `*slog.Logger`, always the `Context` variants;
  each operation is an `oplog` span (`oplog.Start` / `Span.End`), so the
  log is a tree. An error is returned, not logged on the way up.
- New Go files start with the SPDX header of Norca
  (`SPDX-FileCopyrightText: 2026 Norca contributors`,
  `SPDX-License-Identifier: MPL-2.0`).

## Go development

Before any Go coding, review, debugging, troubleshooting, or setup task, load the `samber/cc-skills-golang@golang-how-to` skill first — it routes to whichever other Go skills the task needs.

## Required Go skills

The following Go skills from `samber/cc-skills-golang` MUST always be applied when working on this project. Load them at the start of every Go-related task, regardless of whether the user explicitly mentions them.

- `samber/cc-skills-golang@golang-code-style`
- `samber/cc-skills-golang@golang-data-structures`
- `samber/cc-skills-golang@golang-design-patterns`
- `samber/cc-skills-golang@golang-documentation`
- `samber/cc-skills-golang@golang-error-handling`
- `samber/cc-skills-golang@golang-modernize`
- `samber/cc-skills-golang@golang-naming`
- `samber/cc-skills-golang@golang-safety`
- `samber/cc-skills-golang@golang-security`
- `samber/cc-skills-golang@golang-testing`
- `samber/cc-skills-golang@golang-troubleshooting`
- `samber/cc-skills-golang@golang-context`
- `samber/cc-skills-golang@golang-concurrency`
- `samber/cc-skills-golang@golang-structs-interfaces`
- `samber/cc-skills-golang@golang-lint`
