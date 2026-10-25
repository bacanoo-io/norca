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
import { seedNetwork } from 'nomad-ui/mirage/norca/network';
import { useToken, lastBody, lastRequest } from 'nomad-ui/tests/helpers/norca';

module('Acceptance | services and network', function (hooks) {
  setupApplicationTest(hooks);
  setupMirage(hooks);

  hooks.beforeEach(function () {
    window.localStorage.clear();
    server.create('agent');
    server.create('node-pool');
    server.create('node');
  });

  test('the catalog shows the health of every service and what discovery leaves out', async function (assert) {
    useToken(null, 'management');
    seedNetwork(server);
    await visit('/services');

    assert.dom('[data-test-page-title]').hasText('Services');
    assert.dom('[data-test-page-pretitle]').hasText('Services and networking');
    assert.dom('[data-test-nav-group="Services and networking"]').exists();
    assert
      .dom('[data-test-service-catalog] [data-test-ui-table-row]')
      .exists({ count: 3 });
    assert
      .dom('[data-test-stat="passing"] [data-test-stat-value]')
      .hasText('4');
    assert
      .dom('[data-test-stat="excluded"] [data-test-stat-value]')
      .hasText('1');
    assert.dom('[data-test-mtls-root]').includesText('norca.local');
    const api = [
      ...document.querySelectorAll(
        '[data-test-service-catalog] [data-test-ui-table-row]'
      ),
    ].find((r) => r.textContent.includes('api'));
    assert
      .dom(api.querySelector('[data-test-service-health]'))
      .includesText('2/3');
    assert
      .dom(api.querySelector('[data-test-service-health]'))
      .includesText('1 left out');
    await a11yAudit(assert, STRICT);

    await fillIn('[data-test-service-filter]', 'primary');
    assert
      .dom('[data-test-service-catalog] [data-test-ui-table-row]')
      .exists({ count: 1 });
  });

  test('a service lists its instances with the checks by role and its intentions', async function (assert) {
    useToken(null, 'management');
    seedNetwork(server);
    await visit('/services');
    await click('[data-test-service-link="api"]');

    assert.strictEqual(currentURL(), '/services/api?namespace=default');
    assert
      .dom('[data-test-service-instances] [data-test-ui-table-row]')
      .exists({ count: 3 });
    assert
      .dom('[data-test-left-out]')
      .includesText('1 of 3 instances are left out');
    assert.dom('[data-test-instance-discovery]').exists();
    assert
      .dom('[data-test-service-intentions] [data-test-ui-table-row]')
      .exists({ count: 2 });
    await a11yAudit(assert, STRICT);
  });

  test('without services the catalog says how they appear', async function (assert) {
    useToken(null, 'management');
    await visit('/services');
    assert
      .dom('[data-test-ui-table-empty]')
      .includesText('service provider is nomad');
    assert.dom('[data-test-mtls-root]').includesText('not enabled');
  });

  test('intentions: graph, list, create, change and delete', async function (assert) {
    useToken(null, 'management');
    seedNetwork(server);
    await visit('/network');

    assert
      .dom('[data-test-page-title]')
      .hasText('Intentions and network rules');
    assert.dom('[data-test-intention-graph] [data-test-chart]').exists();
    assert
      .dom('[data-test-intention-list] [data-test-ui-table-row]')
      .exists({ count: 3 });
    await a11yAudit(assert, STRICT);

    await click('[data-test-add-intention]');
    await a11yAudit(assert, STRICT);
    await click('[data-test-intention-save]');
    assert
      .dom('[data-test-intention-save-error]')
      .includesText('source and a destination');
    await fillIn('[data-test-intention-source-input]', 'worker');
    await fillIn('[data-test-intention-destination-input]', 'api');
    await select('[data-test-intention-decision]', 'deny');
    await click('[data-test-intention-save]');
    let body = lastBody('PUT', '/v1/service-intention');
    assert.strictEqual(body.Source, 'worker');
    assert.strictEqual(body.Destination, 'api');
    assert.strictEqual(body.Action, 'deny');
    assert
      .dom('[data-test-intention-list] [data-test-ui-table-row]')
      .exists({ count: 4 });

    const key = 'default/postgres/default/worker';
    await click(`[data-test-edit-intention="${key}"]`);
    assert.dom('[data-test-intention-source-input]').hasAttribute('readonly');
    await select('[data-test-intention-decision]', 'allow');
    await click('[data-test-intention-save]');
    body = lastBody('PUT', '/v1/service-intention');
    assert.strictEqual(body.Action, 'allow');

    await click(`[data-test-delete-intention="${key}"]`);
    await click('[data-test-confirm-submit]');
    const request = lastRequest('DELETE', '/v1/service-intention');
    assert.ok(request.url.includes('source=worker'));
    assert
      .dom('[data-test-intention-list] [data-test-ui-table-row]')
      .exists({ count: 3 });
  });

  test('a token without write-intention cannot change intentions', async function (assert) {
    useToken({
      Namespaces: [{ Name: '*', Capabilities: ['read-job', 'list-jobs'] }],
    });
    seedNetwork(server);
    await visit('/network');
    assert
      .dom('[data-test-intention-list] [data-test-ui-table-row]')
      .exists({ count: 3 });
    assert.dom('[data-test-add-intention]').doesNotExist();
    assert.dom('[data-test-edit-intention]').doesNotExist();
  });

  test('network policies: the list, the rules and where they are enforced', async function (assert) {
    useToken(null, 'management');
    seedNetwork(server);
    await visit('/network?tab=policies');

    assert
      .dom('[data-test-network-policies="namespace"] [data-test-ui-table-row]')
      .exists({ count: 1 });
    assert.dom('[data-test-policy-rules]').includesText('1 ingress, 0 egress');
    await click('[data-test-policy-link="db-ingress"]');
    assert.strictEqual(currentURL(), '/network/policies/default/db-ingress');
    assert.dom('[data-test-policy-details]').includesText('app=postgres');
    assert.dom('[data-test-policy-rules-table]').includesText('5432/tcp');
    assert
      .dom('[data-test-policy-status]')
      .includesText('1 selected allocation is not filtered');
    assert.dom('[data-test-policy-status]').includesText('host network mode');
    await a11yAudit(assert, STRICT);

    await click('[data-test-delete-policy]');
    await fillIn('[data-test-confirm-typed]', 'db-ingress');
    await click('[data-test-confirm-submit]');
    assert.strictEqual(currentURL(), '/network?tab=policies');
    assert
      .dom('[data-test-network-policies] [data-test-ui-table-empty]')
      .exists();
  });

  test('admin network policies are listed by priority', async function (assert) {
    useToken(null, 'management');
    seedNetwork(server);
    await visit('/network');
    await click('[data-test-tab="admin"]');
    assert.strictEqual(currentURL(), '/network?tab=admin');
    assert
      .dom('[data-test-network-policies="admin"] [data-test-ui-table-row]')
      .exists({ count: 1 });
    await click('[data-test-policy-link="block-metadata"]');
    assert
      .dom('[data-test-policy-rules-table]')
      .includesText('169.254.169.254/32');
    assert.dom('[data-test-policy-details]').includesText('best effort');
  });
});
