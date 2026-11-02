/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

/* eslint-disable qunit/require-expect */
import { click, currentURL, fillIn, visit } from '@ember/test-helpers';
import { module, test } from 'qunit';
import { setupApplicationTest } from 'ember-qunit';
import { setupMirage } from 'ember-cli-mirage/test-support';
import a11yAudit, { STRICT } from 'nomad-ui/tests/helpers/a11y-audit';
import { seedQuotas } from 'nomad-ui/mirage/norca/quotas';
import { useToken, lastRequest } from 'nomad-ui/tests/helpers/norca';

module('Acceptance | quotas', function (hooks) {
  setupApplicationTest(hooks);
  setupMirage(hooks);

  hooks.beforeEach(function () {
    window.localStorage.clear();
    server.create('agent');
    server.create('node-pool');
    server.create('node');
    server.create('namespace', { id: 'prod', name: 'prod', quota: 'team-a' });
  });

  test('usage bars per quota, the namespaces using it and nearly full limits', async function (assert) {
    useToken(null, 'management');
    seedQuotas(server);
    await visit('/quotas');

    assert.dom('[data-test-page-title]').hasText('Quotas');
    assert.dom('[data-test-page-pretitle]').hasText('Capacity');
    assert.dom('[data-test-quota-card]').exists({ count: 2 });
    assert
      .dom('[data-test-quota-card="team-a"] [data-test-quota-usage="cpu"]')
      .includesText('28,400 MHz of 40,000 MHz (71%)');
    assert
      .dom(
        '[data-test-quota-card="team-a"] [data-test-quota-usage="variables"]'
      )
      .includesText('no limit');
    assert
      .dom('[data-test-quota-card="team-a"] [data-test-quota-namespaces]')
      .includesText('prod');
    assert
      .dom('[data-test-quota-card="team-b"] [data-test-nearly-full]')
      .exists();
    assert
      .dom('[data-test-quota-card="team-a"] [data-test-nearly-full]')
      .doesNotExist();
    assert.dom('[data-test-gutter-link="quotas"]').exists();
    await a11yAudit(assert, STRICT);
  });

  test('evaluations blocked by a quota are listed with the exhausted dimensions', async function (assert) {
    useToken(null, 'management');
    seedQuotas(server);
    const job = server.create('job', {
      id: 'report-export',
      createAllocations: false,
      shallow: true,
    });
    server.create('evaluation', {
      jobId: job.id,
      status: 'blocked',
      quotaLimitReached: 'team-b',
      failedTGAllocs: { run: { QuotaExhausted: ['cpu'] } },
    });
    await visit('/quotas');

    assert
      .dom('[data-test-quota-blocked] [data-test-ui-table-row]')
      .exists({ count: 1 });
    assert.dom('[data-test-blocked-dimensions]').hasText('cpu');
    await visit('/');
    assert.dom('[data-test-warnings]').includesText('blocked by quota team-b');
    assert
      .dom('[data-test-warnings]')
      .includesText('Quota team-b is nearly full');
  });

  test('without quotas the empty state says how to create one', async function (assert) {
    useToken(null, 'management');
    await visit('/quotas');
    assert
      .dom('[data-test-quotas-empty]')
      .includesText('set it as the quota of a namespace');
    await click('[data-test-quotas-empty] a');
    assert.strictEqual(currentURL(), '/quotas/new');
  });

  test('a quota is created with per-region limits from the form', async function (assert) {
    useToken(null, 'management');
    await visit('/quotas/new');
    await a11yAudit(assert, STRICT);

    await click('[data-test-quota-save]');
    assert.dom('[data-test-field-error]').includesText('letters, digits');

    await fillIn('[data-test-quota-name]', 'team-c');
    await fillIn('[data-test-quota-description]', 'Team C');
    await fillIn('[data-test-limit-input="cpu"]', '10000');
    await fillIn('[data-test-limit-input="allocations"]', '-1');
    await fillIn('[data-test-task-max-cpu]', '2000');
    await click('[data-test-quota-save]');

    const body = JSON.parse(lastRequest('PUT', '/v1/quota/team-c').requestBody);
    assert.strictEqual(body.Name, 'team-c');
    assert.strictEqual(body.Limits[0].RegionLimit.CPU, 10000);
    assert.strictEqual(body.Limits[0].Allocations, -1);
    assert.strictEqual(body.TaskMax.CPU, 2000);
    assert.strictEqual(currentURL(), '/quotas/team-c');
    assert
      .dom('[data-test-quota-usage="allocations"]')
      .includesText('not allowed');
  });

  test('editing keeps the name and saves the changed limit', async function (assert) {
    useToken(null, 'management');
    seedQuotas(server);
    await visit('/quotas/team-a');
    await click('[data-test-edit-quota]');
    assert.dom('[data-test-quota-name]').hasAttribute('readonly');
    await fillIn('[data-test-limit-input="cpu"]', '50000');
    await click('[data-test-quota-save]');
    const body = JSON.parse(lastRequest('PUT', '/v1/quota/team-a').requestBody);
    assert.strictEqual(body.Limits[0].RegionLimit.CPU, 50000);
    assert.strictEqual(body.Limits[0].RegionLimit.MemoryMB, 81920);
  });

  test('the details show limits, task limits and namespaces; a used quota cannot be deleted', async function (assert) {
    useToken(null, 'management');
    seedQuotas(server);
    await visit('/quotas/team-a');

    assert.dom('[data-test-quota-limit]').exists({ count: 1 });
    assert.dom('[data-test-quota-task-limits]').includesText('8000 MHz CPU');
    assert.dom('[data-test-quota-used-by]').includesText('prod');
    await a11yAudit(assert, STRICT);

    await click('[data-test-delete-quota]');
    await fillIn('[data-test-confirm-typed]', 'team-a');
    await click('[data-test-confirm-submit]');
    assert.dom('[data-test-confirm-error]').includesText('in use by namespace');
    await click('[data-test-confirm-cancel]');

    await visit('/quotas/team-b');
    await click('[data-test-delete-quota]');
    await fillIn('[data-test-confirm-typed]', 'team-b');
    await click('[data-test-confirm-submit]');
    assert.strictEqual(currentURL(), '/quotas');
    assert.dom('[data-test-quota-card]').exists({ count: 1 });
  });

  test('a token with quota read sees the usage but cannot change quotas', async function (assert) {
    useToken({ Quota: { Policy: 'read' } });
    seedQuotas(server);
    await visit('/quotas');
    assert.dom('[data-test-quota-card]').exists({ count: 2 });
    assert.dom('[data-test-new-quota]').doesNotExist();
    await visit('/quotas/team-a');
    assert.dom('[data-test-edit-quota]').doesNotExist();
    assert.dom('[data-test-delete-quota]').doesNotExist();
  });
});
