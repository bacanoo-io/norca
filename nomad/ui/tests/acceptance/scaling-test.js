/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

/* eslint-disable qunit/require-expect */
import { click, currentURL, select, visit } from '@ember/test-helpers';
import { module, test } from 'qunit';
import { setupApplicationTest } from 'ember-qunit';
import { setupMirage } from 'ember-cli-mirage/test-support';
import a11yAudit, { STRICT } from 'nomad-ui/tests/helpers/a11y-audit';
import { seedScaling } from 'nomad-ui/mirage/norca/scaling';
import { useToken, chartTexts } from 'nomad-ui/tests/helpers/norca';

module('Acceptance | scaling', function (hooks) {
  setupApplicationTest(hooks);
  setupMirage(hooks);

  hooks.beforeEach(function () {
    window.localStorage.clear();
    server.create('agent');
    server.create('node-pool');
    server.create('node');
    useToken(null, 'management');
  });

  test('the policies with engine, bounds, current count and last decision', async function (assert) {
    seedScaling(server);
    await visit('/scaling');

    assert.dom('[data-test-page-title]').hasText('Scaling');
    assert.dom('[data-test-page-pretitle]').hasText('Capacity');
    assert.dom('[data-test-gutter-link="scaling"]').exists();
    assert
      .dom('[data-test-scaling-policies] [data-test-ui-table-row]')
      .exists({ count: 2 });
    const rows = [
      ...document.querySelectorAll(
        '[data-test-scaling-policies] [data-test-ui-table-row]'
      ),
    ];
    const api = rows.find((r) => r.textContent.includes('api'));
    const worker = rows.find((r) => r.textContent.includes('worker'));
    assert
      .dom(api.querySelector('[data-test-policy-engine]'))
      .hasText('built-in');
    assert
      .dom(worker.querySelector('[data-test-policy-engine]'))
      .hasText('external');
    assert.dom(api.querySelector('[data-test-policy-bounds]')).hasText('3–20');
    assert
      .dom(api.querySelector('[data-test-policy-current]'))
      .hasText('6 of 6');
    assert
      .dom(api.querySelector('[data-test-policy-last]'))
      .includesText('4 → 6');
    assert.dom(worker).includesText('disabled');
    assert
      .dom('[data-test-stat="builtin"] [data-test-stat-value]')
      .hasText('1');
    assert
      .dom('[data-test-scaling-timeline] [data-test-timeline-item]')
      .exists({ count: 3 });
    assert.dom('[data-test-scaling-timeline]').includesText('api / web: 4 → 6');
    await a11yAudit(assert, STRICT);
  });

  test('the engine filter shows the policies of one engine', async function (assert) {
    seedScaling(server);
    await visit('/scaling');
    await select('[data-test-engine-filter]', 'external');
    assert
      .dom('[data-test-scaling-policies] [data-test-ui-table-row]')
      .exists({ count: 1 });
    assert.dom('[data-test-scaling-policies]').includesText('worker');
  });

  test('without policies the empty message says how to add one', async function (assert) {
    await visit('/scaling');
    assert
      .dom('[data-test-ui-table-empty]')
      .includesText('Add a scaling block');
    assert.dom('[data-test-scaling-timeline]').includesText('No decisions yet');
  });

  test('the policy details: timing, checks, count chart and decisions', async function (assert) {
    seedScaling(server);
    await visit('/scaling');
    await click('[data-test-policy-link="policy-api-web"]');

    assert.strictEqual(
      currentURL(),
      '/scaling/policy-api-web?namespace=default'
    );
    assert.dom('[data-test-page-title]').hasText('api / web');
    assert
      .dom('[data-test-policy-details]')
      .includesText('built-in (on the leader)');
    assert.dom('[data-test-policy-details]').includesText('30 s');
    assert.dom('[data-test-policy-details]').includesText('2 min');
    assert.dom('[data-test-policy-checks]').includesText('avg_cpu');
    assert
      .dom('[data-test-policy-checks]')
      .includesText('target-value: target=70');
    assert.dom('[data-test-policy-chart] [data-test-chart]').exists();
    const texts = await chartTexts('[data-test-policy-chart]');
    assert.ok(texts.includes('min'), 'the chart marks the minimum count');
    assert.ok(texts.includes('max'), 'the chart marks the maximum count');
    assert
      .dom('[data-test-policy-events] [data-test-ui-table-row]')
      .exists({ count: 3 });
    assert.dom('[data-test-policy-events]').includesText('quota cap');
    assert
      .dom('[data-test-policy-document]')
      .includesText('evaluation_interval');
    await a11yAudit(assert, STRICT);
  });

  test('an unknown policy says what to do', async function (assert) {
    await visit('/scaling/nope?namespace=default');
    assert
      .dom('[data-test-policy-error]')
      .includesText('removed with the scaling block');
  });
});
