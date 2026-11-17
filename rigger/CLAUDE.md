# norca-rig

Facts for agents working in this repository. Procedures live in skills; the
design lives in `design/rig-design.md` and is the source of truth, so this
file points to it instead of repeating it.

## The project

- Rig is the package manager of Norca: a Go library (the core) and the `rig`
  program, its first host. Design: `design/rig-design.md`. The examples of
  the language are in `examples/`; nothing parses them yet.
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
- Go path and license: `github.com/bacanoo-io/norca-rig`, MPL-2.0.
- hcl/v2 stays at the commit Norca pins (`go.mod`): Rig's evaluation needs
  `hcl.EvalContext.UndefinedVariable`, which upstream hcl does not have, and
  a newer hcl in Rig would break Norca's decoder when the server embeds Rig
  (design 12.2).
- Build in Claude's Linux VM: `source ../.tools/go-env.sh` first (Go 1.27.3
  built from source, modules from the Windows module cache, `GOSUMDB=off`),
  then the targets of the Makefile. golangci-lint v2.15.0 is installed into
  `~/go/bin` from the module cache with `go install`.
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
