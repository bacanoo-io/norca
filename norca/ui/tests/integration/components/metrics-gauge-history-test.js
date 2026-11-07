/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import { module, test } from 'qunit';
import { setupRenderingTest } from 'ember-qunit';
import { render, waitFor } from '@ember/test-helpers';
import hbs from 'htmlbars-inline-precompile';
import startMirage from 'ember-cli-mirage/start-mirage';
import { Response } from 'miragejs';
import {
  sumHistories,
  bandIntervals,
} from 'nomad-ui/components/metrics/gauge-history';
import { formatMetric, formatDuration } from 'nomad-ui/utils/metrics-format';
import { componentA11yAudit } from 'nomad-ui/tests/helpers/a11y-audit';

const NODE_METRICS = [
  'norca.node.cpu_used',
  'norca.node.memory_used_mb',
  'norca.node.memory_total_mb',
  'norca.node.disk_used_mb',
  'norca.node.disk_total_mb',
  'norca.node.inodes_used_percent',
];

module('Integration | Component | Metrics gauge history', function (hooks) {
  setupRenderingTest(hooks);

  hooks.beforeEach(function () {
    this.server = startMirage(this.owner);
    this.node = {
      id: 'node-1',
      datacenter: 'dc1',
      nodePool: 'default',
      nodeClass: 'web',
    };
  });

  hooks.afterEach(function () {
    this.server.shutdown();
  });

  const history = (server) =>
    server.pretender.handledRequests
      .map((r) => r.url)
      .filter((url) => url.includes('/v1/metrics/history'));

  test('the host history of a node shows CPU, memory and disk', async function (assert) {
    assert.expect(5);
    await render(hbs`<Metrics::NodeHistory @node={{this.node}} />`);
    await waitFor('[data-test-gauge-chart="disk"] [data-test-chart]');

    assert.dom('[data-test-node-history]').exists();
    assert.dom('[data-test-gauge-chart]').exists({ count: 3 });
    assert.ok(
      history(this.server).some(
        (url) =>
          url.includes('metric=norca.node.memory_total_mb') &&
          url.includes('node=node-1') &&
          url.includes('dc=dc1') &&
          url.includes('pool=default') &&
          url.includes('class=web')
      ),
      'the series of the node are read with its labels'
    );
    assert
      .dom('[data-test-gauge-chart="memory"] [data-test-chart-summary]')
      .includesText('Total 8 GiB');
    await componentA11yAudit(this.element, assert);
  });

  test('a node without points shows an empty state, not a chart', async function (assert) {
    this.server.metricsWithoutPoints = NODE_METRICS;
    await render(hbs`<Metrics::NodeHistory @node={{this.node}} />`);
    await waitFor('[data-test-gauge-history-empty]');

    assert.dom('[data-test-gauge-history-empty]').exists();
    assert.dom('[data-test-chart]').doesNotExist();
  });

  test('a chart without points says so while the others draw', async function (assert) {
    this.server.metricsWithoutPoints = [
      'norca.node.disk_used_mb',
      'norca.node.disk_total_mb',
      'norca.node.inodes_used_percent',
    ];
    await render(hbs`<Metrics::NodeHistory @node={{this.node}} />`);
    await waitFor('[data-test-gauge-chart="cpu"] [data-test-chart]');

    assert
      .dom('[data-test-gauge-chart="disk"] [data-test-gauge-chart-empty]')
      .hasText('No points in this range.');
    assert.dom('[data-test-gauge-chart="memory"] [data-test-chart]').exists();
  });

  test('without node read the host history is left out', async function (assert) {
    this.server.get(
      '/metrics/history',
      () => new Response(403, {}, 'Permission denied')
    );
    await render(hbs`<Metrics::NodeHistory @node={{this.node}} />`);

    assert.dom('[data-test-node-history]').doesNotExist();
  });

  test('a disabled store says so', async function (assert) {
    this.server.metricsStoreEnabled = false;
    await render(hbs`<Metrics::NodeHistory @node={{this.node}} />`);
    await waitFor('[data-test-gauge-history-disabled]');
    assert.dom('[data-test-gauge-history-disabled]').exists();
  });

  test('the history of a server is read from that server', async function (assert) {
    this.server.metricsRetention = 7 * 86400e9;
    this.agent = { name: 'server-2.global', region: 'global' };
    await render(hbs`<Metrics::ServerHistory @server={{this.agent}} />`);
    await waitFor('[data-test-gauge-chart="bus-streams"] [data-test-chart]');

    assert.dom('[data-test-gauge-chart]').exists({ count: 4 });
    const urls = history(this.server);
    const named = urls.filter(
      (url) =>
        url.includes('server_id=server-2.global') &&
        url.includes('region=global')
    );
    assert.ok(urls.length > 0, 'the history is read');
    assert.strictEqual(
      named.length,
      urls.length,
      'every history request names the server'
    );
    assert.ok(urls.some((url) => url.includes('metric=norca.bus.routes')));
    assert
      .dom('[data-test-tab="30d"]')
      .doesNotExist('the retention of that server limits the ranges');
  });

  test('the node group history shows the nodes, the rollout and the conditions', async function (assert) {
    await render(hbs`<Metrics::NodeGroupHistory @name="workers" />`);
    await waitFor('[data-test-gauge-chart="conditions"] [data-test-chart]');

    assert.dom('[data-test-gauge-chart]').exists({ count: 3 });
    const urls = history(this.server);
    assert.ok(
      urls.some(
        (url) =>
          url.includes('metric=norca.nodegroup.condition') &&
          url.includes('group=workers') &&
          url.includes('type=RollBlocked')
      ),
      'a condition type of the series list is read'
    );
    assert
      .dom('[data-test-gauge-chart="conditions"] [data-test-chart-summary]')
      .includesText('RollBlocked')
      .doesNotIncludeText('JoinFailing');
    assert
      .dom('[data-test-gauge-chart="nodes"] [data-test-chart-summary]')
      .includesText('Wanted 3')
      .includesText('Ready 3');
  });

  test('the load balancer history shows its targets and readiness', async function (assert) {
    await render(
      hbs`<Metrics::LoadBalancerHistory @namespace="infra" @name="ingress" />`
    );
    await waitFor('[data-test-gauge-chart="ready"] [data-test-chart]');

    assert.dom('[data-test-gauge-chart]').exists({ count: 2 });
    assert.ok(
      history(this.server).some(
        (url) =>
          url.includes('metric=norca.lb.targets') &&
          url.includes('ns=infra') &&
          url.includes('name=ingress')
      )
    );
    assert
      .dom('[data-test-gauge-chart="ready"] [data-test-chart-summary]')
      .includesText('Ready 100%');
  });

  test('the control plane reads the leader and adds up the namespaces', async function (assert) {
    const leader = this.server.create('agent');
    await render(hbs`<Metrics::ControlPlaneHistory />`);
    await waitFor('[data-test-gauge-chart="jobs"] [data-test-chart]');

    assert.dom('[data-test-gauge-chart="scheduler"]').exists();
    const urls = history(this.server);
    assert.ok(
      urls.some(
        (url) =>
          url.includes('metric=norca.sched.broker_ready') &&
          url.includes(`server_id=${encodeURIComponent(leader.name)}`)
      ),
      'the scheduler series are read from the leader'
    );
    assert.ok(
      urls.some(
        (url) =>
          url.includes('metric=norca.jobs.running') && url.includes('ns=prod')
      )
    );
    assert
      .dom('[data-test-gauge-chart="jobs"]')
      .includesText('Jobs in 2 namespaces');
    assert
      .dom('[data-test-gauge-chart="jobs"] [data-test-chart-summary]')
      .includesText('Running 8', 'the running jobs of both namespaces');
  });

  test('a part the token may not read is left out', async function (assert) {
    this.server.get('/metrics/history', function (schema, { queryParams }) {
      if (queryParams.metric.startsWith('norca.sched.')) {
        return new Response(403, {}, 'Permission denied');
      }
      return {
        Enabled: true,
        Metric: queryParams.metric,
        Points: [{ Time: new Date().toISOString(), Avg: 1, Max: 1 }],
      };
    });
    await render(hbs`<Metrics::ControlPlaneHistory />`);
    await waitFor('[data-test-gauge-chart="jobs"]');

    assert.dom('[data-test-gauge-chart="scheduler"]').doesNotExist();
    assert.dom('[data-test-gauge-chart="jobs"]').exists();
  });

  test('the helpers', function (assert) {
    const t1 = '2026-11-07T10:00:00Z';
    const t2 = '2026-11-07T10:00:30Z';
    assert.deepEqual(
      sumHistories([
        { Enabled: true, Points: [{ Time: t1, Avg: 1, Max: 2 }] },
        {
          Enabled: true,
          Points: [
            { Time: t1, Avg: 3, Max: 4 },
            { Time: t2, Avg: 5, Max: 5 },
          ],
        },
      ]).Points,
      [
        { Time: t1, Avg: 4, Max: 6 },
        { Time: t2, Avg: 5, Max: 5 },
      ]
    );
    const start = Date.parse(t1);
    assert.deepEqual(
      bandIntervals(
        {
          Step: 30e9,
          Points: [
            { Time: t1, Avg: 0.5, Max: 1 },
            { Time: t2, Avg: 1, Max: 1 },
            { Time: '2026-11-07T10:01:00Z', Avg: 0, Max: 0 },
            { Time: '2026-11-07T10:01:30Z', Avg: 1, Max: 1 },
          ],
        },
        2
      ),
      [
        [2, start, start + 60e3],
        [2, start + 90e3, start + 120e3],
      ]
    );
    assert.strictEqual(formatMetric(8192, 'mib'), '8 GiB');
    assert.strictEqual(formatMetric(0.05, 'seconds'), '50 ms');
    assert.strictEqual(formatMetric(0.75, 'ratio'), '75%');
    assert.strictEqual(formatDuration(720 * 3600e9), '30d');
    assert.strictEqual(formatDuration(36 * 3600e9), '1d 12h');
    assert.strictEqual(formatDuration(2 * 3600e9), '2h');
  });
});
