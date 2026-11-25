# An index of the shape of the global registry

The global registry of Rigger (github.com/bacanoo-io/norca-global-registry)
holds **entries, not sources**: small JSON files that list, per namespace
and name, the signed versions of modules and stacks, and where each one is
(the repository, tag and commit of its author), with its tree digest and
signatures. A source `<namespace>/<name>` that names no registry of a
stack is a module of it.

This directory is such an index over the registry of this project, so that
the global form of an address can be tried on one machine:
`<% .Owner %>/webapp` names the module `webapp` of the namespace `<% .Owner %>`.
An organization can run an index of its own the same way, for its own
repositories.

```
index/
  namespaces/<% .Owner %>.json      the owner and the source repositories of the namespace
  keys/<% .Owner %>/*.pub           the public keys its versions must be signed with
  advisories.json                   warnings and blocked versions (none)
  modules/<letter>/<% .Owner %>/<name>.json   written by rigger index update
  stacks/<letter>/<% .Owner %>/<name>.json    written by rigger index update
```

The entries are not written by hand. After the modules of the registry are
published (`../registry/README.md`):

```shell
mkdir -p index/keys/<% .Owner %>
cp keys/registry.pub index/keys/<% .Owner %>/registry.pub
rigger index update --dir index        # lists every tagged, signed and checked version
rigger index check --dir index         # checks the files of the index
```

Then the index stands in for the global registry:

```shell
export RIGGER_GLOBAL_REGISTRY="$PWD/index"
rigger search webapp
rigger show <% .Owner %>/webapp --readme
```

and a stack without a `registry "<% .Owner %>"` block installs
`<% .Owner %>/webapp` from it. Without `RIGGER_GLOBAL_REGISTRY` the same
address names the namespace `<% .Owner %>` of the real global registry.

`rigger index update` lists the modules and leaves the stack
`web-platform` out, saying why: **a stack of the global registry uses only
modules of the global registry** (and its own and built-in ones), so that
everything it installs can be found and verified in one place.
`web-platform` names its modules through its `registry` block. A stack
meant for the global registry leaves that block out and names
`<% .Owner %>/cache` as a module of the global registry; it is synced with
the index in `RIGGER_GLOBAL_REGISTRY` once its modules are listed there.

Getting a namespace into the real global registry is asked for with an
issue there; its maintainers add the namespace, its keys and its
repositories, and its workflows list the versions from then on.
