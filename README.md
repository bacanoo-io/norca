# Norca

An independent, open source workload scheduler and orchestrator built on the **HashiCorp Nomad v1.6.5** source code (MPL 2.0), with its own extensions: a built-in autoscaler, namespace quotas, admission policies, backup and restore, service discovery without Consul, package management and a new web UI.

**This project is not a product of HashiCorp or IBM and is not affiliated with them.** "Nomad", "Consul" and "HashiCorp" are trademarks of their respective owners; they appear here only to describe the origin of the code.

## Origin

| Component | Based on | Commit | License |
| --- | --- | --- | --- |
| `nomad/` | HashiCorp Nomad v1.6.5 | `a7cfff372cd52b02232a02bd3ffb5e5d35f88a0f` | MPL 2.0 |
| `nomad/pack/` | HashiCorp Nomad Pack v0.4.2 | `cd4d2a41c22adba1350b8d5da63252f2dde33262` | MPL 2.0 |
| built-in autoscaler (`nomad/nomad/autoscaler/`, planned) | HashiCorp Nomad Autoscaler v0.6.0 | `a24707db8f2abd3ee74ce0cb5cc0d2b699d2cace` | MPL 2.0 |

v1.6.5 is the last Nomad release published under MPL 2.0. `nomad/` and Nomad Pack were imported with their full git history, so the starting commits can be verified; Nomad Pack was later moved into `nomad/pack/` (part of the Nomad Go module, run as `nomad pack`), and `git log --follow` reaches back to its original history. This project contains no code from HashiCorp releases published under BSL 1.1. Details: [docs/licensing.md](docs/licensing.md).

## Feature status

| Feature | Status | Design |
| --- | --- | --- |
| Nomad v1.6.5 core features | Available | – |
| Security dependency updates (phase 1) | Planned | [development plan](docs/design/nomad-fork-development-plan.md) |
| Built-in autoscaler | Planned | [development plan](docs/design/nomad-fork-development-plan.md) |
| Namespace quotas, admission policies, further Kubernetes-inspired features | Planned | [detailed specification](docs/design/kubernetes-features-spec.md) |
| Backup and restore | Planned | [plan](docs/design/backup-restore-plan.md) |
| Service discovery without Consul | Planned | [plan](docs/design/consul-independence-plan.md) |
| Package management (based on Nomad Pack) | Planned | [plan](docs/design/package-management-plan.md) |
| New web UI | Planned | [plan](docs/design/ui-plan.md) |

Only features marked "Available" can be used; everything else is in the design phase.

## License

The code and documentation of this project are licensed under the **MPL 2.0** ([LICENSES/MPL-2.0.txt](LICENSES/MPL-2.0.txt)). Files taken from third parties keep their original license; these are marked by their file headers and by [REUSE.toml](REUSE.toml). The licenses of dependencies will be listed in `THIRD_PARTY_NOTICES.md`, generated as part of the release process.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). Every commit requires a DCO sign-off (`git commit -s`).
