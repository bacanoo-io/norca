# Contributing to uBixVault

Thanks for your interest in uBixVault. Contributions of all kinds are welcome — code,
documentation, design feedback, and issue reports.

> **Project status:** uBixVault is in pre-implementation design. The best contributions
> right now are review and discussion of the design documents in [`docs/`](docs/) — see
> [`docs/DESIGN.md`](docs/DESIGN.md), [`docs/DECISIONS.md`](docs/DECISIONS.md), and
> [`docs/ROADMAP.md`](docs/ROADMAP.md).

## Ground rules

- Be respectful and constructive. This project follows the
  [Code of Conduct](CODE_OF_CONDUCT.md).
- For security issues, **do not** open a public issue — follow the
  [Security Policy](SECURITY.md).

## Proposing changes

1. **Open an issue first** for anything non-trivial, so the approach can be discussed
   before code is written.
2. Fork the repository and create a feature branch.
3. Keep pull requests focused and reasonably small; one logical change per PR.
4. Make sure the build, tests, and linters pass (see below).
5. Open the pull request with a clear description of the change and its motivation.

## Engineering standards

Contributions are expected to follow the standards described in
[`docs/DESIGN.md` §7](docs/DESIGN.md):

- **Idiomatic Go** — `gofmt`/`goimports` clean, `go vet` and `golangci-lint` passing.
- **Tests** — table-driven unit tests; the crypto core and seal/unseal state machine are
  held to a high coverage bar. New behavior comes with tests.
- **Security first** — default-deny, fail-closed, no secrets in logs or errors, no
  hand-rolled cryptography (use vetted standard-library primitives).
- **Errors** wrapped with `%w`; `context.Context` threaded through request paths.

## Commit messages

This project uses [Conventional Commits](https://www.conventionalcommits.org/). Examples:

```
feat(transit): add key rotation endpoint
fix(seal): reject unseal with duplicate key shares
docs(design): clarify barrier key hierarchy
```

## Versioning

uBixVault follows [Semantic Versioning](https://semver.org/).

## License

By contributing, you agree that your contributions will be licensed under the
[BSD 3-Clause License](LICENSE) that covers this project.
