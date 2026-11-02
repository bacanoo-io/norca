# exec2 task driver

Runs tasks without a container image, isolated with Landlock, cgroups v2 and
per-task users.

## Origin

This directory is a fork of the HashiCorp `nomad-driver-exec2` plugin:

- Source: https://github.com/hashicorp/nomad-driver-exec2
- Version: v0.1.1, commit `806e1aa12b9549addd9babbf17400e953acd3c3e`
- License: Mozilla Public License 2.0 (MPL-2.0); the copied files keep their
  original copyright and SPDX headers.

The first commit of this directory holds the upstream files unchanged:
`plugin/`, `pkg/`, `version/`, `e2e/`, `CHANGELOG.md`, and the upstream
`README.md` as `UPSTREAM_README.md`. The external-plugin entry point
(`main.go`), the build and release files and `go.mod` were not copied: in
Norca the driver is built into the agent.

Upstream builds against Nomad v1.10.2. Packages that Norca does not have
are written for Norca from the requirements of the driver, not copied.
