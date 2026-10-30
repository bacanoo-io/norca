## Norca {{VERSION}}

<!-- Draft release: add the highlights and the changes of this release here before publishing. -->

### Breaking changes

- **Topology: the zone tier is removed.** Norca keeps one level below the region, the datacenter; model an availability zone as a datacenter. The client `topology` block, `Topology.Zone` and the `Zone` field of service registrations are gone; service discovery `prefer` accepts `none`, `datacenter` and `region`.

### Changes

- **Cluster pack registries** (`packs.norca.io/Registry`: `dir` with the built-in packs, `git`, `oci`, `bundle`): managed on the Packages page of the UI (registries, catalog with README, compatibility and signature) and with `norca pack registry`. Install and Upgrade use a form built from the pack's variables; the leader renders the pack and plans or applies it as a server-rendered release, optionally atomic. Syncs fetch into a staging directory and keep the last good packs when a sync fails.
- **System bus**: the servers embed NATS with JetStream (`server { bus {} }`, ports `bus` 4649 and `bus_cluster` 4650; the servers of a region form one cluster; nodes log in with their node secret, other connections with an ACL token with a `bus` rule; `norca operator bus status`). The leader publishes the event stream of the region into `NORCA_EVENTS_<region>` (subjects `nomad.<region>.<topic>`) and syncs the cluster registries on pack push notices (`POST /v1/pack-registries/notify-push`).
- **Backups to a directory**: `destination "<name>" { type = "directory" path = "..." }`, with the encryption, verification, retention and pinning of the S3 destinations.
- **Windows build**: `windows.bat` builds and packages the Windows release locally.
- **CNI**: the client uses the CNI library `containernetworking/cni` v1.3.1 (was v1.1.2), `containernetworking/plugins` v1.9.1 (was v1.2.0) and `containerd/go-cni` v1.1.14 (was v1.1.9). Network configurations (`.conflist`, `.conf`, `.json` in `cni_config_dir`) may now declare `cniVersion` up to **1.1.0**; the built-in `bridge` network mode still writes a 0.4.0 configuration and works with every 1.x release of the CNI reference plugins. See [CNI compatibility](#cni-compatibility).

### Fixes

- **Pack registry calls on followers** failed with `missing registry` (and the other pack RPCs lost their fields) in release builds: the generated msgpack codec of the embedded request metadata encoded only that part when a follower forwarded the call to the leader.
- **git pack registries** no longer panic on sync (nil logger).
- **Release values** with `optional()` object attributes render correctly.
- **cgroups**: the cpuset manager is disabled when the parent cgroup exists but is not writable, instead of leaving every task pending.

### Source code and license

The source code of this release is available under the Mozilla Public License 2.0 at {{TAG_URL}}.

Norca is an independent fork of HashiCorp Nomad v1.6.5 (MPL 2.0); it is not a product of HashiCorp or IBM and is not affiliated with them. Every archive contains `LICENSE`, `NOTICE` and `THIRD_PARTY_NOTICES.md` (the licenses and notices of the third-party components); the container image carries the same files in `/usr/share/doc/norca/`.

### Downloads

| Platform | Archive |
| --- | --- |
| Linux x86-64 | `norca_{{VERSION}}_linux_amd64.tar.gz` |
| Linux ARM64 | `norca_{{VERSION}}_linux_arm64.tar.gz` |
| macOS Apple silicon | `norca_{{VERSION}}_darwin_arm64.tar.gz` |
| Windows x86-64 | `norca_{{VERSION}}_windows_amd64.zip` |

Each archive contains `norca`. Checksums: `norca_{{VERSION}}_SHA256SUMS`. Software bill of materials (CycloneDX 1.5 JSON): `norca_{{VERSION}}_<os>_<arch>.cdx.json`.

### CNI compatibility

| | |
| --- | --- |
| CNI specification versions | 0.1.0, 0.2.0, 0.3.0, 0.3.1, 0.4.0, 1.0.0, 1.1.0 |
| Configuration of the `bridge` network mode | `cniVersion` 0.4.0 (plugins `loopback`, `bridge`, `firewall`, `portmap`) |
| Recommended CNI reference plugins | v1.9.1 (any 1.x release works) |

The CNI reference plugins are not part of the archives: install them on every Linux client in its `cni_path` (default `/opt/cni/bin`) for the `bridge` network mode, native mTLS sidecars and network rules.

| Platform | Download |
| --- | --- |
| Linux x86-64 | https://github.com/containernetworking/plugins/releases/download/v1.9.1/cni-plugins-linux-amd64-v1.9.1.tgz ([SHA-256](https://github.com/containernetworking/plugins/releases/download/v1.9.1/cni-plugins-linux-amd64-v1.9.1.tgz.sha256)) |
| Linux ARM64 | https://github.com/containernetworking/plugins/releases/download/v1.9.1/cni-plugins-linux-arm64-v1.9.1.tgz ([SHA-256](https://github.com/containernetworking/plugins/releases/download/v1.9.1/cni-plugins-linux-arm64-v1.9.1.tgz.sha256)) |
| All releases | https://github.com/containernetworking/plugins/releases |
| CNI specification | https://github.com/containernetworking/cni/blob/v1.3.1/SPEC.md |

### Container image

`{{IMAGE}}:{{VERSION}}` for linux/amd64 and linux/arm64 (digest `{{DIGEST}}`), with an SPDX SBOM and provenance attestation. Its `org.opencontainers.image.source` and `org.opencontainers.image.revision` labels name the repository and the commit `{{REVISION}}`.
