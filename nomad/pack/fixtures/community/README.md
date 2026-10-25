<!--
SPDX-FileCopyrightText: 2026 Norca contributors
SPDX-License-Identifier: MPL-2.0
-->

# Community registry fixtures

`packs/redis` is an unmodified copy of the `redis` pack of the
[Nomad Pack Community Registry](https://github.com/hashicorp/nomad-pack-community-registry/tree/main/packs/redis),
taken at commit `2a43af42b31fa79958e56c92f69d4dac70df0a78` (the last commit that changed the
pack, 2025-12-08). The community registry is licensed under the Mozilla Public License 2.0
(MPL-2.0), the license of this repository; see the repository's `LICENSE` file and the license
headers of the pack files. Copyright of the pack: IBM Corp. and the community registry
contributors.

The pack is used by the compatibility tests in `internal/cli/community_test.go`, which render,
plan and run it against an in-process Norca agent, and push, sign, verify, bundle and import it
through an in-process OCI registry. Do not modify the pack; refresh it from the upstream registry
instead. A network test that fetches the current upstream version is enabled by setting
`NORCA_PACK_COMMUNITY_TESTS=1`.
