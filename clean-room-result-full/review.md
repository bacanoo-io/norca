# Review of the full run of 2026-11-11

The run of `summary.md` and `pairs.csv`: Norca `7b6609684` (main), base v1.6.5,
BSL release v2.1.1 (`d0f210429`), Dolos 2.10.0, threshold 0.4, `-Sets full`
(the `full` and `fullmod` parts in one run). It replaces an earlier run of the
same evening that straddled a change of the audit scripts.

**Result: no Norca file is a copy.** The 3 `fullmod` candidates are mechanical
edits of HashiCorp's MPL lines; of the 109 `full` candidates, 34 are files
imported from HashiCorp's MPL projects, 5 are the Apache-2.0 copy of tstorage,
64 resemble HashiCorp's MPL v1.6.5 code at least as closely as the BSL file,
and the auditor judged the remaining 6 (3 bound to an MPL interface, 3
independent).

## How the review was made

Two steps without BSL code, then the auditor:

1. **Origin of each Norca file:** the commit that added it and its header
   (HashiCorp headers mark the MPL imports listed in `NOTICE`).
2. **An MPL-only comparison:** the 81 Norca files named in `summary.md` against
   the v1.6.5 tree of the import commit `4af32f07e` (1769 Go and 914 JavaScript
   files), with the same Dolos version (`dolos run -l go|javascript -S 0.2`).
   For each Norca file: its best MPL similarity and its longest fragment with
   an MPL file. The MPL code **covers** a pair when the Norca file has an MPL
   v1.6.5 file it resembles at least as much, with a fragment at least as long,
   as the BSL file: the common part is then HashiCorp's MPL pattern.
3. **The auditor** looked at the BSL side of the pairs the MPL code does not
   cover and gave the verdicts.

## fullmod: 3 candidates, mechanical edits

| Norca file | Similarity | Fragment | The lines Norca added | Commit |
| --- | --- | --- | --- | --- |
| `api/deployments_test.go` | 0.878 | 36 | go vet fix: `fmt.Errorf(fmt.Sprintf(...))` → `fmt.Errorf(...)`, 4 lines, the only form | `59d6940b1` |
| `nomad/structs/config/tls_test.go` | 0.466 | 26 | `TLSPreferServerCipherSuites` removed from struct literals; gofmt realigned the MPL lines around it | `ec3fefd9d` |
| `ui/tests/integration/components/task-log-test.js` | 0.412 | 23 | Ember deprecations: `run.later` → `later`, implicit this → `{{this.x}}`, the forms of the Ember codemods | `1d61b31a4` |

## full: 109 candidates in 67 Norca files

| Group | Files | Pairs | Notes |
| --- | --- | --- | --- |
| MPL imports with HashiCorp headers | 16 | 34 | Nomad Pack v0.4.2, Nomad Autoscaler v0.6.0, nomad-driver-exec2 v0.1.1 (`NOTICE`); `ui/tests/integration/components/jobs/service-page-test.js` is the v1.6.5 MPL `job-page/service-test.js` reworked (MPL 0.981 with a 511 fragment, BSL 0.786 with 44) |
| tstorage, Apache-2.0 copy | 4 | 5 | not HashiCorp code; generic Go |
| Norca, MPL covers similarity and fragment | 21 | 36 | mostly CLI commands (`service_intention`, `admission_policy_list`, `secret_engine`, `object*`: MPL fragments 52–66 with `node_pool_*`, `eval_status`, against BSL 18–43), state store, structs, endpoint tests |
| Norca, MPL covers the fragment, lower similarity | 21 | 28 | small type and constant files (`node_scaling.go` with 24 lines, `server_role.go`, `scaling_decision.go`, `csi_usage.go`): BSL fragments 3–16, MPL 7–36 |
| Norca, longer fragment with the BSL file | 6 | 6 | the auditor's verdicts below |

### Verdicts

| Norca file | BSL file | BSL sim / fragment | MPL sim / fragment | Verdict |
| --- | --- | --- | --- | --- |
| `helper/raftbadger/store_test.go` | `helper/raftutil/migrate_test.go` | 0.521 / 25 | 0.476 / 14 | **Interface-bound.** Tests of the Badger log store through exactly the methods of `raft.LogStore` (`FirstIndex`, `LastIndex`, `GetLog`, `StoreLog`, `StoreLogs`, `DeleteRange`) and `raft.ErrLogNotFound` of hashicorp/raft v1.8.0 (MPL 2.0, used as the fork bacanoo-io/raft); written by Norca with the Badger store (`cacdde71e`). |
| `helper/users/dynamic/dynamic_test.go` | `helper/users/dynamic/pool_test.go` | 0.479 / 23 | 0.458 / 12 | **Independent.** The auditor compared the two: the BSL test is built differently. The package path is the one nomad-driver-exec2 v0.1.1 (MPL) imports for `dynamic.LookupUser`; the pool API is Norca's (`7e699b9f3`). |
| `helper/diskusage/diskusage_test.go` | `helper/escapingfs/copydir_test.go` | 0.474 / 18 | 0.485 / 14 | **Independent.** No BSL file of disk usage exists; the test pairs at 0.42–0.49 with eight unrelated test files (among them `helper/raftutil/state_test.go`, MPL baseline 1.000): the temp-file pattern of Go tests. |
| `plugins/drivers/fsisolation/fsisolation.go` | `plugins/drivers/fsisolation/isolation.go` | 0.842 / 16 | 0.267 / 4 | **Interface-bound.** The auditor: the common part is the static constants; the Norca file has more functions. The package path and `fsisolation.Unveil` are what the unchanged MPL import of nomad-driver-exec2 uses (`ab4134242`, `plugin/about.go`); `none`, `chroot` and `image` are the values of v1.6.5 MPL `plugins/drivers/driver.go`. |
| `api/secrets_delivery.go` | `client/allocrunner/cni/plugins.go` | 0.653 / 12 | 0.298 / 9 | **Independent.** No BSL counterpart (its only pair is a CNI file; no BSL file with "secret" in its path pairs with a Norca file at or above 0.4). The common part is the struct tag style of v1.6.5 MPL `api/tasks.go` (`mapstructure:"change_mode" hcl:"change_mode,optional"` and the other fields of `Template`). |
| `helper/diskusage/diskusage_other.go` | `helper/funcs_unix.go` | 0.588 / 5 | 0.400 / 3 | **Interface-bound, no other form.** A 13-line build-tag fallback returning `info.Size()`. The pattern is v1.6.5 MPL: `command/agent/log_file_{linux,bsd,windows}.go` (a per-OS function of an `os.FileInfo`, the fallback a one-line return), `client/allocdir/fs_unix.go` (`fi.Sys().(*syscall.Stat_t)`), `helper/pluginutils/loader/filter_{unix,windows}.go`. `helper/funcs_unix.go` is not in v1.6.5. |

## Notes

- The header of `summary.md` names `sets full`: the option, which runs both
  parts.
