# Licensing and repository structure

**Status:** Accepted direction · 2026-10-24 · Related documents: [nomad-fork-development-plan.md](design/nomad-fork-development-plan.md) and the other design documents

This document records under which license each part of the project falls, where the starting code comes from, which dependencies are allowed, and what must be observed in the public repository, in binary releases and in contributions. It is not legal advice; before the first public release it is worth having it reviewed by a lawyer experienced in open source licenses.

## Principle

**Wherever possible, everything is MPL 2.0.** This applies to the Nomad fork (modified and new files alike), the Nomad Pack fork, the standalone components (for example the controller SDK in `nomad/controller` and the NATS bridge in `nomad/natsbridge`) and the documentation. The only exceptions are files taken from third parties, unchanged or modified: these keep their original license.

**There is no separate control plane.** Every feature is built into the fork, according to the design documents.

## Starting code

| Component | Version | Commit | License | Note |
| --- | --- | --- | --- | --- |
| HashiCorp Nomad | v1.6.5 (2023-12-13) | `a7cfff372cd52b02232a02bd3ffb5e5d35f88a0f` | MPL 2.0 | The last MPL release; everything from v1.6.6 on is BSL 1.1 |
| HashiCorp Nomad Autoscaler | v0.6.0 (2026-09-28) | `a24707db8f2abd3ee74ce0cb5cc0d2b699d2cace` | MPL 2.0 | There was no license change; its code goes into the `nomad/autoscaler` package of the Nomad fork |
| HashiCorp Nomad Pack | v0.4.2 (2026-03-13) | `cd4d2a41c22adba1350b8d5da63252f2dde33262` | MPL 2.0 | Upstream, its test helper (`internal/cli/testhelper`) pulls in the BSL main Nomad module; in the fork it lives in `nomad/pack/` inside the Nomad module and uses the fork's own packages |
| uBixVault | v1.5.0 (2026-09-30) | `817554750c2ccd2dd9a8ac072615630ade05b025` | BSD-3-Clause | The core of the built-in secret store ([secrets-plan.md](design/secrets-plan.md)); imported with its history into `nomad/secretstore/ubix/`, where `nomad/tools/ubixsync` removes the parts Norca does not use, rewrites the import paths to the Nomad module and generates the facades of its internal packages. Its files keep the BSD-3-Clause license (`REUSE.toml`, `THIRD_PARTY_NOTICES.md`) |

Nomad v1.6.5 goes into the repository together with its git history (for example `git subtree add --prefix nomad https://github.com/hashicorp/nomad.git v1.6.5`), so anyone can verify that the starting point was an MPL commit. Nomad Pack likewise, with its history; it was imported into `nomad-pack/` and later moved into `nomad/pack/`, where it is part of the Nomad Go module and runs as the `norca pack` subcommand (`git log --follow` keeps the history).

**Consul is not part of the repository.** The Consul-compatible layer implements the documented behavior of Consul's public API and contains no Consul code. The conformance tests use the `github.com/hashicorp/consul/api` client (MPL 2.0).

**Using the official Consul.** If an installation needs a full Consul (for example for a service mesh), the user installs the official HashiCorp release under its own license terms (BSL 1.1). The project does not distribute, include or modify Consul: the fork connects to it only through the MPL-licensed `consul/api` client and the documented HTTP API, and any Consul package of the package manager refers only to the official image or download. The MPL version v1.16.4 of Consul is included only as a reference (for example as a model for the `raft-wal` integration); it is not recommended for a production service mesh because of its unpatched security issues (see [consul-independence-plan.md](design/consul-independence-plan.md), phase 6).

## What must not enter the repository

- Code, diffs or code snippets from the versions released by HashiCorp under BSL 1.1: Nomad v1.6.6 and later, Consul v1.16.5 and later (everything from v1.17.0), and the BSL versions of any other HashiCorp product.
- Non-public code or internal documentation of HashiCorp's Enterprise products.
- The BSL version of the `github.com/hashicorp/nomad` main module in any `go.mod` file, not even as a test dependency. Components that use the Nomad API use the fork's own `api` package (with a `replace` directive).

**Clean room rule for BSL fixes:** if a fix already exists in HashiCorp's BSL releases, the analyst hands it over as a description of the problem, based on the public issue and the CHANGELOG; the developer writes their own solution without looking at the BSL diff. The pull request must state which public issue it is based on.

