/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import { module, test } from 'qunit';
import { setupRenderingTest } from 'ember-qunit';
import { render } from '@ember/test-helpers';
import hbs from 'htmlbars-inline-precompile';
import startMirage from 'ember-cli-mirage/start-mirage';
import { Response } from 'miragejs';
import { componentA11yAudit } from 'nomad-ui/tests/helpers/a11y-audit';

module('Integration | Component | Metrics store status', function (hooks) {
  setupRenderingTest(hooks);

  hooks.beforeEach(function () {
    this.server = startMirage(this.owner);
    this.servers = [
      { name: 'server-1.global', region: 'global' },
      { name: 'server-2.global', region: 'global' },
      { name: 'server-3.global', region: 'global' },
    ];
  });

  hooks.afterEach(function () {
    this.server.shutdown();
  });

  const requests = (server) =>
    server.pretender.handledRequests
      .map((r) => r.url)
      .filter((url) => url.includes('/v1/operator/metrics-store'));

  test('every server is asked for its store and shown', async function (assert) {
    assert.expect(9);
    await render(hbs`<Metrics::StoreStatus @servers={{this.servers}} />`);

    assert.dom('[data-test-store-status]').exists();
    assert.dom('[data-test-store-status-server]').exists({ count: 3 });
    assert
      .dom('[data-test-store-status-retention]')
      .hasText('30d', 'the retention in days');
    assert
      .dom('[data-test-store-status-disk]')
      .includesText('300 MiB of 4 GiB');
    assert
      .dom('[data-test-store-status-hosts]')
      .includesText('7d')
      .includesText('40 MiB of 1 GiB');
    assert.dom('[data-test-store-status-differs]').doesNotExist();
    const urls = requests(this.server);
    assert.ok(
      urls.some(
        (url) =>
          url.includes('server_id=server-2.global') &&
          url.includes('region=global')
      ),
      'each server is read by server_id'
    );
    await componentA11yAudit(this.element, assert);
  });

  test('a server with another retention or disk cap is marked', async function (assert) {
    this.server.metricsStoreStatusOf = (name) =>
      name === 'server-2.global'
        ? { Retention: 7 * 86400e9, MaxDiskSize: 2 * 1024 ** 3 }
        : {};
    await render(hbs`<Metrics::StoreStatus @servers={{this.servers}} />`);

    assert
      .dom('[data-test-store-status-differs]')
      .includesText('server-2.global')
      .includesText('retention 7d, the other servers 30d')
      .includesText('max_disk_size 2 GiB, the other servers 4 GiB');
  });

  test('a server with another hosts store is marked', async function (assert) {
    this.server.metricsStoreStatusOf = (name) =>
      name === 'server-3.global'
        ? {
            Hosts: {
              Retention: 86400e9,
              MaxDiskSize: 512 * 1024 ** 2,
              DiskBytes: 0,
            },
          }
        : {};
    await render(hbs`<Metrics::StoreStatus @servers={{this.servers}} />`);

    assert
      .dom('[data-test-store-status-differs]')
      .includesText('server-3.global')
      .includesText('host_retention 1d, the other servers 7d')
      .includesText('host_max_disk_size 512 MiB, the other servers 1 GiB');
  });

  test('a store dropping task groups is reported', async function (assert) {
    this.server.metricsStoreStatusOf = (name) =>
      name === 'server-1.global'
        ? { Series: 50000, MaxSeries: 50000, DroppedGroups: 3 }
        : {};
    await render(hbs`<Metrics::StoreStatus @servers={{this.servers}} />`);

    assert
      .dom('[data-test-store-status-dropping]')
      .includesText('3 task groups left out');
  });

  test('a disabled store and an unreachable server are rows of their own', async function (assert) {
    this.server.metricsStoreStatusOf = (name) => {
      if (name === 'server-1.global') return { Enabled: false };
      if (name === 'server-3.global') {
        return new Response(500, {}, 'no path to server');
      }
      return {};
    };
    await render(hbs`<Metrics::StoreStatus @servers={{this.servers}} />`);

    assert.dom('[data-test-store-status-disabled]').exists({ count: 1 });
    assert.dom('[data-test-store-status-error]').hasText('no path to server');
  });

  test('without operator read the card is left out', async function (assert) {
    this.server.get(
      '/operator/metrics-store',
      () => new Response(403, {}, 'Permission denied')
    );
    await render(hbs`<Metrics::StoreStatus @servers={{this.servers}} />`);

    assert.dom('[data-test-store-status]').doesNotExist();
  });
});
