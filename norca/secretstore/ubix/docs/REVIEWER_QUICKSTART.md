# Reviewer quickstart

A one-page path to a running uBixVault and its tests, for security reviewers.
Pairs with the scope in [`docs/SECURITY_REVIEW_BRIEF.md`](SECURITY_REVIEW_BRIEF.md)
and the threat model in [`docs/DESIGN.md`](DESIGN.md) §5.

## Build

```sh
make build                                   # -> bin/ubixvault (Go 1.25, static, CGO-free)
# equivalently: CGO_ENABLED=0 go build -trimpath -o bin/ubixvault ./cmd/ubixvault
```

## Happy path (file storage, loopback plaintext)

The server binds `127.0.0.1:8200` by default; on loopback it serves plain HTTP
(TLS is required only on non-loopback addresses).

```sh
./bin/ubixvault server -data ./data -audit-log ./audit.log &
V=http://127.0.0.1:8200

# Initialize: 3 unseal shares, threshold 2. Save the keys + root token it returns.
curl -s -X POST $V/v1/sys/init -d '{"secret_shares":3,"secret_threshold":2}'

# Unseal with two shares (k0, k1 from the init response).
curl -s -X POST $V/v1/sys/unseal -d '{"key":"<k0>"}'
curl -s -X POST $V/v1/sys/unseal -d '{"key":"<k1>"}'
curl -s $V/v1/sys/seal-status                       # sealed:false

# Write + read a secret (T = the root token).
T=<root_token>
curl -s -H "X-Vault-Token: $T" -X POST $V/v1/secret/data/app -d '{"data":{"api_key":"sk-123"}}'
curl -s -H "X-Vault-Token: $T" $V/v1/secret/data/app
```

Routes are registered in `internal/api/sys.go` (Vault-compatible `/v1/*`). There
is a read-only web console at `/ui/`.

## MySQL/MariaDB storage backend

```sh
docker compose up -d mariadb                 # compose.yaml; DB "ubixvault"
# The vault creates its own tables.
./bin/ubixvault server \
  -storage mysql \
  -storage-mysql-dsn 'root:root@tcp(127.0.0.1:3306)/ubixvault' &
# then init/unseal as above. Inspect the DB: every value is barrier ciphertext.
```

## External (KMS/HSM) seal — auto-unseal path

```sh
cat > /tmp/kmsseal.sh <<'EOF'
#!/bin/sh
case "$1" in wrap) openssl base64 -A ;; unwrap) openssl base64 -A -d ;; esac
EOF
chmod +x /tmp/kmsseal.sh
./bin/ubixvault server -data ./data2 -seal-external-command /tmp/kmsseal.sh &
# init once (wraps the master key via the command), then restart the server:
# it logs "auto-unsealed" and comes back unsealed (the command unwrapped the key).
```

## Tests, fuzzing, and scanners

```sh
make test                                       # unit + property tests (race detector + coverage) + fuzz seed corpora
make integration                                # against the compose MariaDB (build tag + DSN env, as CI runs it)
make fuzz                                        # fuzz the policy parser 30s (edit the target for others)
make lint                                        # golangci-lint (.golangci.yml)
make vuln                                        # govulncheck ./...
# make help lists every target.

# Other fuzz targets (run directly): internal/jwtauth (FuzzSplitJWT, FuzzParseJWKS),
# internal/transit (FuzzParseCiphertext), internal/snapshot (FuzzRestore),
# internal/shamir (FuzzSplitCombine), e.g.:
go test -run=x -fuzz=FuzzSplitJWT -fuzztime=60s ./internal/jwtauth/
```

## Where the crown jewels live

| Area | Path |
|------|------|
| Encryption barrier (AEAD, AAD, rekey) | `internal/barrier` |
| Shamir (GF(2⁸)) + seal state machine | `internal/shamir`, `internal/core` |
| Seals (KEK / transit / external) | `internal/seal` |
| ACL + in-house HCL parser | `internal/policy` |
| Tokens & leases | `internal/token` |
| Auth methods (JWT/OIDC, AppRole, userpass, k8s, TLS-cert, LDAP/AD) | `internal/jwtauth`, `internal/approle`, `internal/userpass`, `internal/kubeauth`, `internal/certauth`, `internal/ldapauth` |
| Identity (entities, aliases, groups, templating) | `internal/identity` |
| Audit (fail-closed, HMAC) | `internal/audit` |
| Storage (file, MySQL) | `internal/storage` |