**BUSL-marked files inside v1.6.5:** 45 files of the Nomad v1.6.5 tag carried an `SPDX-License-Identifier: BUSL-1.1` header. They entered the 1.6.x release branch through backports between 2023-08 and 2023-12 and never existed under MPL 2.0 (the ACL roles/tokens UI, `helper/backoff.go`, two test doubles, a few tests and the rescheduling e2e suite). They were removed and reimplemented clean room on 2026-10-24: the analyst wrote functional specifications from exported signatures, MPL call sites, the public API and public changelog/PR titles only, without reading the files; the developers worked in worktrees where the files were already deleted and were forbidden to read them or the commits that introduced them. The rescheduling e2e suite was restored from its earlier, MPL-licensed upstream version instead. `reuse lint` must not report `BUSL-1.1` anywhere.

**Licenses becoming free:** four years after their release, the BSL versions move to MPL 2.0 (Nomad v1.6.6: 2028-01-15, Nomad v1.6.10: 2028-04-16, Nomad v1.7.0: 2027-12-07, Consul v1.17.0: 2027-11-03). After that, the code of the given version can be freely adopted; until then it cannot.

## File-level license marking

The repository follows the REUSE specification (reuse.software). The license texts are in the `LICENSES/` directory, every file gets an SPDX header, or `REUSE.toml` describes its license. `reuse lint` runs in CI and fails the build on incorrect marking.

**Headers:**

| File | Header |
| --- | --- |
| An original HashiCorp file, unmodified | Unchanged (`Copyright (c) HashiCorp, Inc.`, `SPDX-License-Identifier: MPL-2.0`) |
| An original HashiCorp file, modified | The original lines stay; alongside them may be added: `SPDX-FileCopyrightText: <year> Norca contributors` |
| New file | `SPDX-FileCopyrightText: <year> Norca contributors` and `SPDX-License-Identifier: MPL-2.0` |
| File taken from a third party (for example Kubernetes, Apache 2.0) | The original header unchanged; if modified, a prominent comment about the modification (mandatory under section 4.b of Apache 2.0) |
| Documentation (`docs/`) | MPL 2.0, given at directory level in `REUSE.toml` |
| Mockup (`docs/design/mockups/`) | Own part: MPL 2.0; the embedded Tabler CSS and Tabler icons: MIT, marked in the file |

We do not mix licenses within one file: code from a third party goes into a separate file, with its own license.

## Dependencies

**Allowed licenses** in the compiled binaries and in the UI bundle: MPL-2.0, Apache-2.0, MIT, BSD-2-Clause, BSD-3-Clause, ISC, and the Go and Go standard library license (BSD-3-Clause).

**Legacy UI exceptions** (decision of 2026-10-25): the old Ember UI, served at `/ui/legacy` for one release, may also contain `robust-predicates` (Unlicense), `caniuse-lite` (CC-BY-4.0, build time only) and `jsonify` (public domain, build time only). The exceptions are recorded in `nomad/tools/thirdparty/overrides.go` and in the `ui-licenses` CI job, are removed together with the legacy UI, and do not apply to the new UI.

**Prohibited:**

| License or package | Reason |
| --- | --- |
| BUSL-1.1 (every HashiCorp BSL module) | Not open source, with distribution restrictions |
| SSPL, Elastic License, Commons Clause and similar | Not open source |
| ApexCharts (`apexcharts`) | Dual license: the community license prohibits embedding in a product used by others |
| GPL and AGPL in the compiled binary | It would change the distribution terms of the entire binary |

**Checking:** in CI, `go-licenses` runs on every Go module, and an npm license checker runs for the UI, against the allowed list; the build fails on a prohibited or unknown license. A new dependency can only be added if its license is on the allowed list; an exception requires a decision, with an update of `docs/licensing.md`.

**NATS:** the license of NATS was disputed in 2025 (a planned switch to BSL); the project stayed in the CNCF under Apache 2.0. Verified on 2026-10-24 for the versions in `nomad/natsbridge/go.mod`: `nats.go` v1.54.0, `nkeys` v0.4.16, `nuid` v1.0.1 and, as a test-only dependency, `nats-server/v2` v2.15.0 (with `jwt/v2` v2.8.2) each ship the unmodified Apache License 2.0 text, their Go files carry Apache 2.0 headers, and none contains a BUSL or other non-open-source marking. A NATS version bump must repeat this check.

