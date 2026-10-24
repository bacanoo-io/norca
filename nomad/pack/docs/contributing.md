# Contributing to Nomad Pack

Contributions to Nomad Pack are welcome.

To add packs, please contribute to the [Nomad Pack Community Registry](https://github.com/hashicorp/nomad-pack-community-registry).

## Development dependencies

- Golang
- Git

Nomad Pack is part of the Nomad module (`nomad/go.mod`); its code lives in `nomad/pack/` and the
commands are registered in the Nomad CLI as `nomad pack ...`.

## Building and Running Locally

Build the `nomad` binary from the `nomad/` directory:

```
go build -o ./bin/nomad .
```

Run your code:

```
./bin/nomad pack -h
```

Run the Nomad Pack tests (the tests that start a Nomad agent need the `nomad` binary on your PATH):

```
go test ./pack/...
```

The mTLS test certificates in `pack/fixtures/mtls` are generated with the fork's `helper/tlsutil`;
regenerate them with `go run generate.go` in that directory.
