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
import { useToken, lastBody, lastRequest } from 'nomad-ui/tests/helpers/norca';

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
  });

  test('without releases the list says how to install a package', async function (assert) {
    useToken(null, 'management');
    server.db.norcaReleases.remove();
    await visit('/packages');
    assert.dom('[data-test-ui-table-empty]').includesText('norca pack run');
  });
});
