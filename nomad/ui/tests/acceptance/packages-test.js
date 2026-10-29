/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

/* eslint-disable qunit/require-expect */
import {
  click,
  currentURL,
  fillIn,
  select,
  settled,
  triggerEvent,
  visit,
} from '@ember/test-helpers';
import { module, test } from 'qunit';
import { setupApplicationTest } from 'ember-qunit';
import { setupMirage } from 'ember-cli-mirage/test-support';
import a11yAudit, { STRICT } from 'nomad-ui/tests/helpers/a11y-audit';
import { seedReleases, revision } from 'nomad-ui/mirage/norca/releases';
import { seedPacks } from 'nomad-ui/mirage/norca/packs';
import {
  useToken,
  lastBody,
  lastRequest,
  setEditorText,
  editorJSON,
} from 'nomad-ui/tests/helpers/norca';

const RELEASE = '/packages/platform/postgres-operator';

module('Acceptance | packages', function (hooks) {
  setupApplicationTest(hooks);
  setupMirage(hooks);

  hooks.beforeEach(function () {
    window.localStorage.clear();
    server.create('agent');
    server.create('node-pool');
    server.create('node');
    seedReleases(server);
    seedPacks(server);
  });

  test('the releases with pack, revision, status, drift and signature', async function (assert) {
    useToken(null, 'management');
    await visit('/packages');

    assert.dom('[data-test-page-title]').hasText('Packages');
    assert.dom('[data-test-gutter-link="packages"]').exists();
    assert
      .dom('[data-test-release-list] [data-test-ui-table-row]')
      .exists({ count: 2 });
    assert.dom('[data-test-drift-alert]').includesText('1 release differs');
    const rows = [
      ...document.querySelectorAll(
        '[data-test-release-list] [data-test-ui-table-row]'
      ),
    ];
    assert.dom(rows[0]).includesText('observability');
    assert
      .dom(rows[0].querySelector('[data-test-release-drift]'))
      .hasText('drifted');
    assert
      .dom(rows[0].querySelector('[data-test-release-signature]'))
      .hasText('unsigned');
    assert
      .dom(rows[1].querySelector('[data-test-release-signature]'))
      .hasText('verified');
    assert
      .dom(rows[1].querySelector('[data-test-release-phase]'))
      .hasText('deployed');
    await a11yAudit(assert, STRICT);

    await visit('/');
    assert
      .dom('[data-test-warnings]')
      .includesText('Release observability differs from its revision');
  });

  test('a release: resources, revisions, comparison, hooks and events', async function (assert) {
    useToken(null, 'management');
    await visit('/packages');
    await click('[data-test-release-link="postgres-operator"]');
    assert.strictEqual(currentURL(), RELEASE);

    assert.dom('[data-test-release-phase]').hasText('deployed');
    assert
      .dom('[data-test-release-resources] [data-test-ui-table-row]')
      .exists({ count: 5 });
    assert
      .dom('[data-test-release-history] [data-test-timeline-item]')
      .exists({ count: 3 });
    assert
      .dom('[data-test-release-history]')
      .includesText('post-upgrade hook failed');
    assert.dom('[data-test-compare-pack]').includesText('1.3.2 → 1.4.0');
    assert.dom('[data-test-release-compare]').includesText('replicas');
    assert.dom('[data-test-resource-change]').hasText('changed');
    await select('[data-test-compare-a]', '1');
    assert.dom('[data-test-compare-pack]').includesText('1.3.0 → 1.4.0');
    assert
      .dom('[data-test-release-hooks]')
      .includesText('postgres-operator-migrate');
    assert
      .dom('[data-test-release-events]')
      .includesText('revision 3 deployed');
    assert
      .dom('[data-test-release-details]')
      .includesText('Verified by the server');
    await a11yAudit(assert, STRICT);
  });

  test('drift is checked on request and explained', async function (assert) {
    useToken(null, 'management');
    await visit('/packages/platform/observability');
    assert.dom('[data-test-release-drift]').exists();
    await click('[data-test-check-drift]');
    assert
      .dom('[data-test-release-drift]')
      .includesText('job platform/log-shipper: modified');

    await visit(RELEASE);
    await click('[data-test-check-drift]');
    assert.dom('[data-test-release-in-sync]').includesText('match revision 3');
  });

  test('roll back previews the changes, then applies a new revision', async function (assert) {
    useToken(null, 'management');
    await visit(RELEASE);
    await click('[data-test-rollback]');

    assert.true(lastBody('POST', '/rollback').DryRun);
    assert.strictEqual(lastBody('POST', '/rollback').Revision, 1);
    assert.dom('[data-test-rollback-preview]').includesText('update');
    await a11yAudit(assert, STRICT);
    await click('[data-test-confirm-submit]');

    const body = lastBody('POST', '/rollback');
    assert.notOk(body.DryRun);
    assert.strictEqual(body.Revision, 1);
    assert
      .dom('[data-test-release-history] [data-test-timeline-item]')
      .exists({ count: 4 });
    assert.dom('[data-test-release-history]').includesText('rollback to 1');
  });

  test('tests run on request and the result is shown', async function (assert) {
    useToken(null, 'management');
    await visit(RELEASE);
    await click('[data-test-run-tests]');
    assert.ok(lastRequest('POST', '/release/postgres-operator/test'));
    assert.dom('[data-test-last-test]').includesText('passed');
  });

  test('uninstall needs the typed release name', async function (assert) {
    useToken(null, 'management');
    await visit(RELEASE);
    await click('[data-test-uninstall]');
    assert.dom('[data-test-confirm-submit]').isDisabled();
    await click('[data-test-skip-hooks] input');
    await fillIn('[data-test-confirm-typed]', 'postgres-operator');
    await click('[data-test-confirm-submit]');

    const request = lastRequest('DELETE', '/v1/release/postgres-operator');
    assert.ok(request.url.includes('no_hooks=true'));
    assert.strictEqual(currentURL(), '/packages');
    assert
      .dom('[data-test-release-list] [data-test-ui-table-row]')
      .exists({ count: 1 });
  });

  test('values are edited against the variables schema and become an upgrade command', async function (assert) {
    useToken(null, 'management');
    await visit(RELEASE);
    await click('[data-test-edit-values]');
    assert.dom('[data-test-values-editor]').exists();
    const values = editorJSON();
    assert.strictEqual(values.replicas, 3, 'the deployed revision 3');
    await a11yAudit(assert, STRICT);

    await setEditorText({ ...values, replicas: 8 });
    assert
      .dom('[data-test-editor-problem]')
      .includesText('replicas: must be at most 5');

    await setEditorText({ ...values, replicas: 4 });
    await click('[data-test-editor-review-button]');
    assert.dom('[data-test-editor-review] [data-test-text-diff]').exists();
    await click('[data-test-editor-submit]');
    assert.dom('[data-test-values-command-ready]').exists();
    const command = document
      .querySelector('[data-test-values-command] pre')
      .textContent.trim();
    assert.ok(
      command.startsWith('norca pack run postgres-operator'),
      'renders the same pack'
    );
    assert.ok(command.includes("--name='postgres-operator'"));
    assert.ok(command.includes("--namespace='platform'"));
    assert.ok(command.includes("--var 'replicas=4'"));
    assert.notOk(
      lastRequest('PUT', '/v1/release/postgres-operator'),
      'nothing is written by the UI'
    );
  });

  test('a token with read only sees the release without actions', async function (assert) {
    useToken({
      Namespaces: [
        {
          Name: 'platform',
          Capabilities: ['read-job'],
          Objects: [
            {
              Kind: 'packs.norca.io/Release',
              Policy: 'read',
              Capabilities: [],
            },
          ],
        },
      ],
    });
    await visit(RELEASE);
    assert.dom('[data-test-check-drift]').exists();
    assert.dom('[data-test-rollback]').doesNotExist();
    assert.dom('[data-test-uninstall]').doesNotExist();
    assert.dom('[data-test-run-tests]').doesNotExist();
    assert.dom('[data-test-edit-values]').doesNotExist();
  });

  test('without releases the list says how to install a package', async function (assert) {
    useToken(null, 'management');
    server.db.norcaReleases.remove();
    await visit('/packages');
    assert.dom('[data-test-ui-table-empty]').includesText('norca pack run');
  });

  test('the catalog lists the packs of the registries, newest version first', async function (assert) {
    useToken(null, 'management');
    await visit('/packages');
    await click('[data-test-tab="catalog"]');

    assert
      .dom('[data-test-pack-card]')
      .exists({ count: 2 }, 'one card per pack');
    assert.dom('[data-test-pack-card="builtin/redis"]').includesText('7.2.4');
    assert.dom('[data-test-pack-card="builtin/redis"]').includesText('Signed');
    assert
      .dom(
        '[data-test-pack-card="builtin/loadbalancer-operator"] [data-test-pack-incompatible]'
      )
      .exists();

    await select('[data-test-catalog-category]', 'networking');
    assert.dom('[data-test-pack-card]').exists({ count: 1 });
    await select('[data-test-catalog-category]', '');

    await click('[data-test-btn-readme="builtin/redis"]');
    assert.dom('[data-test-pack-readme]').includesText('In-memory data store');
    await a11yAudit(assert, STRICT);
  });

  test('a pack is installed from its variables form', async function (assert) {
    useToken(null, 'management');
    await visit('/packages');
    await click('[data-test-tab="catalog"]');
    await click('[data-test-btn-install="builtin/redis"]');

    assert.dom('[data-test-form-release-name]').hasValue('redis');
    assert.dom('[data-test-tab="general"]').hasText('General');
    assert.dom('[data-test-tab="resources"]').exists();
    assert.dom('[data-test-tab="security"]').exists();
    assert.dom('[data-test-pack-field="namespace"] select').exists();

    // x-ui-show-if: replicas only for the replica mode.
    await click('[data-test-tab="resources"]');
    assert.dom('[data-test-pack-field="replicas"]').doesNotExist();
    assert
      .dom('[data-test-pack-field="resources.cpu"]')
      .exists('object attributes are fields');
    await click(
      '[data-test-pack-field="resources.cpu"] [data-test-pack-preset="1000"]'
    );

    await click('[data-test-tab="general"]');
    await click('[data-test-radio-option="replica"]');
    await fillIn('[data-test-pack-field="datacenters"] textarea', 'dc1, dc2');
    await fillIn('[data-test-pack-field="extra_env"] textarea', 'not a pair');
    await click('[data-test-tab="resources"]');
    // The slider and its number field are one value.
    assert
      .dom('[data-test-pack-field="replicas"]')
      .hasAttribute('data-test-pack-widget', 'slider');
    assert.dom('[data-test-pack-slider-min]').hasText('1');
    assert.dom('[data-test-pack-slider-max]').hasText('9');
    await fillIn(
      '[data-test-pack-field="replicas"] [data-test-pack-slider]',
      '6'
    );
    assert
      .dom('[data-test-pack-field="replicas"] input[type="number"]')
      .hasValue('6');
    await fillIn('[data-test-pack-field="replicas"] input[type="number"]', '3');
    assert.dom('[data-test-pack-slider]').hasValue('3');

    await click('[data-test-tab="security"]');
    assert.dom('[data-test-pack-field="redis_password"]').doesNotExist();
    await click('[data-test-pack-field="auth_enabled"] input');
    await fillIn('[data-test-pack-field="redis_password"] input', 's3cret');
    assert
      .dom('[data-test-pack-field="redis_password"]')
      .includesText('nomad/jobs/redis');

    // The text of a map that does not parse blocks the install.
    await click('[data-test-btn-plan]');
    assert.notOk(lastRequest('POST', '/plan-pack'));
    assert.dom('[data-test-tab="general"] .badge').hasText('1');
    assert
      .dom('[data-test-pack-field="extra_env"] [data-test-field-error]')
      .includesText('key=value');
    await fillIn(
      '[data-test-pack-field="extra_env"] textarea',
      'LOG_LEVEL=debug'
    );

    await click('[data-test-tab="cli"]');
    const command = document.querySelector(
      '[data-test-form-cli-command] pre'
    ).textContent;
    assert.ok(command.includes("--registry='builtin'"));
    assert.ok(command.includes("--var 'replicas=3'"));
    assert.ok(
      command.includes("--var-secret 'nomad/jobs/redis:password=<secret>'")
    );
    assert.notOk(command.includes('s3cret'), 'no secret in the command');
    assert.notOk(command.includes('--atomic'), 'atomic left to the namespace');

    // Atomic mode: the namespace default, or set for this install.
    assert
      .dom('[data-test-form-atomic] [data-test-radio-option="default"]')
      .isChecked();
    await click('[data-test-form-atomic] [data-test-radio-option="off"]');
    await click('[data-test-tab="cli"]');
    assert.ok(
      document
        .querySelector('[data-test-form-cli-command] pre')
        .textContent.includes('--atomic=false')
    );

    await click('[data-test-btn-plan]');
    assert
      .dom('[data-test-plan-result] [data-test-plan-action="create"]')
      .exists();

    await click('[data-test-btn-deploy]');
    const body = lastBody('POST', '/v1/release/redis/deploy-pack');
    assert.strictEqual(body.Registry, 'builtin');
    assert.false(body.Atomic, 'the atomic mode chosen');
    assert.strictEqual(body.Pack, 'redis');
    assert.strictEqual(body.Version, '7.2.4');
    assert.deepEqual(
      body.Values,
      {
        mode: 'replica',
        replicas: 3,
        datacenters: ['dc1', 'dc2'],
        extra_env: { LOG_LEVEL: 'debug' },
        resources: { cpu: 1000, memory: 256 },
        auth_enabled: true,
      },
      'only the changes, without the secret'
    );
    assert.deepEqual(body.Secrets, [
      { Path: 'nomad/jobs/redis', Items: { password: 's3cret' } },
    ]);
    assert.strictEqual(currentURL(), '/packages/default/redis');
  });

  test('the values are edited as JSON too', async function (assert) {
    useToken(null, 'management');
    await visit('/packages');
    await click('[data-test-tab="catalog"]');
    await click('[data-test-btn-install="builtin/redis"]');
    await click('[data-test-tab="general"]');
    await click('[data-test-radio-option="replica"]');

    await click('[data-test-tab="json"]');
    const editor = () =>
      document.querySelector('[data-test-form-json] .CodeMirror').CodeMirror;
    assert.deepEqual(JSON.parse(editor().getValue()), { mode: 'replica' });

    editor().setValue('{"mode": "replica", "replicas": 4');
    await settled();
    assert.dom('[data-test-form-json-error]').includesText('Not valid JSON');
    await click('[data-test-btn-deploy]');
    assert.notOk(lastRequest('POST', '/deploy-pack'), 'blocked by the JSON');
    assert.dom('[data-test-tab="json"] .badge').hasText('1');

    editor().setValue('{"mode": "replica", "replicas": 4, "nope": 1}');
    await settled();
    assert.dom('[data-test-form-json-error]').includesText('nope');

    editor().setValue('{"mode": "replica", "replicas": 4}');
    await settled();
    assert.dom('[data-test-form-json-error]').doesNotExist();
    await click('[data-test-tab="resources"]');
    assert
      .dom('[data-test-pack-field="replicas"] input[type="number"]')
      .hasValue('4');

    await click('[data-test-btn-deploy]');
    assert.deepEqual(lastBody('POST', '/v1/release/redis/deploy-pack').Values, {
      mode: 'replica',
      replicas: 4,
    });
  });

  test('required variables and versions', async function (assert) {
    useToken(null, 'management');
    await visit('/packages');
    await click('[data-test-tab="registries"]');
    await click('[data-test-btn-add-registry]');
    await fillIn('[data-test-input-reg-name]', 'apps');
    await fillIn('[data-test-input-reg-source]', 'oci://ghcr.io/example/packs');
    await click('[data-test-btn-save-registry]');

    await click('[data-test-tab="catalog"]');
    await click('[data-test-btn-install="apps/web-app"]');
    await click('[data-test-btn-deploy]');
    assert.notOk(lastRequest('POST', '/deploy-pack'));
    assert
      .dom('[data-test-pack-field="image"] [data-test-field-error]')
      .hasText('Image is required');
    await fillIn('[data-test-pack-field="image"] input', 'nginx:1');
    await fillIn('[data-test-form-release-name]', 'web');
    await click('[data-test-btn-deploy]');
    assert.deepEqual(lastBody('POST', '/v1/release/web/deploy-pack').Values, {
      image: 'nginx:1',
    });

    // Another version reloads the form of that version.
    await visit('/packages');
    await click('[data-test-tab="catalog"]');
    await click('[data-test-btn-install="builtin/redis"]');
    await select('[data-test-form-version]', '7.0.15');
    assert.ok(
      lastRequest('GET', '/v1/pack/builtin/redis').url.includes(
        'version=7.0.15'
      )
    );
    await click('[data-test-btn-deploy]');
    assert.strictEqual(lastBody('POST', '/deploy-pack').Version, '7.0.15');
  });

  test('a release from a cluster registry is upgraded with its values', async function (assert) {
    useToken(null, 'management');
    await visit('/packages');
    await click('[data-test-tab="catalog"]');
    await click('[data-test-btn-install="builtin/redis"]');
    await click('[data-test-tab="security"]');
    await click('[data-test-pack-field="auth_enabled"] input');
    await fillIn('[data-test-pack-field="redis_password"] input', 's3cret');
    await click('[data-test-btn-deploy]');
    assert.strictEqual(currentURL(), '/packages/default/redis');

    await click('[data-test-upgrade-release]');
    assert
      .dom('[data-test-upgrade-modal] [data-test-form-release-name]')
      .isDisabled();
    await click('[data-test-upgrade-modal] [data-test-tab="security"]');
    assert
      .dom('[data-test-pack-field="auth_enabled"] input')
      .isChecked('the values of the release');
    assert
      .dom('[data-test-pack-field="redis_password"]')
      .includesText('Leave empty to keep');
    await click('[data-test-upgrade-modal] [data-test-tab="resources"]');
    await fillIn('[data-test-pack-field="resources.memory"] input', '1024');
    await click('[data-test-upgrade-modal] [data-test-btn-deploy]');

    const body = lastBody('POST', '/v1/release/redis/deploy-pack');
    assert.deepEqual(body.Values, {
      auth_enabled: true,
      resources: { cpu: 500, memory: 1024 },
    });
    assert.notOk(body.Secrets, 'the stored password is kept');
    assert.dom('[data-test-upgrade-modal]').doesNotExist();

    // Releases of CLI installs (a registry URL) are upgraded with the CLI.
    await visit(RELEASE);
    assert.dom('[data-test-upgrade-release]').doesNotExist();
  });

  test('a release installed with the CLI is upgraded after its registry is added', async function (assert) {
    useToken(null, 'management');
    // norca pack run web-app --registry=apps-local, from an OCI registry.
    const pack = {
      name: 'web-app',
      version: '1.2.0',
      registry: 'apps-local',
      source: 'oci://ghcr.io/example/packs:1.2.0',
      ref: '1.2.0',
    };
    server.db.norcaReleases.insert({
      Region: 'global',
      ID: 'rel-web',
      Namespace: 'default',
      Name: 'web',
      Spec: { pack, revision: 1, maxHistory: 10, atomic: true },
      Status: { phase: 'deployed', revision: 1, resources: [], conditions: [] },
      History: [
        revision('default', 'web', 1, '1.2.0', {
          spec: { pack, values: { image: 'nginx:1' } },
          status: { phase: 'deployed', hooks: [] },
        }),
      ],
      Events: [],
      Drift: [],
    });

    await visit('/packages/default/web');
    assert.dom('[data-test-upgrade-release]').doesNotExist();
    await click('[data-test-add-release-registry]');
    assert.dom('[data-test-add-registry-name]').hasValue('apps-local');
    assert.dom('[data-test-add-registry-ref]').hasValue('1.2.0');
    assert
      .dom('[data-test-add-registry-source]')
      .hasValue('oci://ghcr.io/example/packs');
    await click('[data-test-add-registry-confirm]');
    assert.deepEqual(lastBody('POST', '/v1/pack-registry'), {
      Name: 'apps-local',
      Spec: {
        type: 'oci',
        source: 'oci://ghcr.io/example/packs',
        defaultRef: '1.2.0',
      },
    });

    await click('[data-test-upgrade-release]');
    assert
      .dom('[data-test-upgrade-modal] [data-test-pack-field="image"] input')
      .hasValue('nginx:1', 'the values of the CLI install');
  });

  test('packs are uploaded and removed on the Registries tab', async function (assert) {
    useToken(null, 'management');
    await visit('/packages');
    await click('[data-test-tab="registries"]');

    const file = new File(['archive'], 'web-app-1.2.0.tgz', {
      type: 'application/gzip',
    });
    await triggerEvent('[data-test-btn-upload-pack] input', 'change', {
      files: [file],
    });
    assert.ok(
      lastRequest('PUT', '/v1/pack-upload').url.includes(
        'filename=web-app-1.2.0.tgz'
      )
    );
    assert
      .dom('[data-test-registry-row="uploads"]')
      .includesText('Uploaded packs');
    assert
      .dom('[data-test-registry-upload="web-app-1.2.0.tgz"]')
      .includesText('web-app@1.2.0');

    await click('[data-test-tab="catalog"]');
    assert.dom('[data-test-pack-card="uploads/web-app"]').exists();

    await click('[data-test-tab="registries"]');
    await click('[data-test-btn-remove-upload="web-app-1.2.0.tgz"]');
    assert.ok(lastRequest('DELETE', '/v1/pack-upload/uploads/'));
    assert
      .dom('[data-test-registry-upload="web-app-1.2.0.tgz"]')
      .doesNotExist();
  });

  test('a release from a local directory is upgraded after its pack is uploaded', async function (assert) {
    useToken(null, 'management');
    const pack = {
      name: 'web-app',
      version: '1.2.0',
      registry: '<<local folder>>',
      path: '/home/ops/packs/web-app',
    };
    server.db.norcaReleases.insert({
      Region: 'global',
      ID: 'rel-local',
      Namespace: 'default',
      Name: 'local-web',
      Spec: { pack, revision: 1, maxHistory: 10, atomic: true },
      Status: { phase: 'deployed', revision: 1, resources: [], conditions: [] },
      History: [
        revision('default', 'local-web', 1, '1.2.0', {
          spec: { pack, values: { image: 'nginx:1' } },
          status: { phase: 'deployed', hooks: [] },
        }),
      ],
      Events: [],
      Drift: [],
    });

    await visit('/packages/default/local-web');
    assert.dom('[data-test-upgrade-release]').doesNotExist();
    assert.dom('[data-test-add-release-registry]').doesNotExist();
    await triggerEvent('[data-test-upload-release-pack] input', 'change', {
      files: [new File(['archive'], 'web-app-1.2.0.tgz')],
    });
    await click('[data-test-upgrade-release]');
    assert
      .dom('[data-test-upgrade-modal] [data-test-pack-field="image"] input')
      .hasValue('nginx:1');
    assert.dom('[data-test-upgrade-modal]').includesText('uploads registry');
  });

  test('pack registries are added, synced, edited and removed', async function (assert) {
    useToken(null, 'management');
    await visit('/packages');
    await click('[data-test-tab="registries"]');

    assert
      .dom('[data-test-registry-row="builtin"]')
      .includesText('Server directory');
    assert.dom('[data-test-registry-packs]').hasText('3');

    await click('[data-test-btn-add-registry]');
    await click('[data-test-btn-save-registry]');
    assert
      .dom('[data-test-registry-form-error]')
      .hasText('The name is required');

    await fillIn('[data-test-input-reg-name]', 'apps');
    await select('[data-test-select-reg-type]', 'oci');
    await fillIn('[data-test-input-reg-source]', 'oci://ghcr.io/example/packs');
    await fillIn('[data-test-input-reg-ref]', 'v1');
    await select('[data-test-select-reg-interval]', '1h');
    await fillIn('[data-test-input-reg-auth-path]', 'registries/ghcr');
    await fillIn(
      '[data-test-input-reg-keys]',
      '-----BEGIN PUBLIC KEY-----\nabc\n-----END PUBLIC KEY-----\n\nkey-2'
    );
    await click('[data-test-btn-save-registry]');
    assert.deepEqual(lastBody('POST', '/v1/pack-registry'), {
      Name: 'apps',
      Spec: {
        type: 'oci',
        source: 'oci://ghcr.io/example/packs',
        defaultRef: 'v1',
        syncInterval: '1h',
        authSecret: { path: 'registries/ghcr' },
        trustedKeys: [
          '-----BEGIN PUBLIC KEY-----\nabc\n-----END PUBLIC KEY-----',
          'key-2',
        ],
      },
    });
    assert.dom('[data-test-registry-modal]').doesNotExist();
    assert.dom('[data-test-registry-row="apps"]').exists();
    assert
      .dom('[data-test-registry-auth]')
      .includesText('default/registries/ghcr');
    const apps = [
      ...document.querySelectorAll(
        '[data-test-registries-table] [data-test-ui-table-row]'
      ),
    ].find((r) => r.textContent.includes('apps'));
    assert
      .dom(apps.querySelector('[data-test-registry-next-sync]'))
      .includesText('next in an hour');

    // A source the servers cannot fetch is saved with the error.
    await click('[data-test-btn-add-registry]');
    await fillIn('[data-test-input-reg-name]', 'broken');
    await select('[data-test-select-reg-type]', 'git');
    await fillIn(
      '[data-test-input-reg-source]',
      'https://example.com/missing.git'
    );
    await click('[data-test-btn-save-registry]');
    const broken = [
      ...document.querySelectorAll(
        '[data-test-registries-table] [data-test-ui-table-row]'
      ),
    ].find((r) => r.textContent.includes('broken'));
    assert
      .dom(broken.querySelector('[data-test-registry-phase]'))
      .includesText('Error');
    assert
      .dom(broken.querySelector('[data-test-registry-phase]'))
      .includesText('not found');

    await click('[data-test-btn-sync="builtin"]');
    assert.ok(lastRequest('POST', '/v1/pack-registry/builtin/sync'));

    await click('[data-test-btn-edit="apps"]');
    assert.dom('[data-test-input-reg-name]').isDisabled();
    assert.dom('[data-test-input-reg-ref]').hasValue('v1');
    assert.dom('[data-test-select-reg-interval]').hasValue('1h');
    assert.dom('[data-test-input-reg-auth-path]').hasValue('registries/ghcr');
    await click('[data-test-btn-cancel-registry]');

    await click('[data-test-btn-delete="broken"]');
    await click('[data-test-confirm-submit]');
    assert.ok(lastRequest('DELETE', '/v1/pack-registry/broken'));
    assert.dom('[data-test-registry-row="broken"]').doesNotExist();
  });
});
