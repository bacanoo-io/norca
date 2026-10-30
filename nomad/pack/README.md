
# Nomad Pack

Nomad Pack is a templating and packaging tool used with [Nomad](https://www.nomadproject.io).

In this repository Nomad Pack is part of the Norca fork of Nomad: the code under `nomad/pack/`
originates from Nomad Pack v0.4.2 and is built into the main `norca` binary, where it runs as the
`norca pack` subcommand. There is no separate `nomad-pack` binary. Packs are rendered on the client.

Nomad Pack is used to:

- Easily deploy popular applications to Nomad
- Re-use common patterns across internal applications
- Find and share job specifications with the Nomad community

## Installation

Nomad Pack is built into the `norca` binary of this repository; build it from the `nomad/`
directory (for example with `go build -o norca .`) and make sure the binary is on your PATH. All
pack commands are available below `norca pack`:

```shell
norca pack --help
```

## Usage

### Dependencies

Nomad Pack users must have Norca running and accessible at the address defined in the `NORCA_ADDR` (or `NOMAD_ADDR`)
environment variable.

If Nomad ACLs are enabled, a token with proper permissions must be defined in the `NORCA_TOKEN` (or `NOMAD_TOKEN`)
environment variable.

### Basic Use

First, run the `list` command to see which packs are available to deploy.

```shell
norca pack list
```

`norca pack` has no built-in registry: add one with the `registry add` command, for example the
[Norca pack registry](https://github.com/bacanoo-io/norca-registry) (NATS, the cross-region mTLS gateway, RustFS as the
backup store, Consul, the LoadBalancer operator). Packs of the registry named `default` can be used
without `--registry`:

```shell
norca pack registry add norca https://github.com/bacanoo-io/norca-registry --ref=v0.1.0
norca pack run nats --registry=norca

norca pack registry add default https://github.com/bacanoo-io/norca-registry --ref=v0.1.0
norca pack run nats
```

Pack commands without `--ref` use the packs at the ref the registry was last added or updated
with (here `v0.1.0`), and without `--name` the deployment (and the release) is named after the
pack; `--ref` selects another cached ref, and the deployment name is then `pack@ref`.

To deploy one of these packs, use the `run` command. This deploys each jobs defined in the pack to Nomad.
To deploy the `hello_world` pack (from a registry named `default`), you would run the following command:

```shell
norca pack run hello_world
```

Each pack defines a set of variables that can be provided by the user. To get information on the pack
and to see which variables can be passed in, run the `info` command.

```shell
norca pack info hello_world
```

Values for these variables are provided using the `--var` flag.

```shell
norca pack run hello_world --var message=hola
```

Values can also be provided by passing in a variables file. See the variables section of the
[Detailed usage guide](docs/detailed-usage.md) for details.

```shell
tee -a ./my-variables.hcl << END
message="bonjour"
END

norca pack run hello_world -f ./my-variables.hcl
```

If you want to remove all of the resources deployed by a pack, run the `destroy` command with the
pack name.

```shell
norca pack destroy hello_world
```

### Adding Non-Default Pack Registries

No registry is added automatically; `--registry` defaults to the registry named `default`.

You can add further registries by using the `registry add` command. For instance, if you wanted
to add [an example Gitlab registry](https://gitlab.com/mikenomitch/pack-registry),
you would run the following command to download the registry.

```shell
norca pack registry add example gitlab.com/mikenomitch/pack-registry
```

To view the packs you can now deploy, run the `registry list` command.

```shell
norca pack registry list
```

Packs from this registry can now be deployed using the `run` command.

### OCI registries, signing and offline installation

Packs can also be published to and installed from OCI registries (for example an internal Harbor
or Zot registry), signed with cosign compatible key pairs and verified against a per-registry trust
policy, moved into air-gapped environments as offline bundles, and pinned with a `pack.lock` file:

```shell
norca pack key generate
norca pack registry add internal oci://registry.example.internal/packs --trust-key=cosign.pub --require-signature
norca pack push ./redis --registry=internal --sign-key=cosign.key
norca pack run redis --registry=internal

norca pack bundle create --output=packs.tar --registry=internal redis:0.2.1
norca pack bundle import packs.tar --registry=internal-isolated
```

See the [supply chain guide](docs/supply-chain.md) for the configuration file, authentication,
trust policies, bundles, `pack.lock` and the artifact formats.

### Writing your own Packs

Nomad Pack is valuable when used with official and community packs, but many users will also want to
use their own.

Converting your existing Nomad job specifications into reusable packs is achievable in a few steps,
see the [Writing Packs documentation](docs/writing-packs.md) for more details.

## Pack Registries

Packs are organized into "registries" which contain multiple packs and shared templates.

The [Norca pack registry](https://github.com/bacanoo-io/norca-registry) is the public registry of the Norca operations packs;
contributions are welcome there. The
[Nomad Pack Community Registry](https://github.com/hashicorp/nomad-pack-community-registry) of the
Nomad community works as well (`norca pack registry add community
github.com/hashicorp/nomad-pack-community-registry`); its packs target Nomad and don't use Norca's
extensions.

Pull Requests and feedback on both repositories are welcome!

## Upcoming Features and Changes

- Support for Volumes and ACLs
- Support for other Version Control Systems
- Pack search command
- Server-side releases and signature enforcement in admission policies
- Keyless (Sigstore identity) signatures

## Additional Resources

- [Detailed Usage Guide](docs/detailed-usage.md)
- [OCI Registries, Signing, Offline Bundles and pack.lock](docs/supply-chain.md)
- [How to Write Your Own Pack](docs/writing-packs.md)
- [Contributing](docs/contributing.md)

## Tutorials

- [Introduction to Nomad Pack](https://learn.hashicorp.com/tutorials/nomad/nomad-pack-intro)
- [Writing Custom Packs](https://learn.hashicorp.com/tutorials/nomad/nomad-pack-writing-packs)
