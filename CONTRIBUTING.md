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

```
// SPDX-FileCopyrightText: 2026 Norca contributors
// SPDX-License-Identifier: MPL-2.0
```

Do not modify or remove the existing copyright and license lines in HashiCorp's original files. A modified HashiCorp file may additionally get a `SPDX-FileCopyrightText: <year> Norca contributors` line.

## Prohibited

- Taking code, diffs or snippets from HashiCorp releases published under BSL 1.1 (Nomad v1.6.6 and later, Consul v1.16.5 and later, and the BSL versions of every other HashiCorp product), or from the non-public code of HashiCorp Enterprise products.
- Adding a BSL version of the `github.com/hashicorp/nomad` main module to any `go.mod` file.
- Adding a dependency whose license is not on the allowed list ([docs/licensing.md](docs/licensing.md)).

## Clean room rule

If your change fixes a bug that HashiCorp has already fixed in a BSL release, write the fix from the public issue and the CHANGELOG description, without looking at the BSL code or diff. Link the public issue in the pull request.

## Third-party code

Code taken from other projects (for example Kubernetes) goes into a separate file with its original license header. If you modify it, mark the modification in the file. State the source and the license in the pull request.

## Checks

Every pull request runs (`.github/workflows/`): `reuse lint`, license checks for the Go modules and the UI packages, the check that `THIRD_PARTY_NOTICES.md` is up to date (`make notices` in `nomad/` regenerates it), the ApexCharts ban, the DCO check, and the check that the generated msgpack codec (`nomad/nomad/structs/structs.generated.go`) is current. The test workflow (`test.yml`) builds the `nomad`, `api`, `controller`, `natsbridge` and `tools` modules for Linux and Windows, runs `go vet ./...` in each of them, and runs the unit tests that need neither root nor Docker, Consul or Vault (the core packages of the `nomad` module listed in the workflow, and the tests of the other modules, which start a dev agent from a `norca` binary built in the job). The integration suites (drivers, client, command, server, e2e) do not run in CI: run the tests of the packages you change, and of their direct dependents, before opening a pull request.
