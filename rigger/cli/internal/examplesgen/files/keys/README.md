# The signing keys of the registry

`rigger key generate --out keys/registry` writes the key pair here:

| File | |
| --- | --- |
| `registry.key` | the private key, encrypted with the password of `RIGGER_KEY_PASSWORD`. **Never commit it**, never copy it into the registry, a stack or a cluster. Keep a backup where your other secrets are; a CI that publishes gets it from its secret store. |
| `registry.pub` | the public key. It goes wherever signatures are checked: `stack/shop/keys/`, `index/keys/<% .Owner %>/`, `cluster/registry.hcl` reads it from here. |

The key is a cosign-format ECDSA P-256 key. Without `RIGGER_KEY_PASSWORD`
(or with it empty) the private key has no password; do that only for a
throwaway key.

```shell
export RIGGER_KEY_PASSWORD='a long password'
rigger key generate --out keys/registry
rigger key verify registry/modules/webapp --key keys/registry.pub   # after rigger module publish

# The signature alone, as module publish writes it, without its commit
# and tag (the module then has a change to commit):
# rigger key sign registry/modules/webapp --key keys/registry.key
```

A lost private key can't sign new versions; a leaked one must be replaced:
make a new pair, publish new versions signed with it, and put the new
public key wherever the old one was.
