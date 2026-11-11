# Clean room similarity audit: how it works

This directory holds the audit that measures how similar Norca's code is to
HashiCorp code Norca must not be derived from: the Nomad code published under
BUSL-1.1 (v1.6.5 and later). It writes only file names and similarity scores,
never source code, so its results can be published and checked. Who runs it
and how its results are recorded is in [CLEAN_ROOM.md](../CLEAN_ROOM.md); the
latest result is in [clean-room-result/](../clean-room-result).

| File | Role |
| --- | --- |
| `audit.ps1` | Starts the throwaway container (PowerShell 5.1 or 7, Docker) |
| `run.sh` | Runs inside the container: collects the compared files, runs Dolos |
| `report.mjs` | Turns the Dolos results into `pairs.csv` and `summary.md` |
| `commits.txt` | Norca commits of clean room work that are always in the bugfix set |

## Isolation

The audit runs in a throwaway container (`--rm`). The container sees only
this directory (read-only), the output directory and, when a local Norca
checkout is audited, that checkout (read-only). The HashiCorp side is fetched
from GitHub inside the container. Only `pairs.csv` and `summary.md` leave it;
Dolos' own output, which contains code, is removed with the container.

## Parameters

| Parameter | Default | Meaning |
| --- | --- | --- |
| `NorcaRef` | `main` | The Norca revision to audit |
| `NorcaUrl` | the checkout `audit.ps1` belongs to | Where Norca is cloned from: a local checkout (mounted read-only) or a URL |
| `BslTag` | `latest` | The HashiCorp release the bugfix lines are compared with; `latest` is the highest `v1.x.y` tag of `hashicorp/nomad` |
| `Sets` | `busl,bugfix` | Which comparisons to run |
| `Threshold` | `0.4` | The similarity at which a pair counts as notable |
| `OutDir` | `clean-room-result/` of the checkout | Where the result is written |
| `DolosVersion` | `2.10.0` | The version of [Dolos](https://github.com/dodona-edu/dolos) |
| `Image` | `node:24-bookworm` | The container image |
| `CACert` | none | A certificate file (PEM, or DER as Windows exports it) of extra certificate authorities for npm and git in the container: the root certificate of a proxy or an antivirus that intercepts TLS. Mounted read-only; `CA_CERT` without `audit.ps1` |

The base of the comparison is always Nomad `v1.6.5`, the last MPL release,
which Norca imported.

## Step 1: inputs

1. Norca is cloned in full (the bugfix set needs the parents of commits) and
   `NorcaRef` is resolved to a commit.
2. Two Nomad tags are fetched with their trees: `v1.6.5` and `BslTag`.
3. The import commit is found by its message (`Import nomad v1.6.5`). Its
   message lists the 45 files the import left out because they carry a
   BUSL-1.1 header. If a listed file has no BUSL-1.1 header in `v1.6.5`, the
   audit stops.
4. The anchor is the second parent of the import commit: the last commit of
   the release/1.6.x branch before the license change of 2023-08-10. Its
   whole tree is MPL.

## Step 2: paths across the history

Norca's tree changed its layout over time, so every audited revision is
classified first:

| Layout | Nomad code in | Server package in |
| --- | --- | --- |
| A | `nomad/` | `nomad/nomad/` |
| B | `norca/` | `norca/nomad/` |
| C | `norca/` | `norca/norca/` |

Paths are mapped between a layout and the v1.6.5 path in both directions, and
every result uses v1.6.5 paths, so runs on different layouts can be compared.

## Step 3: the busl set

For each of the 45 BUSL files:

1. The BUSL file is taken from `v1.6.5`.
2. Every Norca file of the same extension in the same directory is taken from
   `NorcaRef` (the replacements of the BUSL files live in those directories).
3. Each Norca file gets a kind:
   - **mpl**: the file is in the import tree; HashiCorp's own MPL file,
     possibly with Norca changes.
   - **restored**: not in the import tree, but the anchor has a file at the
     same path and the two differ in at most 10% of the lines of the longer
     one, once Go module paths are normalised and the Norca header lines
     (`SPDX-FileCopyrightText`, `Norca:`) are removed. This is HashiCorp's
     MPL code from before the license change, which v1.6.5 replaced with a
     BUSL file.
   - **new**: everything else, i.e. code Norca added.
4. A Norca file at the same path as the BUSL file (a replacement) is also
   compared with it character by character.

## Step 4: the bugfix set

1. Commits: every non-merge commit up to `NorcaRef` whose message mentions
   "clean room" (or the former "white room"), plus the commits in
   `commits.txt`.
2. Files: the Go, JavaScript, TypeScript and Handlebars files each commit
   changed under the Nomad code directory of its layout; a file `BslTag` does
   not have is skipped.
3. Norca side: the lines that commit added to the file (`git diff -U0`
   against the commit's parent, added lines only).
4. HashiCorp side: the lines HashiCorp added to the same file between
   `v1.6.5` and `BslTag`.
5. Fragments of added lines do not parse as source code, so they are compared
   character by character. Empty sides are skipped.

## Step 5: comparison

Dolos runs once per set and language: Go, JavaScript and TypeScript by syntax
tree (names and literals do not count), everything else (Handlebars, SCSS,
the bugfix fragments, the character-level copies) character by character. A
run needs at least two files. Dolos compares every file with every other
file; only pairs across the two sides (one Norca file, one HashiCorp file)
are kept.

**Similarity** is the share of the two files covered by common fragments,
from 0 to 1. For every pair the longest common fragment and the total overlap
(in tokens) are recorded too.

## Step 6: evaluation

- **MPL baseline**: for each BUSL file, the highest similarity of any `mpl`
  Norca file to it, i.e. how similar HashiCorp's own MPL code is to that BUSL
  file. Short Ember files reach 0.8 to 1.0 against each other.
- **Candidate**: a pair whose Norca file is `new`, whose similarity is at or
  above `Threshold`, and above the MPL baseline of its BUSL file. `mpl` and
  `restored` files are never candidates; restored files are listed apart.
- **Character level**: same-path pairs report the character-level score next
  to the syntax score. A high syntax score with a low character score is
  shared structure; both high is a reason to look closely.
- The auditor reviews every candidate outside the development environment and
  records a verdict: independent, interface-bound, or to be rewritten.

## Outputs

`pairs.csv` has one row per cross pair:

| Column | Meaning |
| --- | --- |
| `set` | `busl` or `bugfix` |
| `language` | `go`, `javascript`, `typescript` (syntax tree) or `char` |
| `norca_file`, `upstream_file` | The two files; in the bugfix set `norca_file` starts with the 12-character commit prefix |
| `similarity` | Share of the two files covered by common fragments |
| `longest_fragment`, `total_overlap` | Longest common fragment and total overlap, in tokens |
| `same_path` | Whether both files have the same path |
| `norca_kind` | `mpl`, `restored` or `new` (busl set) |
| `mpl_baseline` | The MPL baseline of the BUSL file (busl set) |
| `char_similarity` | Character-level score of a same-path pair |
| `candidate` | Whether the pair is a candidate |

The rows are sorted by similarity, highest first, across both sets.
`summary.md` holds the metadata of the run (`norca_ref`, `norca_commit`,
`base_tag`, `bsl_tag`, `bsl_commit`, `dolos`, `sets`, `threshold`, `date`),
the candidates, the restored files, the same-path pairs and the top bugfix
pairs.

## Running

```powershell
.\clean-room-audit\audit.ps1                                   # both sets, newest BSL tag
.\clean-room-audit\audit.ps1 -Sets bugfix -BslTag v1.10.5 -Threshold 0.3
```

Without PowerShell, the same container can be started directly; run it from
the root of a Norca checkout:

```sh
docker run --rm \
  -v "$PWD/clean-room-audit:/audit:ro" -v "$PWD/clean-room-result:/out" \
  -v "$PWD:/norca-src:ro" -e NORCA_URL=/norca-src \
  -e NORCA_REF=main -e BSL_TAG=latest -e SETS=busl,bugfix \
  -e THRESHOLD=0.4 -e DOLOS_VERSION=2.10.0 \
  node:24-bookworm bash /audit/run.sh
```

A proxy or an antivirus that intercepts TLS (it re-signs the HTTPS
connections with its own root certificate) makes the downloads of the
container fail with "unable to verify the first certificate" or "server
certificate verification failed". The audit then prints the end of the log
and a hint: give the root certificate with `-CACert` (exported from the
Windows certificate store, for example from "Trusted Root Certification
Authorities"), or turn the interception off for the run.

## Limits

- Dolos measures structural and textual overlap, not derivation. A high score
  is a reason for the auditor to look, not a finding.
- Generated code (for example `structs.generated.go`) is compared like any
  other file; it matches HashiCorp's generated code because the same
  generator runs on largely the same MPL structure definitions.
- The busl set compares only the files in the directories of the BUSL files.
