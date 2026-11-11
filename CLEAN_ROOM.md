# Clean room similarity audit

Measures how similar Norca's clean room code is to the HashiCorp code it must
not be derived from, with [Dolos](https://github.com/dodona-edu/dolos) (MIT,
Ghent University), and records only file pairs and scores.

## Who runs it

The **auditor** role only: not a Norca developer and not a Claude session that
works on Norca code. The clean room rule of Norca (the
[license page](https://github.com/bacanoo-io/norca-doc/blob/main/content/docs/introduction/license.md) of the
documentation) keeps HashiCorp code published under
BSL 1.1 out of the development environment; this audit needs that code as the
other side of the comparison, so it runs in a throwaway container. The scripts
and a step-by-step description of the comparison and its parameters are in
`clean-room-audit/` of the Norca repository (`clean-room-audit/README.md`).
This document is also kept, with the same content, as `cleanroom\README.md` of
the private lab repository:

- `audit.ps1` starts `node:24-bookworm` with three mounts only: the
  `clean-room-audit/` directory (read-only), the Norca checkout it belongs to
  (read-only) and the output directory.
- Inside, `run.sh` clones Norca from that checkout (or from a URL given with
  `-NorcaUrl`) and fetches the two compared Nomad tags from GitHub (with
  retries), extracts the compared files to `/work`, runs Dolos and hands the
  results to `report.mjs`.
- `report.mjs` writes `pairs.csv` and `summary.md`: file names and scores, no
  source code. Dolos' own `files.csv` and `kgrams.csv` (which contain code) and
  its web view stay in the container, which `--rm` removes.

Developers and Claude sessions may read `summary.md` and `pairs.csv`; they do
not run the script, open the container or look up the HashiCorp files a pair
names. A pair that needs a closer look goes to the auditor, who compares the
code outside the development environment and records only the conclusion.

## Sets

| Set | Norca side | HashiCorp side |
| --- | --- | --- |
| `busl` | Every file at `-NorcaRef` with the same extension in the directories of the 45 left-out files (the clean room replacements live there) | The 45 files of Nomad v1.6.5 with a BUSL-1.1 header, as listed in the Norca import commit |
| `bugfix` | The lines each clean room commit added to a file (`commits.txt`, plus every commit whose message mentions "clean room" or the former "white room") | The lines HashiCorp added to the same file between v1.6.5 and `-BslTag` (default: the newest v1.x.y tag) |

Go, JavaScript and TypeScript files of the `busl` set are compared by syntax
tree, everything else (Handlebars, SCSS, and all `bugfix` fragments, which do
not parse on their own) character by character. Only pairs across the two
sides are reported.

## Running

From the root of a Norca checkout:

```powershell
.\clean-room-audit\audit.ps1                                   # both sets, newest BSL tag
.\clean-room-audit\audit.ps1 -Sets bugfix -BslTag v1.10.5 -Threshold 0.3
```

Needs Docker and internet access (for the HashiCorp side). The result replaces
`clean-room-result/` of the checkout, to be committed: `pairs.csv` has every
pair, `summary.md` the commit, tags and candidates of the run.
`norca/scripts/compliance-audit.sh` checks that it is in the repository. A mechanics test without any BSL code
writes elsewhere: `-Sets bugfix -BslTag v1.6.4 -OutDir <dir>` (an MPL release).

## Reading the result

Similarity is the share of the two files covered by common fragments (0 to
1). Dolos compares the syntax tree, so names and literals do not count: two
table-driven Go tests or two Ember page objects with different content but the
same shape score high. Two measures separate this from copying:

- **MPL baseline.** For every BUSL file the report gives the highest similarity
  of HashiCorp's own MPL files (the files Norca imported from v1.6.5) to it.
  Short Ember files (mirage models, abilities, page objects) reach 0.8 to 1.0
  against each other, because HashiCorp writes them all from the same pattern.
  A file Norca added is a **candidate** only if it is at or above the
  threshold (default 0.4) and above that baseline.
- **Character level.** Every same-path pair (a clean room replacement and the
  BUSL file it replaces) is compared once more character by character, which
  also counts names, literals and comments. A high syntax score with a low
  character score is shared structure; both high is a reason to look closely.
- **Restored MPL files.** v1.6.5 replaced some MPL files with BUSL files. A
  Norca file at such a path that matches the MPL file of the anchor (the
  import's second parent, the last release/1.6.x commit before the license
  change) in all but at most 10% of its lines, module path and Norca header
  lines aside, is HashiCorp's MPL code brought back. The BUSL file grew out
  of it, so they resemble each other; the report lists such files on their
  own and never as candidates.

In the `bugfix` set, generated code (`structs.generated.go`) matches
legitimately; same-path pairs (Norca and HashiCorp changing the same file)
matter most.

The auditor records a verdict for every candidate (independent,
interface-bound, or to be rewritten). The record for the clean room pages is:
date, Dolos version, Norca commit, BSL tag, threshold, the candidates and their
verdicts.

## Multiregion deployments

Nomad v1.6.5 (MPL 2.0) contains the public side of multiregion jobs: the
`multiregion` block of the jobspec (`jobspec/parse_multiregion.go`), the
structures `Multiregion`, `MultiregionStrategy` and `MultiregionRegion` with
their copy, canonicalization and plan diff code, the HTTP API, the CLI
commands (`deployment run`, `deployment unblock` and the multiregion output of
`job run`, `job plan` and `job status`) and the region forwarding of the RPC
layer. The behaviour itself was Enterprise only, and HashiCorp never published
it. The MPL tree has stubs in its place, built with the `!ent` tag, that do
nothing or return an error:

- `job_endpoint_oss.go`: `multiregionRegister`, `multiregionStart`,
  `multiregionDrop`, `multiregionStop`, `multiregionCreateDeployment`,
  `interpolateMultiregionFields`, `multiregionSpecChanged`;
- `structs/structs_oss.go`: `Multiregion.Validate`;
- `deploymentwatcher/multiregion_oss.go`: `nextRegion`, `RunDeployment`,
  `UnblockDeployment`, `CancelDeployment`.

Norca replaced these stubs with its own implementation in the import commit
`18349e4ef` (paths relative to `norca/`, the `nomad/` directory before
2026-11-02):

| File | Content |
| --- | --- |
| `norca/job_endpoint_multiregion.go` | Registration in every listed region: per-region interpolation, peer registrations, deployments that wait in the pending state, stop and drop |
| `norca/structs/multiregion.go` | Validation of the `multiregion` block |
| `norca/deploymentwatcher/multiregion.go` | The rollout across regions: pending, running, blocked and unblocked deployments, `max_parallel`, `on_failure`, retries after a partition, `Deployment.Run`, `Unblock` and `Fail` by hand |
| `norca/release_multiregion.go` | Multiregion pack releases, a Norca feature with no Nomad counterpart |
| `ui/app/components/jobs/multiregion-timeline.*` | The region timeline of the UI |

The tests are next to each file (`*_multiregion_test.go`,
`multiregion_test.go`). `norca/deploymentwatcher/multiregion.go` keeps the
HashiCorp copyright line of the MPL stub it replaced, because it keeps that
stub's function signatures and doc comments; the other files are new and
carry only the Norca header.

What the audit covers: the UI timeline is in the `busl` set (the directory of
the ACL UI files). In the 2026-11-08 run its highest pair is 0.108, and it is
not a candidate. The Go files are in neither set. Their directories hold none
of the 45 BUSL files, and HashiCorp's implementation is not public, so there
is no file to compare them with. The import commit is not in the `bugfix`
set.

## Server roles and standby servers

Nomad v1.6.5 (MPL 2.0) contains only the public side of non-voting servers:
the `non_voting_server` agent option, the `nonvoter` Serf tag, the bootstrap
that counts voters only, and the `!ent` stubs of `nomad/autopilot_oss.go`
(`autopilotPromoter`, `autopilotServerExt`, `autopilotConfigExt`), which return
autopilot's default promoter and nothing. Nomad Enterprise had non-voting
servers, redundancy zones and an enterprise autopilot; HashiCorp never
published that code.

Norca designed server roles, standby failover and observers from its own
requirements (design document `raft-observers-and-hot-standby.md`). The sources
were the Norca tree, `hashicorp/raft` (the `bacanoo-io/raft` fork) and
`hashicorp/raft-autopilot` v0.1.6 (MPL 2.0: the `Promoter` interface and the
`StablePromoter`). No Enterprise code, binary or documentation, and no
HashiCorp source or documentation of a BSL release was used. The commits say
"clean room":

| Commit | Content |
| --- | --- |
| `8087e57bc` (phase 1) | `server { role = "voter" \| "standby" \| "observer" }` instead of `non_voting_server`, the Serf tag `raft_role`, the role promoter (voters promoted when stable, observers and standbys never), `bootstrap_expect` counting voter-role servers, the role in the API and in `operator raft list-peers` |
| `8dd43588b` (phase 2) | autopilot `target_voters`, `standby_failover_after` and `standby_cooldown`; standby promotion when a voter fails and demotion when it returns; `operator raft promote-standby`; the `Server` event topic and the standby metrics |
| `2914fa12f` (phase 3) | `backup { prefer_observer = true }`: the leader asks a healthy observer to take the scheduled and manual backups (RPC `Backup.TakeForLeader`, server certificate, current leader only, the same backup configuration by fingerprint) and records them; `TakenBy` instead of `LeaderID` in the record and the manifest |
| phase 4 | the server that receives a job registration checks it before forwarding it to the leader (the mutators without webhooks, then the job validation) and runs a dry run of the placements on its own state on request (`dry_run_capacity`, also for plans); `limits { token_read_* , token_write_* }`: request limits per ACL token on the HTTP API of the servers |

The files (paths relative to `norca/`): `norca/server_role.go`,
`norca/autopilot_promoter.go`, `norca/server_standby.go`,
`norca/structs/server_role.go`, `norca/stream/topics_server.go`,
`norca/backup_observer.go`, `norca/backup/delegate_test.go`,
`norca/job_endpoint_edge.go`, `command/agent/http_ratelimit.go`,
`api/server_role.go` and `command/operator_raft_promote_standby.go` are new and
carry only the Norca header. `norca/autopilot_oss.go` keeps the HashiCorp
copyright line of the MPL stubs it fills, because it keeps their signatures and
doc comments.

What the audit covers: the Go files are in neither set. Their directories hold
none of the 45 BUSL files, and HashiCorp's implementation is not public, so
there is no file to compare them with.
