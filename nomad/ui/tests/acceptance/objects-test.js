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
import { seedObjects } from 'nomad-ui/mirage/norca/objects';
import { seedReleases } from 'nomad-ui/mirage/norca/releases';
import { useToken, lastRequest } from 'nomad-ui/tests/helpers/norca';

module('Acceptance | objects and type definitions', function (hooks) {
  setupApplicationTest(hooks);
  setupMirage(hooks);

  hooks.beforeEach(function () {
    window.localStorage.clear();
    server.create('agent');
    server.create('node-pool');
    server.create('node');
    seedObjects(server);
    seedReleases(server);
    useToken(null, 'management');
  });

  test('objects are listed with the columns of their type definition', async function (assert) {
    await visit('/objects');

    assert.dom('[data-test-page-title]').hasText('Objects');
    assert.dom('[data-test-gutter-link="objects"]').exists();
    assert
      .dom('[data-test-object-list] [data-test-ui-table-header="col-0"]')
      .hasText('Version');
    assert
      .dom('[data-test-object-list] [data-test-ui-table-row]')
      .exists({ count: 3 });
    const billing = [
      ...document.querySelectorAll(
        '[data-test-object-list] [data-test-ui-table-row]'
      ),
    ].find((r) => r.textContent.includes('billing'));
    const cells = [...billing.querySelectorAll('[data-test-object-cell]')].map(
      (c) => c.textContent.trim()
    );
    assert.deepEqual(cells, ['16', '3', 'True']);
    assert
      .dom(billing.querySelector('[data-test-object-health]'))
      .hasText('ready');
    const reports = [
      ...document.querySelectorAll(
        '[data-test-object-list] [data-test-ui-table-row]'
      ),
    ].find((r) => r.textContent.includes('reports'));
    assert
      .dom(reports.querySelector('[data-test-object-health]'))
      .hasText('not ready');
    assert
      .dom(reports.querySelector('[data-test-object-generation]'))
      .hasText('4 / 3');
    await a11yAudit(assert, STRICT);

    await select('[data-test-kind-select]', 'backup.example.com/Schedule');
    assert.ok(currentURL().includes('kind=backup.example.com'));
    assert
      .dom('[data-test-object-list] [data-test-ui-table-empty]')
      .includesText('No object of this type');
  });

  test('an object shows spec, status, conditions, owners and its event log', async function (assert) {
    await visit('/objects');
    await click('[data-test-object-link="reports"]');
    assert.strictEqual(
      currentURL(),
      '/objects/db.example.com/PostgresCluster/reports?namespace=default'
    );

    assert.dom('[data-test-object-health]').hasText('not ready');
    assert
      .dom('[data-test-object-behind]')
      .includesText('generation 4, observed 3');
    assert.dom('[data-test-object-spec]').includesText('"replicas": 2');
    assert.dom('[data-test-object-status]').includesText('Observed generation');
    assert
      .dom('[data-test-object-conditions] [data-test-ui-table-row]')
      .exists({ count: 2 });
    assert.dom('[data-test-object-events]').includesText('ReplicaFailed (3×)');
    await a11yAudit(assert, STRICT);

    await visit(
      '/objects/db.example.com/PostgresCluster/test-db?namespace=staging'
    );
    assert.dom('[data-test-object-owner]').includesText('Release test-stack');
  });

  test('an unknown object says what to do', async function (assert) {
    await visit(
      '/objects/db.example.com/PostgresCluster/nope?namespace=default'
    );
    assert
      .dom('[data-test-object-error]')
      .includesText('may have been deleted');
  });

  test('type definitions list scope, objects, rules, webhooks and the package', async function (assert) {
    await visit('/kinds');
    assert.dom('[data-test-page-title]').hasText('Type definitions');
    assert
      .dom('[data-test-kind-list] [data-test-ui-table-row]')
      .exists({ count: 2 });
    const pg = [
      ...document.querySelectorAll(
        '[data-test-kind-list] [data-test-ui-table-row]'
      ),
    ].find((r) => r.textContent.includes('PostgresCluster'));
    assert.dom(pg.querySelector('[data-test-kind-objects]')).hasText('3');
    assert
      .dom(pg.querySelector('[data-test-kind-package]'))
      .hasText('postgres-operator');
    assert.dom(pg).includesText('yes (spec.replicas)');
    await a11yAudit(assert, STRICT);

    await click('[data-test-kind-link="db.example.com/PostgresCluster"]');
    assert.strictEqual(currentURL(), '/kinds/db.example.com/PostgresCluster');
    assert.dom('[data-test-kind-schema]').includesText('"maximum": 5');
    assert
      .dom('[data-test-kind-rules]')
      .includesText('replicas cannot drop below 2');
    assert
      .dom('[data-test-kind-columns] [data-test-ui-table-row]')
      .exists({ count: 3 });
    assert.dom('[data-test-webhook-approved]').hasText('yes');
    assert.dom('[data-test-kind-details]').includesText('at most 50');
    await a11yAudit(assert, STRICT);
  });

  test('admission policies: list, rules and delete with a typed name', async function (assert) {
    await visit('/admission');
    assert.dom('[data-test-page-title]').hasText('Admission policies');
    assert
      .dom('[data-test-admission-policies] [data-test-ui-table-row]')
      .exists({ count: 2 });
    assert
      .dom('[data-test-admission-policies]')
      .includesText('namespaces prod*');
    await a11yAudit(assert, STRICT);

    await click('[data-test-admission-link="no-privileged-containers"]');
    assert
      .dom('[data-test-admission-details]')
      .includesText('deny: the write is rejected');
    assert
      .dom('[data-test-admission-rules]')
      .includesText("t.Driver == 'docker'");
    await a11yAudit(assert, STRICT);

    await click('[data-test-delete-admission]');
    await fillIn('[data-test-confirm-typed]', 'no-privileged-containers');
    await click('[data-test-confirm-submit]');
    assert.ok(
      lastRequest('DELETE', '/v1/admission-policy/no-privileged-containers')
    );
    assert.strictEqual(currentURL(), '/admission');
    assert
      .dom('[data-test-admission-policies] [data-test-ui-table-row]')
      .exists({ count: 1 });
  });

  test('a token without admission rights does not see the menu item', async function (assert) {
    useToken({
      Namespaces: [{ Name: 'default', Capabilities: ['list-jobs'] }],
    });
    await visit('/');
    assert.dom('[data-test-gutter-link="admission"]').doesNotExist();
    assert.dom('[data-test-gutter-link="kinds"]').exists();
  });
});