**Supply chain:** a new dependency version is only accepted if it was released at least two weeks ago.

## Distribution: source, binaries, container images

- **Source:** the public GitHub repository satisfies the source-access condition of the MPL.
- **Binaries** (MPL 3.2): the description of every release links the exact source tag and states that the code is available under MPL 2.0. The `LICENSE`, the `NOTICE` and `THIRD_PARTY_NOTICES.md` (with the NOTICE contents of the Apache 2.0 licensed dependencies, as required by section 4.d) accompany the binary.
- **Container images:** the same files in the image (for example `/usr/share/doc/norca/`), and the `org.opencontainers.image.source`, `org.opencontainers.image.revision` and `org.opencontainers.image.licenses` labels.
- **SBOM:** a CycloneDX or SPDX format software bill of materials per release, for the binaries and the images.

**Implementation:** `nomad/tools/thirdparty` generates `THIRD_PARTY_NOTICES.md` (`make notices` in `nomad/`) from the Go modules linked into `norca` and `norca-natsbridge` on every release platform and from the production npm dependencies of the UI, and writes a CycloneDX SBOM per binary from its Go build information (`make sbom`). It fails on a license outside the allowed list; decisions about individual components (dual licenses, unusual license files) are recorded with their reasons in `nomad/tools/thirdparty/overrides.go`, and a decision marked as pending legal review blocks the release. The compliance workflow checks that the committed file is current. `.github/workflows/release.yml` (tag `v*`) builds the archives with `LICENSE`, `NOTICE` and `THIRD_PARTY_NOTICES.md`, the SBOMs and checksums, the container image (files in `/usr/share/doc/norca/`, OCI labels, SBOM and provenance attestations), and a draft release whose notes link the source tag and state the MPL 2.0 terms.

## Trademarks and branding

- The names "Nomad", "Consul", "HashiCorp" and "IBM" and their logos are trademarks of their owners. The name of the project, the name of the binary and the branding do not contain them.
- The README may describe the origin factually: "an independent fork, based on the MPL 2.0 licensed source code of HashiCorp Nomad v1.6.5; not a product of HashiCorp or IBM, and not affiliated with them".
- The HashiCorp logos, the HashiCorp Design System and the HashiCorp icon set are removed from the UI during the Tabler migration.
- We do not use HashiCorp product names (for example "Sentinel", "Enterprise") in feature names, and we do not claim that a feature is "the free version of an Enterprise feature".
- The name of the binary is not `nomad`; at most a compatibility alias remains, which documentedly redirects. The binary is `norca`; a copy or symlink named `nomad` behaves identically (documented in the README), but none is shipped or installed by default.

## Separating plans from implementation

- The design documents are in the `docs/design/` directory, each beginning with a status marker: *Plan*, *In progress* or *Implemented*.
- The feature status table of the README marks only features that actually work as available.
- The data in the mockup is fictional, which the mockup itself indicates.
- Only an existing component gets a directory; there are no empty, pre-licensed directories.

## Contributions

- **DCO** (Developer Certificate of Origin): a `Signed-off-by:` line in every commit (`git commit -s`), checked by CI.
- The license of a contribution is the license of the file concerned (MPL 2.0 for a new file).
- The pull request template asks: does it contain code from a third party (if so, from where and under which license), and is it based on a BSL fix (if so, was it made according to the clean room rule).

## Repository structure

```
norca/
├── LICENSES/                 # MPL-2.0.txt (+ the license texts needed for third-party files, e.g. MIT.txt, BSD-3-Clause.txt)
├── REUSE.toml
├── README.md
├── NOTICE
├── THIRD_PARTY_NOTICES.md    # generated
├── CONTRIBUTING.md
├── docs/
│   ├── licensing.md          # this document
│   └── design/               # design documents, mockup
├── nomad/                    # Nomad v1.6.5 fork, with history (main Go module)
│   ├── api/                  # API client (separate Go module)
│   ├── controller/           # controller SDK and sample operators (separate Go module, uses only api/)
│   ├── natsbridge/           # event stream → NATS bridge (separate Go module, uses only api/ and controller/)
│   ├── secretstore/ubix/     # uBixVault v1.5.0 (BSD-3-Clause), with history; core of the built-in secret store
│   └── pack/                 # Nomad Pack v0.4.2 fork, with history; `norca pack` subcommand
└── .github/
    ├── pull_request_template.md
    └── workflows/            # reuse lint, license check, DCO, generated code, tests, release
```

