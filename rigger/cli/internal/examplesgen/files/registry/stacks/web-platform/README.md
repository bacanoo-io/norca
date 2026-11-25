# web-platform

A published stack of the registry: the modules `webapp` and `cache` of the
same registry, installed together by one apply, the web service wired to
the cache, the password of the cache and the session key generated once.

| File | Holds |
| --- | --- |
| `metadata.hcl` | the `stack` block: name, version, requirements; the tabs of its install form |
| `stack.hcl` | the `rigger` block (name, the registry) and the two modules, by address and version |
| `variables.hcl` | the inputs of the stack |
| `secrets.hcl` | the secrets created once |
| `outputs.hcl` | what the stack tells |
| `rigger.lock.hcl` | written by `rigger stack sync`: the module versions, commits and digests; published with the stack |
| `examples/public.vars.hcl` | values to validate it with |

From the root of the registry, after the modules are published:

```shell
rigger stack sync stacks/web-platform                                      # write rigger.lock.hcl
git add stacks/web-platform/rigger.lock.hcl && git commit -s -m "web-platform: lock"
rigger stack validate stacks/web-platform -f stacks/web-platform/examples/public.vars.hcl
rigger stack publish stacks/web-platform --key ../keys/registry.key
```

`rigger stack publish` validates the stack with its lock file (with the
defaults of its variables), signs it, commits the signature and tags
`web-platform/v1.0.0`. Nothing is pushed.

Applying it by its address, with values of one's own (needs a cluster):

```shell
rigger stack plan  '<% .RegistryPath %>//stacks/web-platform' --version "~> 1.0" -f my.vars.hcl
rigger stack apply '<% .RegistryPath %>//stacks/web-platform' --version "~> 1.0" -f my.vars.hcl
```

A stack of one's own takes it as a part, and its modules read the outputs
of the part as `stack.<part>.<output>`:

```hcl
stack "web" {
  source  = "<% .Owner %>/web-platform"
  version = "~> 1.0"
  values  = { hostname = "shop.example.com" }
}
```
