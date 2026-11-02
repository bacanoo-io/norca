/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

/* eslint-disable qunit/require-expect */
import { click, currentURL, visit } from '@ember/test-helpers';
import { module, test } from 'qunit';
import { setupApplicationTest } from 'ember-qunit';
import { setupMirage } from 'ember-cli-mirage/test-support';
import a11yAudit, { STRICT } from 'nomad-ui/tests/helpers/a11y-audit';
import {
  seedDeployments,
  MULTIREGION,
} from 'nomad-ui/mirage/norca/deployments';
import { useToken, lastRequest } from 'nomad-ui/tests/helpers/norca';

module('Acceptance | deployments', function (hooks) {
  setupApplicationTest(hooks);
  setupMirage(hooks);

  hooks.beforeEach(function () {
    window.localStorage.clear();
    server.create('agent');
    server.create('node-pool');
    server.create('node');
    ['eu-west', 'us-east', 'ap-south'].forEach((id) =>
      server.create('region', { id })
    );
    server.create('job', {
      id: 'api',
      createAllocations: false,
      noDeployments: true,
      shallow: true,
      multiregion: MULTIREGION,
    });
    seedDeployments(server);
  });

  test('the multiregion rollout shows every region in order with its state', async function (assert) {
    useToken(null, 'management');
    await visit('/deployments?region=eu-west');

    assert.dom('[data-test-page-title]').hasText('Deployments');
    assert.dom('[data-test-page-pretitle]').hasText('Workloads');
    assert.dom('[data-test-rollout-card="api"]').exists();
    const steps = [
      ...document.querySelectorAll('[data-test-rollout-step]'),
    ].map((s) => s.dataset.testRolloutStep);
    assert.deepEqual(steps, ['eu-west', 'us-east', 'ap-south']);
    assert
      .dom('[data-test-rollout-step="eu-west"] [data-test-step-status]')
      .hasText('blocked');
    assert
      .dom('[data-test-rollout-step="us-east"] [data-test-step-status]')
      .hasText('running');
    assert
      .dom('[data-test-rollout-step="ap-south"] [data-test-step-description]')
      .includesText('waiting for peer region');
    assert
      .dom('[data-test-rollout-strategy]')
      .includesText('1 region at a time; on failure every region fails');
    assert.dom('[data-test-step-action="eu-west:unblock"]').exists();
    assert.dom('[data-test-step-action="us-east:promote"]').exists();
    assert.dom('[data-test-step-action="ap-south:run"]').exists();
    await a11yAudit(assert, STRICT);
  });

  test('the actions go to the region of each deployment', async function (assert) {
    useToken(null, 'management');
    await visit('/deployments?region=eu-west');

    await click('[data-test-step-action="ap-south:run"]');
    let request = lastRequest('POST', '/v1/deployment/run/');
    assert.ok(request.url.includes('region=ap-south'));
    assert.strictEqual(
      JSON.parse(request.requestBody).DeploymentID,
      'd-ap-0000-0000-0000-000000000003'
    );
    assert
      .dom('[data-test-rollout-step="ap-south"] [data-test-step-status]')
      .hasText('running');

    await click('[data-test-step-action="us-east:promote"]');
    request = lastRequest('POST', '/v1/deployment/promote/');
    assert.true(JSON.parse(request.requestBody).All);

    await click('[data-test-step-action="eu-west:unblock"]');
    await a11yAudit(assert, STRICT);
    await click('[data-test-confirm-submit]');
    assert.ok(
      lastRequest('POST', '/v1/deployment/unblock/').url.includes(
        'region=eu-west'
      )
    );
    assert
      .dom('[data-test-rollout-step="eu-west"] [data-test-step-status]')
      .hasText('successful');

    await click('[data-test-step-action="us-east:fail"]');
    assert.dom('[data-test-confirm-body]').includesText('every region fails');
    await click('[data-test-confirm-submit]');
    assert
      .dom('[data-test-rollout-step="us-east"] [data-test-step-status]')
      .hasText('failed');
  });

  test('a token without submit-job sees the rollout without actions', async function (assert) {
    useToken({
      Namespaces: [{ Name: '*', Capabilities: ['read-job', 'list-jobs'] }],
    });
    await visit('/deployments?region=eu-west');
    assert.dom('[data-test-rollout-step]').exists({ count: 3 });
    assert.dom('[data-test-step-action]').doesNotExist();
  });

  test('the list shows active deployments and, on request, the finished ones', async function (assert) {
    useToken(null, 'management');
    await visit('/deployments?region=eu-west');
    assert
      .dom('[data-test-deployment-list] [data-test-ui-table-row]')
      .exists({ count: 1 });
    await click('[data-test-show-all] input');
    assert
      .dom('[data-test-deployment-list] [data-test-ui-table-row]')
      .exists({ count: 2 });
    assert.dom('[data-test-deployment-list]').includesText('worker');
  });

  test('a deployment shows its groups, canaries, allocations and the rollout', async function (assert) {
    useToken(null, 'management');
    await visit('/deployments/d-us-0000-0000-0000-000000000002?region=us-east');

    assert.dom('[data-test-page-title]').hasText('api v42');
    assert.dom('[data-test-deployment-status]').hasText('running');
    assert.dom('[data-test-group-canaries]').hasText('2 of 2');
    assert
      .dom('[data-test-deployment-allocations] [data-test-ui-table-row]')
      .exists({ count: 1 });
    assert.dom('[data-test-deployment-allocations]').includesText('canary');
    assert
      .dom('[data-test-deployment-rollout] [data-test-rollout-step]')
      .exists({ count: 3 });
    await a11yAudit(assert, STRICT);
  });

  test('an unknown deployment says why it may be missing', async function (assert) {
    useToken(null, 'management');
    await visit('/deployments/nope?region=eu-west');
    assert
      .dom('[data-test-deployment-error]')
      .includesText('garbage collection');
    await click('[data-test-deployment-error] a');
    assert.ok(currentURL().startsWith('/deployments'));
  });
});
