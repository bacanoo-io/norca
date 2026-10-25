/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

/* eslint-disable qunit/require-expect */
import {
  currentURL,
  visit,
  click,
  findAll,
  settled,
  waitUntil,
} from '@ember/test-helpers';
import { module, test } from 'qunit';
import { setupApplicationTest } from 'ember-qunit';
import { setupMirage } from 'ember-cli-mirage/test-support';
import a11yAudit from 'nomad-ui/tests/helpers/a11y-audit';
import Layout from 'nomad-ui/tests/pages/layout';

function streamResponse(lines) {
  return Promise.resolve({
    status: 200,
    ok: true,
    body: null,
    text: () =>
      Promise.resolve(lines.map((l) => JSON.stringify(l)).join('\n') + '\n'),
  });
}

module('Acceptance | dashboard', function (hooks) {
  setupApplicationTest(hooks);
  setupMirage(hooks);

  hooks.beforeEach(function () {
    window.localStorage.clear();
    // A healthy cluster: the server is alive, no evaluations are blocked.
    const agent = server.create('agent');
    agent.update({ member: { ...agent.member, Status: 'alive' } });
    server.create('node-pool');
    server.createList('node', 2, { status: 'ready' });
    server.createList('job', 2, {
      createAllocations: false,
      noDeployments: true,
      shallow: true,
    });
  });

  test('the root URL shows the dashboard instead of redirecting to the jobs list', async function (assert) {
    await visit('/');
    assert.strictEqual(currentURL(), '/');
    assert.dom('[data-test-page-title]').hasText('Dashboard');
    assert.dom('[data-test-page-pretitle]').hasText('Overview');
    assert.ok(document.title.includes('Dashboard'));
    await a11yAudit(assert);
  });

  test('the federation strip has a column for the region with leader, servers and clients', async function (assert) {
    await visit('/');
    assert.dom('[data-test-federation-region]').exists({ count: 1 });
    assert.dom('[data-test-region-servers]').hasText(/^\d+\/1 alive$/);
    assert.dom('[data-test-region-clients]').includesText('2/2 ready');
    assert.dom('[data-test-region-leader]').doesNotHaveTextContaining('none');
  });

  test('a down client and a blocked evaluation become warnings that say what to do', async function (assert) {
    server.create('node', { status: 'down' });
    server.create('evaluation', { status: 'blocked' });
    await visit('/');

    assert.dom('[data-test-warning]').exists({ count: 2 });
    assert.dom('[data-test-warnings]').includesText('client is down');
    assert.dom('[data-test-warnings]').includesText('rescheduled');
    assert.dom('[data-test-warnings]').includesText('evaluation is blocked');
    assert
      .dom('[data-test-stat="blocked-evaluations"] [data-test-stat-value]')
      .hasText('1');
  });

  test('without problems the warnings card says that nothing needs attention', async function (assert) {
    await visit('/');
    assert.deepEqual(
      findAll('[data-test-warning]').map((el) => el.textContent.trim()),
      [],
      'no warnings'
    );
    assert.dom('[data-test-no-warnings]').exists();
  });

  test('with several regions, "All regions" shows one column per region', async function (assert) {
    server.create('region', { id: 'eu-west' });
    server.create('region', { id: 'us-east' });
    window.localStorage.norcaRegionScope = '*';

    await visit('/');
    assert.dom('[data-test-federation-region]').exists({ count: 2 });
    assert.dom('[data-test-federation-region="eu-west"]').exists();
    assert.dom('[data-test-federation-region="us-east"]').exists();
    assert.dom('[data-test-page-subtitle]').includesText('All regions');
    assert.strictEqual(Layout.navbar.regionSwitcher.value, '*');
  });

  test('choosing a region in the top bar scopes the dashboard to it', async function (assert) {
    server.create('region', { id: 'eu-west' });
    server.create('region', { id: 'us-east' });
    window.localStorage.norcaRegionScope = '*';

    await visit('/');
    await Layout.navbar.regionSwitcher.select('us-east');
    assert.strictEqual(currentURL(), '/?region=us-east');
    assert.dom('[data-test-federation-region]').exists({ count: 1 });
    assert.dom('[data-test-federation-region="us-east"]').exists();
  });

  test('the dashboard subscribes to the event stream, shows the events and refreshes', async function (assert) {
    const eventStream = this.owner.lookup('service:event-stream');
    const urls = [];
    eventStream.enabled = true;
    let served = false;
    eventStream.request = (url) => {
      urls.push(url);
      if (served) return new Promise(() => {});
      served = true;
      return streamResponse([
        {
          Index: 100,
          Events: [
            {
              Topic: 'Deployment',
              Type: 'DeploymentStatusUpdate',
              Key: 'd1',
              Index: 100,
              Payload: {
                Deployment: {
                  ID: 'd1',
                  JobID: 'web',
                  Namespace: 'default',
                  Status: 'failed',
                  StatusDescription: 'Failed due to unhealthy allocations',
                },
              },
            },
          ],
        },
      ]);
    };

    await visit('/');
    await waitUntil(() =>
      document.querySelector('[data-test-events] [data-test-timeline-item]')
    );

    assert.ok(
      urls[0].startsWith('/v1/event/stream?'),
      'it connects to the event stream'
    );
    assert.ok(
      urls[0].includes('topic=Deployment'),
      'with the topics of the dashboard'
    );
    assert.ok(
      urls[0].includes('index=1'),
      'replaying the buffered events first'
    );
    assert.dom('[data-test-events]').includesText('Deployment of web failed');

    await Layout.navbar.notifications.open();
    assert.ok(
      Layout.navbar.notifications.items.length >= 1,
      'the failure is a notification'
    );

    for (const { connection } of eventStream.connections.values())
      connection.stop();
    await settled();
  });

  test('the dashboard links to the run job page', async function (assert) {
    const managementToken = server.create('token');
    window.localStorage.nomadTokenSecret = managementToken.secretId;

    await visit('/');
    await click('[data-test-dashboard-run-job]');
    assert.strictEqual(currentURL(), '/jobs/run');
  });
});
