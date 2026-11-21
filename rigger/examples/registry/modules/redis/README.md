# redis

The built-in pack `redis` 0.2.1 of norca, converted to a module on paper in
phase 0 of the design (`design/rigger-design.md`, section 14). It follows the
pack so that it renders the same job; the faults of the pack are kept and
named here, to be fixed in a version of their own once the conversion is
proven.

| File | From the pack |
| --- | --- |
| `metadata.hcl` | `metadata.hcl` (`app` and `pack` are one `module` block) |
| `variables.hcl` | `variables.hcl` without `namespace` and `region`, which are the release's |
| `redis.norca.hcl` | `templates/redis.nomad.tpl` |
| `auth.var.hcl` | `templates/redis.var.hcl.tpl` and the `secret` block of `redis_password` (the file is named after its resource) |
| `outputs.hcl` | new: the pack had none |

Faults of the pack, kept:

- `mode` changes nothing but an item of the Variable: every topology runs as
  standalone.
- `service_tags` and `extra_env` are not used by the job, and the job
  registers no service.
- The password goes into the Variable, but the job does not read it: Redis
  runs without a password.
- The path of the Variable is `nomad/jobs/redis` whatever `job_name` is.

`rigger module validate` should find the first two by itself: a variable no
expression of the module reads is a warning (design, section 14).
