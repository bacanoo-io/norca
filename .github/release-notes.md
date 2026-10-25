## Norca {{VERSION}}

<!-- Draft release: add the highlights and the changes of this release here before publishing. -->

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

Each archive contains `norca` and `norca-natsbridge`. Checksums: `norca_{{VERSION}}_SHA256SUMS`. Software bills of materials (CycloneDX 1.5 JSON) for every binary: `<binary>_{{VERSION}}_<os>_<arch>.cdx.json`.

### Container image

`{{IMAGE}}:{{VERSION}}` for linux/amd64 and linux/arm64 (digest `{{DIGEST}}`), with an SPDX SBOM and provenance attestation. Its `org.opencontainers.image.source` and `org.opencontainers.image.revision` labels name the repository and the commit `{{REVISION}}`.
