Norca main module
===

This directory holds the main Go module of [Norca](../README.md). The module
path stays `github.com/hashicorp/nomad` for compatibility with existing imports.

The code started from the source code of HashiCorp Nomad v1.6.5 (MPL 2.0),
imported with its git history; see [docs/licensing.md](../docs/licensing.md).
Norca is not a product of HashiCorp, Inc. or IBM Corp. and is not affiliated
with them. "Nomad", "Consul" and "HashiCorp" are trademarks of their
respective owners.

Layout
---

* the root package and `command/`: the `norca` binary (agent and CLI)
* `api/`: the HTTP API client (a separate Go module)
* `controller/`: the controller SDK of the operator framework (a separate Go module)
* `natsbridge/`: the event stream to NATS bridge (a separate Go module)
* `pack/`: Nomad Pack v0.4.2, built in as `norca pack`
* `tools/`: build tools, such as the third-party notice generator (a separate Go module)
* `ui/`: the web UI
* `website/`: the documentation sources of Nomad v1.6.5, kept as reference;
  they do not describe the Norca extensions

Building
---

```shell
make dev                  # writes bin/norca and copies it to $GOBIN
```

See the [`contributing`](contributing/) directory for more developer documentation.
