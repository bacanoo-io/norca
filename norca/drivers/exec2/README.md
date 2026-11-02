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

## Changes in Norca

- Built into the agent on Linux (`helper/pluginutils/catalog/register_linux.go`).
- Imports use the Norca module; the copied files that changed carry a
  "Modifications Copyright" line.
- Written for Norca: `plugins/drivers/fsisolation` (the `unveil` mode),
  `client/lib/cpustats` with `AgentConfig.Compute()` (the CPU fingerprint of
  the node, internal drivers only), `helper/users/dynamic` and the
  `DynamicWorkloadUsers` capability with the client's `dynamic_user` task
  hook, `subproc.ExitNotRunnable`.
- Limits: `memory.oom.group` is always set; `memory.swap.max` is 0 unless the
  plugin option `allow_swap` is true; `pids_limit` (plugin and task) sets
  `pids.max`, a task may only lower the plugin limit.
- The task environment variables are `NORCA_*` (the driver unveils
  `NORCA_TASK_DIR`, `NORCA_ALLOC_DIR` and `NORCA_SECRETS_DIR`); the e2e tests
  call the `norca` CLI and configure the driver as `plugin "exec2"`.
- The tests use the driver harness cgroup (`testing.slice`) instead of the
  upstream `cgroupslib`, and the Norca dynamic user names (`norca:<uid>`).

## Dynamic users

A task without `user` runs as an anonymous user: the client hands out an ID
from the range `client { dynamic_user_min, dynamic_user_max }` (default
80000-89999) as UID and GID, passes it to the driver as `norca:<id>`, keeps it
across client restarts and frees it when the task will not run again. Files
the client writes for the task (secrets, identity token, mTLS certificates,
the task API socket) belong to that user.
