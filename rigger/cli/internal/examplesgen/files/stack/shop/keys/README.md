# Keys the stack trusts

`registry.pub` is the public key the modules of the registry are signed
with: the `trust` block of `stack.hcl` reads it. It is the public half of
the key pair of `../../../keys/`; copy it here once the pair is made:

```shell
cp ../../keys/registry.pub keys/registry.pub   # from stack/shop
```

A public key is no secret and is committed with the stack. The private key
(`registry.key`) never comes here.
