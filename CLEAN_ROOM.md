# Similarity audit

Measures how similar the code Norca wrote independently is to the HashiCorp
code it must not be derived from, with [Dolos](https://github.com/dodona-edu/dolos) (MIT,
Ghent University), and records only file pairs and scores.

## Terms

"Clean room" was used in this project for four different things. None of them
is the two-team procedure the words usually mean, in which the people who
write the code have never seen the protected work. Each has its own name now:

| Name | What it is | Earlier name |
| --- | --- | --- |
| Independent reimplementation from interfaces | The 45 files of Nomad v1.6.5 with a BUSL-1.1 header were left out of the import and written again from the MPL code that uses them, without the originals | clean room replacements |
| Independent development under source rules | Features whose Nomad counterpart was Enterprise only, built from Norca's own design; the rules say which sources may be used | clean room development |
| Independent fix from the public issue | A defect that HashiCorp fixed in a BSL release is fixed from the public issue and the changelog, without the BSL code or diff | clean room rule |
| Isolated-environment fixes | The fixes of the code review of October 2026, made on an empty drive with only the Norca repositories on it. There is no protected original behind them: the name is about the environment | clean room fixes |

The audit finds the code by labels. A commit made under one of the rules
above ends with a `Source-Rule:` trailer that names it:
`reimplementation`, `independent-development`, `independent-fix` or
`isolated-environment`. Commits before 2026-11-14 say "clean room" in their
message instead, and the fixes of October 2026 carry `// clean room bugfix:`
comments in the code. These older labels stay as they are; they mark where
the rules applied and do not say that a protected original existed.

One person, the maintainer, filled the roles of analyst, developer and
auditor, with AI coding agents (Claude) writing the code. The roles were
separated by sessions and working environments, not by people. What shows
that the code is independent is the similarity audit below, which measures
the result.

## Who runs it

