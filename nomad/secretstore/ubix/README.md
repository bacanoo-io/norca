# uBixVault

**Open, self-hosted secrets & encryption management — a from-scratch secrets
manager in Go.** uBixVault stores and generates secrets behind an encrypted,
sealed barrier: an encryption barrier, Shamir seal/unseal, token auth with ACL
policies, versioned KV secrets, encryption-as-a-service, and dynamic database
credentials — over a Vault-API-compatible HTTP interface.

![API: stable (SemVer)](https://img.shields.io/badge/API-stable%20(SemVer)-brightgreen)
![security: not yet independently audited](https://img.shields.io/badge/security-not%20yet%20independently%20audited-orange)

> **`1.0` — API-stable and feature-complete; not yet independently audited.**
> uBixVault versions the *interface* with [SemVer](https://semver.org/): `1.0`
> means the HTTP API, CLI, storage format, and chart values are stable and the
> feature set is complete. It says nothing about a security audit — that is a
> separate axis (see [`docs/VERSIONING.md`](docs/VERSIONING.md)).
>
> **Security assurance:** uBixVault has **not** yet undergone an independent
> third-party security audit. Its cryptography and trust path are standard-library
> Go, written in-house, fuzzed, and property-tested — but that is not a substitute
> for outside review, which is an active, open milestone
> ([`docs/ROADMAP.md`](docs/ROADMAP.md)). Until it lands, weigh that before
> placing high-blast-radius secrets here: prefer running it alongside an audited
> store such as [HashiCorp Vault](https://www.vaultproject.io/) or
> [OpenBao](https://openbao.org/), start with low-criticality secrets, and make
> the risk visible to whoever owns security. See [`SECURITY.md`](SECURITY.md) and
> why uBixVault exists alongside them: [`docs/POSITIONING.md`](docs/POSITIONING.md).

## Capabilities

- **Encryption barrier** — all data at rest is AES-256-GCM encrypted (storage never sees plaintext); the storage path is bound into the ciphertext so blobs can't be relocated.
- **Seal / unseal** — the vault boots *sealed*; the master key is reconstructed from **Shamir k-of-n** key shares (implemented in-house, constant-time GF(2⁸)) before any secret is readable.
- **Token auth + ACL policies** — default-deny, path-based capabilities; scoped, non-root tokens. A child token only gets policies its creator holds (unless the creator has `sudo`) and cannot outlive it; renewal is capped by `-max-token-ttl` (ADR D-022).
- **KV v2** — versioned secrets with soft-delete / undelete / destroy and max-versions.
- **Transit** — encryption-as-a-service: apps encrypt/decrypt over the API; keys never leave the vault, and rotate without breaking old ciphertext.
- **Dynamic database credentials** — short-lived MariaDB users generated on demand and auto-revoked on lease expiry.
- **Audit logging** — fail-closed; records who accessed what, with the client token HMAC'd (never logged in the clear).
- **High availability** — active/standby replicas over MySQL/MariaDB storage (`-ha`, chart `ha.enabled`): one active replica holds a fenced lock in the database, standbys forward to it, and a node drain or rolling upgrade hands over in about half a second (no failed requests in the kind failover test). See [`docs/DEPLOYMENT.md`](docs/DEPLOYMENT.md#high-availability-activestandby).
- **Web console** — `/ui/`, served from the binary with no external assets: sign in with userpass, LDAP or a token; initialize and unseal; KV v2 with version history; ACL policies; token creation; PKI. Beta: Transit and auth-method management are not in it yet.
- **Works with Vault's own tooling** — the `vault` CLI, External Secrets Operator and other clients built on HashiCorp's Go client (`github.com/hashicorp/vault/api`) work unmodified: `PUT`/`POST` on every write, `?list=true`, durations in seconds, Vault's request and response shapes (checked end to end).

## Quickstart

```sh
# Build and run the server (dev mode: plain HTTP, audit log enabled)
go build -o bin/ubixvault ./cmd/ubixvault
./bin/ubixvault server -data ./data -audit-log ./audit.log
```

In another terminal (`export VAULT=http://127.0.0.1:8200`):

```sh
# 1. Initialize (3 unseal shares, threshold 2). Save the keys and root token!
curl -s -X POST $VAULT/v1/sys/init -d '{"secret_shares":3,"secret_threshold":2}'
# -> {"keys":["<k0>","<k1>","<k2>"], "root_token":"uv...."}

# 2. Unseal with any 2 shares.
curl -s -X POST $VAULT/v1/sys/unseal -d '{"key":"<k0>"}'
curl -s -X POST $VAULT/v1/sys/unseal -d '{"key":"<k1>"}'   # -> "sealed":false

export T="X-Vault-Token: <root_token>"

# 3. KV v2: write and read a versioned secret.
curl -s -H "$T" -X POST $VAULT/v1/secret/data/app -d '{"data":{"api_key":"sk-123"}}'
curl -s -H "$T" $VAULT/v1/secret/data/app

# 4. Transit: encryption-as-a-service (keys never leave the vault).
curl -s -H "$T" -X POST $VAULT/v1/transit/keys/orders
curl -s -H "$T" -X POST $VAULT/v1/transit/encrypt/orders \
  -d "{\"plaintext\":\"$(printf 'card-4242' | base64)\"}"     # -> {"ciphertext":"ubix:v1:..."}

# 5. Scoped access: a read-only token that can read app secrets but not write them.
curl -s -H "$T" -X PUT $VAULT/v1/sys/policies/acl/app-ro \
  -d '{"path":{"secret/data/app":{"capabilities":["read"]}}}'
curl -s -H "$T" -X POST $VAULT/v1/auth/token/create -d '{"policies":["app-ro"]}'
# -> {"auth":{"client_token":"uv...."}}   # this token gets 200 on read, 403 on write

# 6. Dynamic database credentials (requires a reachable MariaDB).
curl -s -H "$T" -X POST $VAULT/v1/database/config \
  -d '{"connection_url":"root:pw@tcp(127.0.0.1:3306)/db"}'
curl -s -H "$T" -X POST $VAULT/v1/database/roles/readonly \
  -d '{"creation_statements":["CREATE USER '\''{{username}}'\''@'\''%'\'' IDENTIFIED BY '\''{{password}}'\''","GRANT SELECT ON *.* TO '\''{{username}}'\''@'\''%'\''"],"default_ttl":"1h"}'
curl -s -H "$T" $VAULT/v1/database/creds/readonly
# -> {"username":"uv_readonly_...","password":"...","lease_id":"...","lease_duration":3600}
```

TLS is enabled by passing `-tls-cert` and `-tls-key`; without them the server
logs a prominent no-TLS warning (dev only).

### Operator CLI

The lifecycle steps (1–2 above) can also be driven with the built-in client
instead of curl (`-address` defaults to `$UBIXVAULT_ADDR`):

```sh
ubixvault operator init -shares 3 -threshold 2   # prints the keys + root token
ubixvault operator unseal <key>                  # repeat until unsealed
ubixvault operator seal-status
ubixvault operator seal -token <root_token>
```

## Architecture

uBixVault is layered, with everything resting on the encryption barrier:

```
HTTP API  ·  auth + ACL middleware  ·  audit
Secrets engines: KV v2 · Transit · Dynamic Database
Tokens · Policies · Core (init / seal / unseal)
BARRIER (AES-256-GCM at rest) · Shamir seal/unseal
Storage backend (file)
```

Each subsystem is a small Go package behind an interface (`storage`, `barrier`,
`shamir`, `core`, `token`, `policy`, `kv`, `transit`, `database`, `audit`, `api`),
which is what lets engines and backends be swapped and tested independently.

## Design & documentation

The architecture and reasoning are first-class parts of the project:

- **[docs/DESIGN.md](docs/DESIGN.md)** — architecture, subsystems, threat model, engineering standards, operational considerations.
- **[docs/DECISIONS.md](docs/DECISIONS.md)** — architecture decision records (Go vs. Rust vs. C++, in-house Shamir, first dependency, …).
- **[docs/ROADMAP.md](docs/ROADMAP.md)** — the v1.0 core and possible extensions.
- **[docs/POSITIONING.md](docs/POSITIONING.md)** — prior art and why this was built.
- **[docs/DEPLOYMENT.md](docs/DEPLOYMENT.md)** — running it: TLS, unsealing, systemd/Docker, backups, upgrades.

## Development

```sh
make test    # go test -race with coverage
make lint    # golangci-lint (also gofmt, vet)
make build   # build the binary
make help    # all targets
```

CI runs build/test (`-race`), lint (incl. gosec), `govulncheck`, and a MariaDB
integration job on every change.

## Why it exists

uBixVault is a lightweight, from-scratch secrets manager built to support
[uBixCore](https://github.com/cwolsen7905/uBixCore), but it is
**framework-agnostic** — any application can use it over its HTTP API. Full
rationale in [`docs/POSITIONING.md`](docs/POSITIONING.md).

## License

BSD 3-Clause. See [LICENSE](LICENSE).
