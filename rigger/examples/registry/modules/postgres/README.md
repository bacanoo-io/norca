# postgres

A single PostgreSQL server with what usually comes with it.

| File | Resources |
| --- | --- |
| `postgres.norca.hcl` | the server job (host volume, static port, readiness check) |
| `databases.norca.hcl` | a hook job after every install and upgrade that creates the application databases, owners and extensions; safe to run again |
| `backup.norca.hcl` | with `backup.enabled`: a periodic dump job |
| `backup.acl.hcl` | with `backup.enabled`: an ACL policy bound to the dump job |
| `info.var.hcl` | a Norca Variable with the names of the release |
| `metadata.hcl`, `variables.hcl`, `locals.hcl`, `outputs.hcl` | the module, its inputs and rules, shared values, outputs |
| `files/` | the SQL of one database (`database.sql.tftpl`, the template syntax of HCL) and the dump script (`backup.sh`, a plain file) |

- **Secrets** are in the secret store: `<secret_prefix>/superuser` (item
  `password`), `<secret_prefix>/apps/<database>` (items `user`, `password`),
  `backup.s3_secret` (items `access_key`, `secret_key`).
- **Dumps need an S3 store.** `metadata.hcl` requires a release of
  `example/rustfs` when `backup.enabled` is set; a stack without one is
  refused at `plan`.
- **Output `databases`:** per database the service, the name and the secret
  of its owner, in the shape an application module takes as a value.

```shell
rigger install . --name postgres --values examples/two-databases.vars.hcl
```

This module has no `icon.png`, so it is shown with the default icon.

License: the module is MPL-2.0. PostgreSQL is under the PostgreSQL License;
the official image is referenced, not shipped.