The **auditor** role only: the maintainer, outside the environment in which
Norca code is written, and never a Claude session that works on Norca code.
The source rule of Norca (the
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
| `busl` | Every file at `-NorcaRef` with the same extension in the directories of the 45 left-out files (the independent reimplementations live there) | The 45 files of Nomad v1.6.5 with a BUSL-1.1 header, as listed in the Norca import commit |
| `full` | Every file of the Nomad code that Norca wrote (new since the import); for a file Norca changed, only the lines it added to its v1.6.5 file | For a new file: every file of the same language of `-FullTag` (default: the newest release of any major version), whatever its path, with the MPL baseline; for the added lines: the lines HashiCorp added to the same file between v1.6.5 and `-FullTag` |
| `bugfix` | The lines each commit made under the rules added to a file (`commits.txt`, plus every commit with a `Source-Rule:` trailer or whose message mentions "clean room" or the former "white room") | The lines HashiCorp added to the same file between v1.6.5 and `-BslTag` (default: the newest v1.x.y tag) |

Go, JavaScript and TypeScript files of the `busl` set are compared by syntax
tree, everything else (Handlebars, SCSS, and all `bugfix` fragments, which do
not parse on their own) character by character. Only pairs across the two
sides are reported.

## Running

From the root of a Norca checkout:

```powershell
.\clean-room-audit\audit.ps1                                   # both sets, newest BSL tag
.\clean-room-audit\audit.ps1 -Sets bugfix -BslTag v1.10.5 -Threshold 0.3
.\clean-room-audit\audit.ps1 -Sets full -OutDir clean-room-result-full     # newest release, every path
```

The `full` set is run on its own into `clean-room-result-full/`: the published
`busl` and `bugfix` runs keep comparing with the newest v1 release, the `full`
run compares with the newest release of any major version. A new file is a
candidate there only if it resembles a BSL file more than HashiCorp's own
v1.6.5 code does; a changed file is judged by the lines Norca added to it,
against the lines HashiCorp added (whole files would measure their common MPL
ancestor). The review of the candidates, with the auditor's verdict for each,
is kept next to the result as `review.md`.

Needs Docker and internet access (for the HashiCorp side). The result replaces
`clean-room-result/` of the checkout, to be committed: `pairs.csv` has every
pair at or above the threshold, `summary.md` the commit, tags, counts and
candidates of the run.
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
- **Character level.** Every same-path pair (an independent reimplementation and the
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
interface-bound, or to be rewritten). The record for the pages of the documentation is:
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
| `a99a443c4` (2026-11-12) | five rules of the standby failover put right after a review: a manual failover without `-replace` does not wait for `standby_failover_after`; a voter is demoted for its role only while the voters that are left keep a healthy majority; the cooldown counts from the change as it shows in the autopilot state; `promote-standby -replace` removes nothing unless a promotion can follow; with a target a failed voter is not removed before the failover time |
| `9469d0d5f` (2026-11-12) | the autopilot configuration is part of the Raft snapshot, so a server that restored its state from one keeps what `operator autopilot set-config` set |

The files (paths relative to `norca/`): `norca/server_role.go`,
`norca/autopilot_promoter.go`, `norca/server_standby.go`,
`norca/structs/server_role.go`, `norca/stream/topics_server.go`,
`norca/backup_observer.go`, `norca/backup/delegate_test.go`,
`norca/autopilot_promoter_rules_test.go`, `norca/fsm_autopilot.go`,
`norca/state/autopilot_restore.go`,
`norca/job_endpoint_edge.go`, `command/agent/http_ratelimit.go`,
`api/server_role.go` and `command/operator_raft_promote_standby.go` are new and
carry only the Norca header. `norca/autopilot_oss.go` keeps the HashiCorp
copyright line of the MPL stubs it fills, because it keeps their signatures and
doc comments.

What the audit covers: the new files are in neither set. Their directories
(`nomad/`, `nomad/structs/`, `nomad/stream/`, `nomad/backup/`, `nomad/state/`,
`api/`,
`command/`, `command/agent/`) hold none of the 45 BUSL files, and HashiCorp's
implementation is not public, so there is no file to compare them with. The
lines these commits added to files that come from v1.6.5 (for example
`nomad/autopilot.go`, `nomad/leader.go`, `nomad/server.go`,
`nomad/operator_endpoint.go`, `nomad/job_endpoint.go` and the generated
`structs.generated.go`) are in the `bugfix` set: the commits say "clean room",
so `run.sh` compares them with the lines HashiCorp added to the same files up
to the BSL tag.

## CPU usage of raw_exec tasks

In Nomad v1.6.5 (MPL 2.0) the executor without isolation, of the `raw_exec` driver
(`drivers/shared/executor/executor.go`, `pid_collector.go`) measures the CPU
of a task as the sum over the processes it finds in a scan every five seconds.
A task that keeps starting processes shorter than the scan reads as nearly
idle, which Norca saw in its own lab (2026-11-12, the load generator of the
step `appscaling-load`) when the autoscaler scaled on that number.

Norca reads the CPU time of the task from the `cpu.stat` file of its v2 cgroup
instead (2026-11-13, the commit says "clean room"). The sources were the Norca
tree: the cgroup the executor already makes for the task
(`executor_universal_linux.go`, `client/lib/resources/containment_linux.go`)
and the `exec` driver of v1.6.5, which reads the CPU of its cgroup the same
way (`executor_linux.go`). The format of `cpu.stat` is from the Linux kernel
documentation (`Documentation/admin-guide/cgroup-v2.rst`). Later Nomad
releases changed how the executor collects statistics; no HashiCorp source or
documentation of a BSL release was opened or used.

The change is in lines added to files that come from v1.6.5
(`client/lib/resources/containment.go`, `containment_linux.go`,
`drivers/shared/executor/executor.go`), which are in the `bugfix` set of the
audit, and in the new test file
`client/lib/resources/containment_linux_test.go`, which carries only the Norca
header.

What the lab showed (2026-11-13, a `raw_exec` task on a node with cores of
4983 MHz: a shell that keeps starting children that count for a moment, its
cgroup at one full core). The use of the cgroup as the node measures it, and
the CPU column of `norca alloc status -stats`, three readings ten seconds
apart, before and after the change:

```text
before  round 1: cgroup used 100% of a core over 10s     0/100 MHz
        round 2: cgroup used 100% of a core over 10s    48/100 MHz
        round 3: cgroup used 100% of a core over 10s     0/100 MHz
after   round 1: cgroup used 100% of a core over 10s  5000/100 MHz
        round 2: cgroup used 100% of a core over 10s  4982/100 MHz
        round 3: cgroup used 100% of a core over 10s  4986/100 MHz
```

## Task cgroups removed by another agent

In Nomad v1.6.5 (MPL 2.0) the cpuset manager of cgroups v2
(`client/lib/cgutil/cpuset_manager_v2.go`) makes the cgroup of a task when its
allocation is added, and the task runner waits until that cgroup exists. The
cleanup of the manager removes every scope of the cgroup parent that it does
not track and that holds no process. Agents that share one cgroup parent (in
Norca's lab the node agents in containers with the cgroup namespace of their
host) remove each other's cgroups between the two, and the task waits for
ever (lab step `nodescaler-dc`, 2026-11-12 and 2026-11-13).

Norca makes the cgroup of a tracked task again when the wait finds it missing
(2026-11-13, the commit says "clean room"). It was found from the goroutines
of a client in the lab and written from the Norca tree alone. Later Nomad
releases replaced this manager; no HashiCorp source or documentation of a BSL
release was opened or used. The change is in lines added to a file that comes
from v1.6.5, which is in the `bugfix` set of the audit.

What the lab showed (step `nodescaler-dc`, 2026-11-13, three runs of three
before the change): the check that failed, the events of the task that did
not start, and the goroutine of its task runner on the client.

```text
[FAIL] the work of the drained node moves back to r1-dc1: one allocation there, two in r1-dc3 on two nodes

2026-11-13T08:07:41+01:00  Task Setup  Building Task Directory
2026-11-13T08:07:41+01:00  Received    Task received by client

goroutine 751 [select, 8 minutes]:
github.com/bacanoo-io/norca/norca/client/lib/cgutil.(*cpusetManagerV2).CgroupPathFor.func1({0x85de7c8, 0x342131e92820})
	github.com/bacanoo-io/norca/norca/client/lib/cgutil/cpuset_manager_v2.go:211 +0x231
github.com/bacanoo-io/norca/norca/client/allocrunner/taskrunner.(*TaskRunner).runDriver(0x34212fc8b188)
	github.com/bacanoo-io/norca/norca/client/allocrunner/taskrunner/task_runner.go:883 +0x163
```

With the change the step passed. The docker node provider of the lab has
since given the agent of every container a cgroup parent of its own, and the
step checks that no agent had to make a cgroup again (2026-11-13):

```text
[PASS] each of the three nodes keeps the cgroup of its task under a cgroup parent of its own on the host
[PASS] the work of the drained node moves back to r1-dc1: one allocation there, two in r1-dc3 on two nodes
[PASS] no agent of the containers found the cgroup of a task removed
```

## The cgroups v1 parent of an agent in a container

In Nomad v1.6.5 (MPL 2.0) the path of a cgroups v1 cgroup
(`GetCgroupPathHelperV1` in `client/lib/cgutil/cpuset_manager_v1.go`) is made
relative to the root of the mount of its hierarchy. In a container that root
is the cgroup of the container on the host, and the parent cgroup of the
agent (`/nomad`), which is not under it, resolved to a path outside the mount
(`/sys/fs/nomad`): the agent then ran without cpuset management and said so
only in a warning (test `TestCpusetManager_V1_Init` in a container,
2026-11-13).

Norca takes a cgroup that is not under the root of the mount as one of the
mount (2026-11-13, the commit says "clean room"). It was found from the
failing test and the mount table of the container and written from the Norca
tree alone. Later Nomad releases replaced this manager; no HashiCorp source or
documentation of a BSL release was opened or used. The change is in lines
added to a file that comes from v1.6.5, which is in the `bugfix` set of the
audit.

What the test showed (2026-11-13, the tests of `client/lib/cgutil` as root
in a container on a host with cgroups v1), before and after the change:

```text
before  mkdir /sys/fs/gotest-<id>/shared: no such file or directory
        panic: interface conversion: cgutil.CpusetManager is *cgutil.NoopCpusetManager, not *cgutil.cpusetManagerV1
after   initialized cpuset cgroup manager: parent=/gotest-752e1d51 cpuset=0-31
        --- PASS: TestCpusetManager_V1_Init (0.00s)
```