## Pre-publication checklist

Checked on 2026-10-25 (branch `chore/prepublish-audit`); each item notes how it was verified.

- [x] `nomad/` imported from the v1.6.5 tag, with history; its LICENSE file is MPL 2.0 — the upstream tag commit `a7cfff37` is an ancestor of `main` (`git merge-base --is-ancestor`), the `nomad/` tree of the import merge `df6b4d59` is identical to the tag's tree, `nomad/LICENSE` is byte-identical to the tag's MPL 2.0 LICENSE, and no tracked file carries a `BUSL-1.1` header or Business Source License text (`THIRD_PARTY_NOTICES.md` contains none; the remaining mentions are documentation, the license classifier and the `packs/consul` notices about the official Consul)
- [x] Nomad Pack imported from the v0.4.2 tag (now `nomad/pack/`); the test helper switched to the fork's build, no BSL module in the `go.mod` files — `cd4d2a41` is an ancestor and the tree of the import merge `ecae2433` equals the tag's tree; `internal/cli/testhelper` imports the fork's `command/agent` from the same module; in the nomad, api, controller, natsbridge and tools modules `go list -m all` lists `github.com/hashicorp/nomad[/...]` only as the main module or as a local `replace`, and no `go.sum` has an entry for them
- [x] Binary and CLI named `norca` (CLI text, build outputs, container image, `NORCA_*` CLI environment variables); `nomad` remains only as a documented, not shipped compatibility alias
- [x] Neutral project name; trademark notice in the README and the NOTICE; HashiCorp logos and branding removed outside the UI — name, README and NOTICE notices done; the HashiCorp logos were removed from the OIDC login page, the upstream `nomad/README.md` and the website assets; `nomad/website` keeps only `content/`, the MPL 2.0 documentation text of v1.6.5, as a reference (the website scaffolding and its `LICENSE.md`, which called the design proprietary to HashiCorp, were removed; branch `chore/upstream-branding`); the CLI help, log and error texts and the `job init` templates no longer link `nomadproject.io` or `developer.hashicorp.com`, and the HTTP User-Agent is `Norca/<version> (Nomad API <version>; <go version>)`; the inert upstream infrastructure (`nomad/.github`, `CODEOWNERS`, `.release`, `.copywrite.hcl`) was deleted. The UI branding is handled by the UI rebuild (UI phase 4 of [ui-plan.md](design/ui-plan.md)); compatibility names (`NOMAD_*` variables, `X-Nomad-Token`, module paths, metric names) stay by design
- [x] The original copyright and SPDX headers are intact; `reuse lint` is clean — reuse 5.0.2: compliant with REUSE 3.3, licenses MIT and MPL-2.0 only; for every file of the two upstream tags still present, the copyright and SPDX lines are unchanged, except the 43 BUSL-1.1 files replaced clean room at the same paths, which carry Norca headers
- [x] License check in CI: no prohibited license, no ApexCharts — the commands of every job of `.github/workflows/compliance.yml` were replayed locally: `go-licenses` v1.6.0 passes for the nomad, api, controller, natsbridge and tools modules, the UI license checker passes on the production dependencies, `make check-notices` passes, and no lock or package file names `apexcharts`
- [x] THIRD_PARTY_NOTICES generated, and included in the binaries and the container images (release archives and image via `release.yml`; no decision in `nomad/tools/thirdparty/overrides.go` is pending: the legacy UI exceptions are recorded as `Exception` overrides, and the `-strict` check of the release passes)
- [x] In the release process: source tag link, MPL notice, OCI labels, SBOM
- [x] CONTRIBUTING.md, pull request template, DCO check — the three agree on sign-off, headers, the BSL and clean room rules, third-party code and dependencies; the `dco` job's command finds no unsigned commit on `main` after the repository skeleton; CONTRIBUTING.md lists the checks CI actually runs, including the test workflow (`.github/workflows/test.yml`)
- [x] The design documents have a status marker, the README has a feature status table — every `docs/design/*.md` starts with a status line updated on 2026-10-25, and the README table matches the merged branches
