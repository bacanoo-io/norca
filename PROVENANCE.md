# Provenance

How the code of this repository was produced, stated by its maintainer.

Maintainer: Joszef Oszlanczi. Commits before 2026-11-14 appear under the name
"bigjoe"; `.mailmap` maps them to this name.

## How the code was written

- **Upstream code** was imported from the releases listed in [NOTICE](NOTICE)
  and in the Origin section of the [README](README.md).
- **Norca-specific code** was written in 2026 by AI coding agents (Claude) from
  my own specifications. These documents are published in
  [design/](design/README.md) of this repository in a rewritten form, not as
  they were first written.
- **One person** filled the roles of analyst, developer and auditor. They were
  separated by agent sessions and working environments, not by people. This is
  not the two-team "clean room" procedure; see the Terms section of
  [CLEAN_ROOM.md](CLEAN_ROOM.md).
- **Human review covered** my own specifications and the revised designs made
  from them, the test and lab results, the summaries before each merge, and
  part of the generated code. It did not cover every line.

## History of this repository

- This repository has been public since 2026-11-08. The repositories it
  replaced were not public.
- Before it was published, its history was rebuilt. Eight files that the
  similarity audit had listed as candidates were rewritten on 2026-11-02 with
  a different structure, and their earlier versions are not in the history;
  the files are listed on the
  [fixes page](https://github.com/bacanoo-io/norca-doc/blob/main/content/docs/clean-room/_index.md)
  of the documentation.
- The earlier states were not preserved: the earlier, non-public repositories
  were deleted.
- Because of the rebuild, commit dates before 2026-11-08 do not show when the
  content of a commit was written. Commit `18349e4ef` is dated 2026-10-25 and
  contains work of 2026-11-02.
- What exists from the time of the work: records of the agent sessions from
  2026-10-28 on (not published), and the results of the similarity audit in
  this repository.

## The 45 left-out files and the audit

- I did not read any of the 45 files of Nomad v1.6.5 that carry a BUSL-1.1
  header before their replacements were written. The replacements were written
  by agent sessions from the MPL code that uses those files.
- The eight files rewritten on 2026-11-02: the decision to rewrite them came
  from the scores of the first runs of the similarity audit, which was still
  being developed. I did not open the HashiCorp files then. The agent was told
  to write the files again with a different structure, and nothing about the
  originals.
- Those first runs, on 2026-11-01 and 2026-11-02, were started from an agent
  session at my request. The audit runs in a container that hands back file
  names and scores only, no code. Every later run against a BSL release I
  started myself; agent sessions started the script after that only to test
  it, against the MPL-licensed tag v1.6.4 in place of a BSL release.
- As the auditor, after the code existed, I ran the similarity audit. For the
  pairs it listed as candidates I opened the HashiCorp file of the pair, that
  is source code, in HashiCorp's public repository on GitHub, and compared it
  with the Norca file (for example six pairs of the run of 2026-11-11 against
  Nomad v2.1.1). I gave the sessions that write code my conclusion about each
  pair in words, not the code. The verdicts are in
  [clean-room-result-full/review.md](clean-room-result-full/review.md).
- The audit and the compliance check need the HashiCorp files as the other
  side of the comparison, so they are fetched on my machine when those run
  (for example `refs/audit/nomad-v1.6.5`). The rule of the sessions that write
  Norca code is not to open them.

## Declarations

- I have never had access to Nomad Enterprise source code, to binaries under
  an enterprise licence, or to non-public HashiCorp documentation.
- I have never been employed or contracted by HashiCorp or IBM.
- Apart from the files I opened as the auditor, described above, I did not
  read Nomad source code published under BSL 1.1 while working on Norca.

## Limits of these statements

- The AI models used may have been trained on public code, including code
  under BSL 1.1. The similarity audit ([CLEAN_ROOM.md](CLEAN_ROOM.md)) is the
  check against reproduced code: it measures the result.
- What was and was not read rests on my word and on the session records.
- The roles were not separated by people.

Signed: Joszef Oszlanczi, 2026-11-14
