# A temporary copy of norca's jobspec2

The non-test files of `norca/norca/jobspec2` of norca, commit
`9a93ae5216dfc5aef2788dd956ab035a8c3dc16f`, unchanged, with their own
headers (HashiCorp, Inc., modifications by Norca contributors, MPL-2.0):

- `functions.go`
- `hcl_alloc_affinity.go`
- `hcl_conversions.go`
- `hcl_scaling.go`
- `parse.go`
- `parse_job.go`
- `parse_map.go`
- `types.config.go`
- `types.variables.go`

Rigger adds `rigger_decode.go` (`DecodeJob`) and its test. `DecodeJob` needs the
unexported decoding of the package, so it can only live inside a copy of it
until norca exports it (design, question 4).

**Why a copy:** norca is not changed before phase 7. **When it goes:** in
phase 7 `DecodeJob` moves into norca, `cli/norcaapi` imports norca's
jobspec2, and this directory is deleted (decided by the maintainer,
2026-11-17).

The copy is not edited, linted or formatted here (`.golangci.yml`).
`make check-copy` compares it with the norca checkout next to this
repository and fails when they differ: then copy the files again and update
the commit above.
