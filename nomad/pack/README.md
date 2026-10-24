
# Nomad Pack

Nomad Pack is a templating and packaging tool used with [Nomad](https://www.nomadproject.io).

In this repository Nomad Pack is part of the Norca fork of Nomad: the code under `nomad/pack/`
originates from Nomad Pack v0.4.2 and is built into the main `nomad` binary, where it runs as the
`nomad pack` subcommand. There is no separate `nomad-pack` binary. Packs are rendered on the client.

Nomad Pack is used to:

- Easily deploy popular applications to Nomad
- Re-use common patterns across internal applications
- Find and share job specifications with the Nomad community

## Installation

Nomad Pack is built into the `nomad` binary of this repository; build it from the `nomad/`
directory (for example with `go build .`) and make sure the binary is on your PATH. All Nomad Pack
commands are available below `nomad pack`:

```shell
nomad pack --help
```

## Usage

### Dependencies

Nomad Pack users must have Nomad running and accessible at the address defined in the `NOMAD_ADDR`
environment variable.

If Nomad ACLs are enabled, a token with proper permissions must be defined in the `NOMAD_TOKEN`
environment variable.

### Basic Use

First, run the `list` command to see which packs are available to deploy.

```shell
nomad pack list
```

If you don't see the default registry (`nomad pack registry list`), it can be added with the `registry add` command.

```shell
nomad pack registry add default github.com/hashicorp/nomad-pack-community-registry
```

To deploy one of these packs, use the `run` command. This deploys each jobs defined in the pack to Nomad.
To deploy the `hello_world` pack, you would run the following command:

```shell
nomad pack run hello_world
```

Each pack defines a set of variables that can be provided by the user. To get information on the pack
and to see which variables can be passed in, run the `info` command.

```shell
nomad pack info hello_world
```

Values for these variables are provided using the `--var` flag.

```shell
nomad pack run hello_world --var message=hola
```

Values can also be provided by passing in a variables file. See the variables section of the
[Detailed usage guide](/docs/detailed-usage.md) for details.

```shell
tee -a ./my-variables.hcl << END
message="bonjour"
END

nomad pack run hello_world -f ./my-variables.hcl
```

If you want to remove all of the resources deployed by a pack, run the `destroy` command with the
pack name.

```shell
nomad pack destroy hello_world
```

### Adding Non-Default Pack Registries

When using Nomad Pack, the default registry for packs is
[the Nomad Pack Community Registry](https://github.com/hashicorp/nomad-pack-community-registry).
Packs from this registry will be made automatically available.

You can add additional registries by using the `registry add` command. For instance, if you wanted
to add [an example Gitlab registry](https://gitlab.com/mikenomitch/pack-registry),
you would run the following command to download the registry.

```shell
nomad pack registry add example gitlab.com/mikenomitch/pack-registry
```

To view the packs you can now deploy, run the `registry list` command.

```shell
nomad pack registry list
```

Packs from this registry can now be deployed using the `run` command.

### Writing your own Packs

Nomad Pack is valuable when used with official and community packs, but many users will also want to
use their own.

Converting your existing Nomad job specifications into reusable packs is achievable in a few steps,
see the [Writing Packs documentation](/docs/writing-packs.md) for more details.

## Pack Registries

Packs are organized into "registries" which contain multiple packs and shared templates.

The [Nomad Pack Community Registry](https://github.com/hashicorp/nomad-pack-community-registry) is
a public registry for community-maintained packs. Nomad community members are encouraged to share
their packs and collaborate with one another in this repo.

Pull Requests and feedback on both repositories are welcome!

## Upcoming Features and Changes

- Support for Volumes and ACLs
- Support for other Version Control Systems
- Pack search command

## Additional Resources

- [Detailed Usage Guide](/docs/detailed-usage.md)
- [How to Write Your Own Pack](/docs/writing-packs.md)
- [Contributing](/docs/contributing.md)

## Tutorials

- [Introduction to Nomad Pack](https://learn.hashicorp.com/tutorials/nomad/nomad-pack-intro)
- [Writing Custom Packs](https://learn.hashicorp.com/tutorials/nomad/nomad-pack-writing-packs)
