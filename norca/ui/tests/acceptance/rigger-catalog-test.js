/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

/* eslint-disable qunit/require-expect */
import { click, currentURL, fillIn, select, visit } from '@ember/test-helpers';
import { module, test } from 'qunit';
import { setupApplicationTest } from 'ember-qunit';
import { setupMirage } from 'ember-cli-mirage/test-support';
import a11yAudit, { STRICT } from 'nomad-ui/tests/helpers/a11y-audit';
import { seedReleases } from 'nomad-ui/mirage/norca/releases';
import { seedRigger } from 'nomad-ui/mirage/norca/rigger';
import { useToken, lastBody, lastRequest } from 'nomad-ui/tests/helpers/norca';

const REDIS = 'global/norca/redis';

async function openCatalog() {
  useToken(null, 'management');
  await visit('/packages');
  await click('[data-test-tab="catalog"]');
}

module('Acceptance | rigger catalog', function (hooks) {
  setupApplicationTest(hooks);
  setupMirage(hooks);

  hooks.beforeEach(function () {
    window.localStorage.clear();
    server.create('agent');
    server.create('node-pool');
    server.create('node');
    seedReleases(server);
    seedRigger(server);
  });

  test('a registry that shows what is selected, the whole index on request, filtered on the servers', async function (assert) {
    await openCatalog();

    assert
      .dom('[data-test-catalog-registry]')
      .hasValue('global', 'the global registry first');
    assert
      .dom('[data-test-module-card]')
      .exists({ count: 1 }, 'what is selected');
    assert.dom(`[data-test-module-card="${REDIS}"]`).includesText('0.3.0');
    assert
      .dom(`[data-test-module-card="${REDIS}"]`)
      .includesText('In-memory data store');
    assert
      .dom(
        `[data-test-module-card="${REDIS}"] [data-test-module-category-icon="database"]`
      )
      .exists('the icon of the category');
    assert.notOk(
      lastRequest('GET', '/search'),
      'the selection is in the status'
    );

    await click('[data-test-catalog-show-all] input');
    assert
      .dom('[data-test-module-card]')
      .exists({ count: 3 }, 'the whole index');
    assert
      .dom(
        '[data-test-module-card="global/norca/nats"] [data-test-module-category-icon="send"]'
      )
      .exists();

    await select('[data-test-catalog-category]', 'messaging');
    assert.ok(
      lastRequest('GET', '/search').url.includes('category=messaging'),
      'the servers filter'
    );
    assert.dom('[data-test-module-card]').exists({ count: 1 });
    await select('[data-test-catalog-category]', '');

    await fillIn('[data-test-catalog-keyword]', 'shop');
    assert.dom('[data-test-module-card]').exists({ count: 1 });
    assert.dom('[data-test-module-card="global/acme/shop"]').exists();
    await fillIn('[data-test-catalog-keyword]', '');

    await fillIn('[data-test-catalog-search]', 'standalone');
    assert
      .dom('[data-test-module-card]')
      .exists({ count: 1 }, 'a word of the description');
    assert.dom('[data-test-catalog-old]').doesNotExist();

    await click(`[data-test-btn-readme="${REDIS}"]`);
    assert
      .dom('[data-test-module-readme]')
      .includesText('In-memory data store');
    assert
      .dom('[data-test-module-icon]')
      .exists('the icon of the module opened');
    await a11yAudit(assert, STRICT);
  });

  test('an old index is shown as old, a registry never synced offers nothing', async function (assert) {
    await openCatalog();
    await select('[data-test-catalog-registry]', 'acme');
    assert.dom('[data-test-catalog-old]').includesText('10 days old');
    assert.dom('[data-test-module-card="acme/web-app"]').exists();

    const reg = server.db.norcaObjects.findBy({ Name: 'acme' });
    server.db.norcaObjects.update(reg.id, { Status: {} });
    await visit('/packages');
    await click('[data-test-tab="catalog"]');
    await select('[data-test-catalog-registry]', 'acme');
    assert.dom('[data-test-catalog-not-synced]').exists();
  });

  test('a module is installed from its form, with JSON values', async function (assert) {
    await openCatalog();
    await click(`[data-test-btn-install="${REDIS}"]`);

    assert.dom('[data-test-form-release-name]').hasValue('redis');
    assert
      .dom('[data-test-form-stored-secrets]')
      .includesText('nomad/jobs/redis#tls_key');

    // x-ui-show-if: replicas only for the replica mode.
    await click('[data-test-tab="resources"]');
    assert.dom('[data-test-pack-field="replicas"]').doesNotExist();
    await click(
      '[data-test-pack-field="resources.cpu"] [data-test-pack-preset="1000"]'
    );
    await click('[data-test-tab="general"]');
    await click('[data-test-radio-option="replica"]');
    await fillIn('[data-test-pack-field="datacenters"] textarea', 'dc1, dc2');
    await fillIn('[data-test-pack-field="extra_env"] textarea', 'not a pair');
    await click('[data-test-tab="resources"]');
    await fillIn('[data-test-pack-field="replicas"] input[type="number"]', '3');

    // A sensitive variable is a value of the install, never shown.
    await click('[data-test-tab="security"]');
    await click('[data-test-pack-field="auth_enabled"] input');
    await fillIn('[data-test-pack-field="redis_password"] input', 's3cret');

    // The text of a map that does not parse blocks the install.
    await click('[data-test-btn-plan]');
    assert.notOk(lastRequest('PUT', '/v1/rigger/install'));
    assert
      .dom('[data-test-pack-field="extra_env"] [data-test-field-error]')
      .includesText('key=value');
    await click('[data-test-tab="general"]');
    await fillIn(
      '[data-test-pack-field="extra_env"] textarea',
      'LOG_LEVEL=debug'
    );

    await click('[data-test-tab="cli"]');
    const command = document.querySelector(
      '[data-test-form-cli-command] pre'
    ).textContent;
    assert.ok(command.includes('/v1/rigger/install?namespace=default'));
    assert.ok(command.includes('"replicas":3'));
    assert.ok(command.includes('"redis_password":"<secret>"'));
    assert.notOk(command.includes('s3cret'), 'no secret in the command');

    await click('[data-test-btn-plan]');
    assert.ok(
      lastRequest('PUT', '/v1/rigger/install').url.includes('plan=true')
    );
    assert
      .dom('[data-test-plan-result] [data-test-plan-action="create"]')
      .exists();

    await click('[data-test-btn-deploy]');
    const body = lastBody('PUT', '/v1/rigger/install');
    assert.strictEqual(body.Registry, 'global');
    assert.strictEqual(body.Module, 'norca/redis');
    assert.strictEqual(body.Release, 'redis');
    assert.strictEqual(body.Version, '0.3.0');
    assert.notOk(body.Values, 'no HCL');
    assert.deepEqual(body.ValuesJSON, {
      mode: 'replica',
      replicas: 3,
      datacenters: ['dc1', 'dc2'],
      extra_env: { LOG_LEVEL: 'debug' },
      resources: { cpu: 1000, memory: 256 },
      auth_enabled: true,
      redis_password: 's3cret',
    });
    assert.strictEqual(currentURL(), '/packages/default/redis');
    // The next steps of the module stay in the notification of the install.
    assert.dom('[data-test-toast-title]').hasText('redis installed');
    assert
      .dom('[data-test-toast-message]')
      .includesText('redis runs in namespace default.');
    assert
      .dom('[data-test-toast-message]')
      .includesText('norca service info redis');
  });

  test('required variables, versions, and a version the cluster cannot run', async function (assert) {
    await openCatalog();
    await select('[data-test-catalog-registry]', 'acme');
    await click('[data-test-btn-install="acme/web-app"]');
    await click('[data-test-btn-deploy]');
    assert.notOk(lastRequest('PUT', '/v1/rigger/install'));
    assert
      .dom('[data-test-pack-field="image"] [data-test-field-error]')
      .hasText('Image is required');
    await fillIn('[data-test-pack-field="image"] input', 'nginx:1');

    await select('[data-test-form-version]', '1.0.0');
    assert.ok(
      lastRequest('GET', '/rigger/registry/acme/module').url.includes(
        'version=1.0.0'
      )
    );
    await fillIn('[data-test-pack-field="image"] input', 'nginx:1');
    await fillIn('[data-test-form-release-name]', 'web');
    await click('[data-test-btn-deploy]');
    const body = lastBody('PUT', '/v1/rigger/install');
    assert.strictEqual(body.Version, '1.0.0');
    assert.deepEqual(body.ValuesJSON, { image: 'nginx:1' });

    await visit('/packages');
    await click('[data-test-tab="catalog"]');
    await click('[data-test-catalog-show-all] input');
    await click('[data-test-btn-install="global/norca/nats"]');
    assert.dom('[data-test-pack-incompatible]').includesText('Norca >= 9.0.0');
    assert.dom('[data-test-btn-deploy]').isDisabled();
  });

  test('a release of another manager is taken over only with adopt', async function (assert) {
    server.db.norcaReleases.insert({
      Region: 'global',
      ID: 'rel-redis',
      Namespace: 'default',
      Name: 'redis',
      Spec: {
        pack: { name: 'redis', version: '7.2.4', manager: 'pack' },
        revision: 1,
        maxHistory: 10,
      },
      Status: { phase: 'deployed', revision: 1, resources: [], conditions: [] },
      History: [],
      Events: [],
      Drift: [],
    });
    await openCatalog();
    await click(`[data-test-btn-install="${REDIS}"]`);

    await click('[data-test-btn-plan]');
    assert
      .dom('[data-test-plan-result]')
      .includesText('takes it over only with adopt');
    await click('[data-test-btn-deploy]');
    assert.dom('[data-test-form-error]').includesText('adopt');
    assert.notOk(lastBody('PUT', '/v1/rigger/install').Adopt);

    await click('[data-test-form-adopt] input');
    await click('[data-test-btn-deploy]');
    assert.true(lastBody('PUT', '/v1/rigger/install').Adopt);
    assert.strictEqual(currentURL(), '/packages/default/redis');
  });

  test('registries are added, synced, edited and removed', async function (assert) {
    useToken(null, 'management');
    await visit('/packages');
    await click('[data-test-tab="registries"]');

    assert.dom('[data-test-registry-row="global"]').exists();
    assert
      .dom('[data-test-registry-row="global"]')
      .includesText('shows 1 selected');
    assert.dom('[data-test-registry-row="acme"]').exists();
    assert
      .dom('[data-test-registry-old]')
      .exists({ count: 1 }, 'the index of acme is old');
    await a11yAudit(assert, STRICT);

    await click('[data-test-btn-sync="acme"]');
    assert.ok(lastRequest('PUT', '/v1/rigger/registry/acme/sync'));
    assert.dom('[data-test-registry-old]').doesNotExist();

    await click('[data-test-btn-add-registry]');
    await fillIn('[data-test-input-reg-name]', 'Bad Name');
    await fillIn(
      '[data-test-input-reg-source]',
      'https://git.example.com/acme/rigger-modules'
    );
    await click('[data-test-btn-save-registry]');
    assert.dom('[data-test-registry-form-error]').includesText('lowercase');
    await fillIn('[data-test-input-reg-name]', 'mirror');
    await fillIn('[data-test-input-reg-auth-path]', 'registries/mirror');
    await click('[data-test-btn-save-registry]');
    assert.deepEqual(
      lastBody('PUT', '/v1/object/rigger.norca.io/Registry/mirror').Spec,
      {
        type: 'git',
        source: 'https://git.example.com/acme/rigger-modules',
        display: 'catalog',
        authSecret: { path: 'registries/mirror' },
      }
    );
    assert.dom('[data-test-registry-row="mirror"]').exists();
    assert
      .dom('[data-test-registry-auth]')
      .includesText('default/registries/mirror');

    // An edit keeps the selection and writes with the index it read.
    await click('[data-test-btn-edit="global"]');
    await select('[data-test-select-reg-display]', 'catalog');
    await click('[data-test-btn-save-registry]');
    const edit = lastRequest(
      'PUT',
      '/v1/object/rigger.norca.io/Registry/global'
    );
    assert.ok(edit.url.includes('cas='));
    assert.deepEqual(JSON.parse(edit.requestBody).Spec.selected, [
      { kind: 'module', namespace: 'norca', name: 'redis' },
    ]);

    await click('[data-test-btn-delete="mirror"]');
    await click('[data-test-confirm-submit]');
    assert.dom('[data-test-registry-row="mirror"]').doesNotExist();
  });

  test('a release of a module is upgraded from the registry it came from', async function (assert) {
    useToken(null, 'management');
    await fetch('/v1/rigger/install?namespace=default', {
      method: 'PUT',
      body: JSON.stringify({
        Release: 'redis',
        Registry: 'global',
        Module: 'norca/redis',
        ValuesJSON: { auth_enabled: true, redis_password: 's3cret' },
      }),
    });
    await visit('/packages/default/redis');

    assert.dom('[data-test-release-details]').includesText('Rigger');
    assert.dom('[data-test-release-details]').includesText('norca/redis');
    assert.dom('[data-test-release-details]').includesText('global');
    assert.dom('[data-test-release-pack-manager]').doesNotExist();
    assert.dom('[data-test-release-owner]').doesNotExist();

    await click('[data-test-show-values]');
    assert.deepEqual(
      JSON.parse(
        document.querySelector('[data-test-release-values] pre').textContent
      ),
      { auth_enabled: true },
      'the values of the release, without the sensitive one'
    );
    await click('[data-test-release-values] [data-test-modal-close]');

    await click('[data-test-upgrade-release]');
    assert
      .dom('[data-test-upgrade-modal]')
      .includesText('Sensitive values are not kept');
    assert
      .dom('[data-test-upgrade-modal] [data-test-form-release-name]')
      .isDisabled();
    await click('[data-test-upgrade-modal] [data-test-tab="security"]');
    assert
      .dom('[data-test-pack-field="auth_enabled"] input')
      .isChecked('the values of the release');
    assert
      .dom('[data-test-pack-field="redis_password"] input')
      .hasValue('', 'a sensitive value is not kept');
    await click('[data-test-upgrade-modal] [data-test-tab="resources"]');
    await fillIn('[data-test-pack-field="resources.memory"] input', '1024');
    await click('[data-test-upgrade-modal] [data-test-btn-deploy]');

    const body = lastBody('PUT', '/v1/rigger/install');
    assert.strictEqual(body.Release, 'redis');
    assert.strictEqual(body.Registry, 'global');
    assert.strictEqual(body.Module, 'norca/redis');
    assert.deepEqual(body.ValuesJSON, {
      auth_enabled: true,
      resources: { cpu: 500, memory: 1024 },
    });
    assert.dom('[data-test-upgrade-modal]').doesNotExist();
  });

  test('a release of a registry repository is upgraded by the name of its module', async function (assert) {
    useToken(null, 'management');
    await fetch('/v1/rigger/install?namespace=default', {
      method: 'PUT',
      body: JSON.stringify({
        Release: 'web',
        Registry: 'acme',
        Module: 'web-app',
        ValuesJSON: { image: 'nginx:1' },
      }),
    });
    await visit('/packages/default/web');
    await click('[data-test-upgrade-release]');
    assert.ok(
      lastRequest('GET', '/rigger/registry/acme/module').url.includes(
        'name=web-app'
      ),
      'the module of the registry, not its source acme/web-app'
    );
    assert.dom('[data-test-pack-field="image"] input').hasValue('nginx:1');
  });

  test('a release of a stack is changed through its stack', async function (assert) {
    useToken(null, 'management');
    server.db.norcaReleases.insert({
      Region: 'global',
      ID: 'rel-db',
      Namespace: 'default',
      Name: 'shop-db',
      Spec: {
        pack: {
          name: 'redis',
          version: '0.3.0',
          registry: 'global',
          source: 'norca/redis',
          manager: 'rigger',
          digest: 'sha256:x',
        },
        owner: {
          kind: 'stack',
          namespace: 'apps',
          name: 'shop',
          version: '1.2.0',
          part: 'data',
        },
        revision: 1,
        maxHistory: 10,
      },
      Status: { phase: 'deployed', revision: 1, resources: [], conditions: [] },
      History: [],
      Events: [],
      Drift: [],
    });
    await visit('/packages/default/shop-db');
    assert
      .dom('[data-test-release-owner]')
      .includesText('apps/shop 1.2.0, part data');
    assert.dom('[data-test-release-details]').includesText('apps/shop');
    assert
      .dom('[data-test-upgrade-release]')
      .doesNotExist('the stack upgrades it');
    await a11yAudit(assert, STRICT);
  });
});
