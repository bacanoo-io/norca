# Norca UI

The web UI of Norca, served by the agent at `/ui/`.

## Prerequisites

This is an [ember.js](https://emberjs.com/) project, and you will need the following tools installed on your computer.

- [The current Node.js LTS release](https://nodejs.org/)
- [Yarn](https://yarnpkg.com)
- [Ember CLI](https://ember-cli.com/)

## Installation

The Nomad UI gets cloned along with the rest of Nomad. To install dependencies, do the following from the root of the Nomad project:

```
$ cd ui
$ HUSKY_SKIP_INSTALL=1 yarn
```

`HUSKY_SKIP_INSTALL=1` keeps the install from setting up git hooks. Runtime
packages belong in `dependencies` and build or test tools in
`devDependencies`: the license inventory (`THIRD_PARTY_NOTICES.md`,
`make check-notices`) covers the runtime packages of `yarn.lock`.

## Running / Development

UI in development mode defaults to using fake generated data, but you can configure it to proxy a live running nomad process by setting `USE_MIRAGE` environment variable to `false`. First, make sure nomad is running. The UI, in development mode, runs independently from Nomad, so this could be an official release or a dev branch. Likewise, Nomad can be running in server mode or dev mode. As long as the API is accessible, the UI will work as expected.

- `USE_MIRAGE=false ember serve`
- Visit your app at [http://localhost:4200](http://localhost:4200).

You may need to reference the direct path to `ember`, typically in `./node_modules/.bin/ember`.

The fake data in development is generated from a stable seed of 1. To generate different data, you can include a query parameter of `?faker-seed=2` or any other number in the URL. To turn off the seed and get different data with every load, use `?faker=seed=0`.

When running with Mirage, the default scenario is set in `config/environment.js` but can be overridden with a query parameter to any of the scenarios named in `mirage/scenarios/default.js` with something like `?mirage-scenario=emptyCluster`.

## Running / Development with Vagrant

All necessary tools for UI development are installed as part of the Vagrantfile. This is primarily to make it easy to build the UI from source while working on Nomad. Due to the filesystem requirements of [Broccoli](http://broccolijs.com/) (which powers Ember CLI), it is strongly discouraged to use Vagrant for developing changes to the UI.

That said, development with Vagrant is still possible, but the `ember serve` command requires two modifications:

- `--watch polling`: This allows the vm to notice file changes made in the host environment.
- `--port 4201`: The default port 4200 is not forwarded, since local development is recommended.

This makes the full command for running the UI in development mode in Vagrant:

```
$ ember serve --watch polling --port 4201
```

### Running Tests

UI tests can be run independently of the Go tests.

- `ember test` (single run, headless browser)
- `ember test --server` (watches for changes, runs in a full browser)

You can use `--filter <test name>` to run a targetted set of tests, e.g. `ember test --filter 'allocation detail'`.

In the test environment, the fake data is generated with a random seed. If you want stable data, you can set a seed while running the test server by appending `&faker-seed=1` (or any other non-zero number) to the URL.

### Linting

- `yarn lint`
- `yarn lint:fix`

### Building

Typically `make prerelease` or `make dev-ui` will be the desired build workflow, but in the event that build artifacts need to be inspected, `ember build` will output compiled files in `ui/dist`.

- `ember build` (development)
- `ember build --environment production` (production)

### Releasing

UI releases are in lockstep with Norca releases: `make prerelease` builds the UI (and the previous UI) and embeds it in the agent (`command/agent/bindata_assetfs.go`).

### Styles and components

The UI is moving from the Bulma-based styles it inherited to
[Tabler](https://tabler.io) (MIT), page by page:

- `app/components/ui/` is the component layer new pages are built from
  (buttons, cards, tables, form fields, tabs, modal, dropdown, charts, ...).
  New pages use these components and Tabler classes only.
- `app/styles/norca/` holds the design tokens and the styles of the
  component layer and the frame. They are imported at the end of
  `app/styles/app.scss`, after the `norca-styles: unscoped below` marker.
- Everything before that marker is the inherited stylesheet. The in-repo
  addon `lib/norca-styles` scopes it under `.legacy` at build time and loads
  Tabler as a CSS cascade layer, so both can live on one page. Pages that
  are not migrated yet render inside a `.legacy` element (the default of
  `<PageLayout>`); a migrated page passes `@legacy={{false}}`.
- Icons come from Tabler Icons (MIT). `app/utils/icons/tabler.js` is
  generated with only the icons in use: add the name to
  `scripts/generate-icons.js` and run `node scripts/generate-icons.js`.
- Charts use ECharts (Apache-2.0), loaded lazily by `<Ui::Chart>`.
  ApexCharts must not be added, not even as a transitive dependency, and
  nothing from the `dist/libs` folder of Tabler may be imported.

Inherited pages still use HashiCorp Design System components
(`@hashicorp/design-system-components`, flight icons, structure icons);
new pages must not.

### Live updates

`services/event-stream` is the client of `/v1/event/stream`: one
connection per region and namespace with the union of the subscribed
topics. It resumes after the last delivered index with `exact=true`; when
the agent answers 410 Gone, the subscribers refetch (`onResync`) and the
stream continues after the refetched state. With Mirage (development) and
in tests it is disabled (`APP.eventStream` in `config/environment.js`).

### The previous UI

The UI of the previous release is served at `/ui/legacy/` for one release.
`make prerelease` (target `ember-dist-legacy`) builds it from the commit in
`LEGACY_UI_REF` with the root URL `/ui/legacy/` into `ui/dist/legacy`,
which is embedded with the rest of the UI. The agent serves
`legacy/index.html` for every unknown path under `/ui/legacy/`. A binary
built without the legacy UI shows the not-found page of the new UI there.
`make dev-ui` does not build the legacy UI.

The release workflow embeds the committed `command/agent/bindata_assetfs.go`:
after a UI change, run `make prerelease` (or `ember-dist`,
`ember-dist-legacy` and `static-assets`) and commit the regenerated file.
The legacy UI builds on Linux and macOS; on Windows its ember-auto-import
version fails, so build it in WSL.

### Troubleshooting

#### The UI is running, but none of the API requests are working

By default (according to the `.ember-cli` file), a proxy address of `http://localhost:4646` is used. If you are running Nomad at a different address, you will need to override this setting when running ember serve: `ember serve --proxy http://newlocation:1111`.

Also, ensure that `USE_MIRAGE` environment variable is set to false, so the UI proxy requests to Nomad process instead of using autogenerated test data.

#### Nomad is running in Vagrant, but I can't access the API from my host machine

Nomad binds to `127.0.0.1:4646` by default, which is the loopback address. Try running nomad bound to `0.0.0.0`: `bin/nomad -bind 0.0.0.0`.

Ports also need to be forwarded in the Vagrantfile. 4646 is already forwarded, but if a port other than the default is being used, that port needs to be added to the Vagrantfile and `vagrant reload` needs to be run.
