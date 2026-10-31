/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import { module, test } from 'qunit';
import { setupRenderingTest } from 'ember-qunit';
import { click, render, waitFor } from '@ember/test-helpers';
import hbs from 'htmlbars-inline-precompile';
import startMirage from 'ember-cli-mirage/start-mirage';
import { Response } from 'miragejs';
import { recommend, points } from 'nomad-ui/utils/metrics-history';
import { componentA11yAudit } from 'nomad-ui/tests/helpers/a11y-audit';

module('Integration | Component | Metrics history', function (hooks) {
  setupRenderingTest(hooks);

  hooks.beforeEach(function () {
    this.server = startMirage(this.owner);
  });

  hooks.afterEach(function () {
    this.server.shutdown();
  });

  const taskGroup = hbs`
    <Metrics::TaskGroupHistory @namespace="default" @job="api" @group="web"
      @reservedCPU={{500}} @reservedMemory={{256}} />
  `;

  const requests = (server, path) =>
    server.pretender.handledRequests
      .map((r) => r.url)
      .filter((url) => url.includes(path));

  test('Must pass an accessibility audit', async function (assert) {
    assert.expect(1);
    await render(taskGroup);
    await waitFor('[data-test-usage-cpu]');
    await componentA11yAudit(this.element, assert);
  });

  test('the task group history shows CPU, memory, OOM kills and right-sizing', async function (assert) {
    await render(taskGroup);
    await waitFor('[data-test-usage-cpu]');

    assert.dom('[data-test-usage-cpu]').exists();
    assert.dom('[data-test-usage-memory]').exists();
    assert.dom('[data-test-usage-oom]').hasText('1 OOM kills');
    assert
      .dom('[data-test-usage-suggested="cpu"]')
      .includesText('400 MHz', 'the CPU peak with 20% headroom')
      .includesText('20% less');
    assert
      .dom('[data-test-usage-suggested="memory"]')
      .includesText('256 MiB')
      .includesText('about right');

    const history = requests(this.server, '/v1/metrics/history');
    assert.ok(
      history.some(
        (url) =>
          url.includes('metric=norca.group.cpu_mhz') &&
          url.includes('job=api') &&
          url.includes('group=web') &&
          url.includes('from=1h')
      ),
      'the CPU history of the group is read for the last hour'
    );

    await click('[data-test-tab="7d"]');

    assert.ok(
      requests(this.server, '/v1/metrics/history').some((url) =>
        url.includes('from=168h')
      ),
      'a range of 7 days is read after selecting it'
    );
    assert.ok(
      requests(this.server, '/v1/metrics/summary').some((url) =>
        url.includes('window=604800s')
      ),
      'the right-sizing covers the range'
    );
  });

  test('the task group history explains a disabled store', async function (assert) {
    this.server.metricsStoreEnabled = false;
    await render(taskGroup);
    await waitFor('[data-test-usage-disabled]');
    assert
      .dom('[data-test-usage-disabled]')
      .includesText('metrics_store.enabled = false');
  });

  test('the task group history explains a denied token', async function (assert) {
    this.server.get(
      '/metrics/history',
      () => new Response(403, {}, 'Permission denied')
    );
    await render(taskGroup);
    await waitFor('[data-test-usage-forbidden]');
    assert.dom('[data-test-usage-forbidden]').exists();
    assert.dom('[data-test-usage-cpu]').doesNotExist();
  });

  test('the scaling history shows usage against running allocations', async function (assert) {
    await render(
      hbs`<Metrics::ScalingHistory @namespace="default" @job="api" @group="web" />`
    );
    await waitFor('[data-test-scaling-history-chart]');
    assert.dom('[data-test-scaling-history-chart]').exists();
    assert.ok(
      requests(this.server, '/v1/metrics/history').some((url) =>
        url.includes('metric=norca.group.allocs_running')
      )
    );
  });

  test('the cluster capacity offers the datacenters and node pools', async function (assert) {
    await render(hbs`<Metrics::ClusterHistory />`);
    await waitFor('[data-test-cluster-history-chart]');
    assert.dom('[data-test-cluster-history-scope]').exists();
    assert.ok(
      requests(this.server, '/v1/metrics/history').some(
        (url) =>
          url.includes('metric=norca.cluster.cpu_allocated') &&
          url.includes('dc=dc1') &&
          url.includes('pool=default')
      )
    );
  });

  test('the cluster capacity is left out without node read access', async function (assert) {
    this.server.get(
      '/metrics/series',
      () => new Response(403, {}, 'Permission denied')
    );
    await render(hbs`<Metrics::ClusterHistory />`);

    assert.dom('[data-test-cluster-history]').doesNotExist();
  });

  test('the helpers', function (assert) {
    assert.strictEqual(recommend(300, 50), 400);
    assert.strictEqual(recommend(0, 50), 0);
    assert.deepEqual(
      points({
        Points: [{ Time: '2026-10-31T10:00:00Z', Avg: 1.234, Max: 2 }],
      }),
      [[Date.parse('2026-10-31T10:00:00Z'), 1.23]]
    );
  });
});
