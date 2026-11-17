# Spike: Norca's job decoder with values from outside (question 4)

Phase 0 of the design, 2026-11-17. Result: section 12.2 and question 4 of
`design/rig-design.md`.

- `zz_rigspike.go` is the entry point the spike tries out:
  `jobspec2.DecodeJob(block, ctx)`, the decoding of `jobspec2` with an
  evaluation context given from outside.
- `zz_rigspike_test.go` decodes a job file of a module (values of `var`,
  `local` and `rig`, the function `image_ref`, a `dynamic` block, runtime
  references) through it and checks the job.
- `zz_rigspike2_test.go` shows what `ParseWithConfig` does with the same
  input today.

Nothing of norca is changed: `run.sh` adds the files to the package for one
`go test` run with `-overlay`. In Claude's Linux VM, source
`../../../../.tools/go-env.sh` first.
