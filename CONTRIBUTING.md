# Contributing

Thank you for contributing. To keep the project legally clean, the following rules are mandatory.

## DCO sign-off

Every commit must carry a sign-off according to the Developer Certificate of Origin (https://developercertificate.org):

```
git commit -s -m "message"
```

With it you certify that you have the right to submit the contribution under the project's license. CI rejects commits without a sign-off.

## License

Your contribution is licensed under the license of the file it touches. New files are licensed under MPL 2.0, with this header:

<!-- REUSE-IgnoreStart -->

```
// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0
```

Do not modify or remove the existing copyright and license lines in HashiCorp's original files. When you modify such a file for the first time, add this line right after the HashiCorp (or IBM) copyright line, in the comment style of the file:

```
// Copyright (c) HashiCorp, Inc.
// Modifications Copyright (c) 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0
```

Files that already carry a `SPDX-FileCopyrightText: <year> Norca contributors` line need no further line.

<!-- REUSE-IgnoreEnd -->

## Prohibited

- Taking code, diffs or snippets from HashiCorp releases published under BSL 1.1 (Nomad v1.6.6 and later, Consul v1.16.5 and later, and the BSL versions of every other HashiCorp product), or from the non-public code of HashiCorp Enterprise products.
- Adding the upstream `github.com/hashicorp/nomad` module (any version) or an upstream version of `github.com/hashicorp/nomad/api` to any `go.mod` file; the latter is replaced by the stand-in in `norca/compat/upstreamapi`.
- Adding a dependency whose license is not on the allowed list ([licence and attribution](https://github.com/bacanoo-io/norca-doc/blob/main/content/docs/introduction/license.md)).

## Independent fix from the public issue

If your change fixes a bug that HashiCorp has already fixed in a BSL release, write the fix from the public issue and the CHANGELOG description, without looking at the BSL code or diff. Link the public issue in the pull request.

End the message of such a commit with the trailer `Source-Rule: independent-fix`. The other values are `independent-development` (a feature whose Nomad counterpart was Enterprise only, built from Norca's own design), `reimplementation` (one of the 45 left-out files) and `isolated-environment`. The similarity audit ([CLEAN_ROOM.md](CLEAN_ROOM.md)) compares the lines of these commits with the lines HashiCorp added to the same files.

## Third-party code

Code taken from other projects (for example Kubernetes) goes into a separate file with its original license header. If you modify it, mark the modification in the file. State the source and the license in the pull request.

## Checks

Every pull request runs (`.github/workflows/`): `reuse lint`, license checks for the Go modules and the UI packages, the check that `THIRD_PARTY_NOTICES.md` is up to date (`make notices` in `norca/` regenerates it), the ApexCharts ban, the DCO check, and the check that the generated msgpack codec (`norca/norca/structs/structs.generated.go`) is current. The test workflow (`test.yml`) builds the `norca`, `api`, `controller` and `tools` modules for Linux and Windows, runs `go vet ./...` in each of them, and runs the unit tests that need neither root nor Docker, Consul or Vault (the core packages of the `norca` module listed in the workflow, and the tests of the other modules, which start a dev agent from a `norca` binary built in the job). The integration suites (drivers, client, command, server, e2e) do not run in CI: run the tests of the packages you change, and of their direct dependents, before opening a pull request.

The tests that need root, such as some `api` tests, run in a Linux container as root. A client running as root mounts a tmpfs on the secrets dir of every task, so give the container `--cap-add SYS_ADMIN` (`--privileged` is not needed); without it the tests whose task must run are skipped (`RequireMount` in `api`, `ci.SkipWithoutMount` in the `norca` module, for example in `command/agent`). From the `norca` directory: `docker run --rm --cap-add SYS_ADMIN -v "$PWD:/src" -w /src golang:1.27-bookworm bash -c 'go build -o /usr/local/bin/norca . && go -C api test -count=1 ./...'`. The `golang` image has no `consul` binary and no syslog socket: `TestConfig_DevModeFlag` then expects `-dev-connect` to be refused, and `TestSyslogFilter` is skipped.

The tests that start a client and wait for it to run an allocation (for example `TestClientFS_*` in `norca/norca`) wait a fixed time, a few seconds. On a machine with few cores for the tests run in parallel they fail with "Alloc on node ... not finished", "timeout" or "No path to node" and nothing wrong in the code. Set `CI=1` in the environment of the test run there: the waits of `testutil` are then four times as long (`testutil.TestMultiplier`); GitHub Actions sets `CI` by itself.

The cgroup tests of the client (`norca/client/lib/cgutil`) write to the cgroup hierarchy and depend on its version. In a container the hierarchy is read-only without `--privileged`, and the tests that make cgroups are skipped; with `--privileged` those of the version of the Docker host run (cgroups v1 on Docker Desktop with WSL 2, where the cgroups v2 tests are skipped). To run the cgroups v2 tests from such a machine, build the test binary (`GOOS=linux CGO_ENABLED=0 go test -c -o cgutil.test ./client/lib/cgutil`) and run it as root on a Linux machine with cgroups v2, for example a VM of the lab (`./cgutil.test -test.v`).
