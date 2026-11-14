# Security

Norca is alpha software maintained by one person. Reports are read and
answered as time allows; there is no guaranteed response time.

## Reporting a vulnerability

Report it privately, not in a public issue: on the repository page open
**Security**, then **Report a vulnerability** (GitHub private vulnerability
reporting). Only the maintainer sees the report.

Please include:

- the Norca commit or version (`norca version`),
- what an attacker can do and what they need for it (a token, network
  access, a job they may submit),
- the steps or a job file that show it.

The fix and the advisory are published together, with credit to the
reporter unless they ask otherwise.

## Which versions get fixes

There are no releases yet. Fixes go to the `main` branch only.

## Fixes of later Nomad releases

Norca starts from Nomad v1.6.5 (December 2023). Fixes that HashiCorp released
only in later versions, which are under the Business Source License, are not
inherited. Where Norca fixes the same problem, the fix is written
independently from the public issue and changelog description (see
[CONTRIBUTING.md](CONTRIBUTING.md)). Do not assume that a vulnerability fixed
in a later Nomad release is fixed in Norca unless the Norca history or
release notes say so.

## Licensing and legal questions

For a question about the licence or the origin of the code, open an issue.
If it should not be public, use the same private report as above. If you
believe the repository contains code it should not, please write there
first.
