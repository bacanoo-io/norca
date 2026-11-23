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
    assert.dom('[data-test-ui-table-empty]').includesText('rigger install');
  });

  test('a release from a cluster registry is upgraded with its values', async function (assert) {
    useToken(null, 'management');
    // A pack installed by the servers (norca pack run on the servers, or a
    // UI before Rigger): the catalog of the UI installs modules of Rigger.
    await fetch('/v1/release/redis/deploy-pack?namespace=default', {
      method: 'POST',
      body: JSON.stringify({
        Registry: 'builtin',
        Pack: 'redis',
        Version: '7.2.4',
        Values: { auth_enabled: true },
        Secrets: [{ Path: 'nomad/jobs/redis', Items: { password: 's3cret' } }],
      }),
    });
    await visit('/packages/default/redis');

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
});
