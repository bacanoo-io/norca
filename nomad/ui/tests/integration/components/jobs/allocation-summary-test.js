/**
 * Copyright (c) HashiCorp, Inc.
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

/* eslint-disable qunit/no-conditional-assertions, qunit/require-expect */
import { module, test } from 'qunit';
import { setupRenderingTest } from 'ember-qunit';
import { click, find, findAll, render } from '@ember/test-helpers';
import hbs from 'htmlbars-inline-precompile';
import { initialize as fragmentSerializerInitializer } from 'nomad-ui/initializers/fragment-serializer';
import { componentA11yAudit } from 'nomad-ui/tests/helpers/a11y-audit';
import startMirage from 'ember-cli-mirage/start-mirage';

module('Integration | Component | jobs/allocation-summary', function (hooks) {
  setupRenderingTest(hooks);

  hooks.beforeEach(function () {
    fragmentSerializerInitializer(this.owner);
    window.localStorage.clear();
    this.store = this.owner.lookup('service:store');
    this.server = startMirage(this.owner);
    this.server.create('namespace');
    this.server.create('node-pool');
  });

  hooks.afterEach(function () {
    this.server.shutdown();
    window.localStorage.clear();
  });

  test('the allocations diagram lists all allocation status figures', async function (assert) {
    this.server.create('job', {
      createAllocations: false,
    });

    await this.store.findAll('job');
    const job = this.store.peekAll('job').get('firstObject');
    this.set('job', job);

    await render(hbs`
      <Jobs::AllocationSummary @summary={{this.job.summary}} @job={{this.job}} />
    `);

    assert.ok(find('[data-test-allocation-status-bar]'), 'Status bar found');

    ['queued', 'starting', 'running', 'complete', 'failed', 'lost'].forEach(
      (status) => {
        assert.equal(
          find(`[data-test-legend-value="${status}"]`).textContent.trim(),
          String(this.get(`job.${status}Allocs`)),
          `${this.get(`job.${status}Allocs`)} are ${status}`
        );
      }
    );

    await componentA11yAudit(this.element, assert);
  });

  test('only the states with allocations have a segment, and the legend links to the allocations of a state', async function (assert) {
    this.server.create('job', {
      createAllocations: false,
    });

    await this.store.findAll('job');
    const job = this.store.peekAll('job').get('firstObject');
    this.set('job', job);

    await render(hbs`
      <Jobs::AllocationSummary @summary={{this.job.summary}} @job={{this.job}} />
    `);

    const states = [
      'queued',
      'starting',
      'running',
      'complete',
      'failed',
      'lost',
    ];
    const withAllocations = states.filter(
      (s) => this.get(`job.${s}Allocs`) > 0
    );

    assert.deepEqual(
      findAll('[data-test-summary-slice]').map((s) =>
        s.getAttribute('data-test-summary-slice')
      ),
      withAllocations,
      'One segment per state with allocations'
    );

    findAll('[data-test-legend-clickable] a').forEach((link) => {
      assert.ok(
        link.getAttribute('href').includes('/allocations'),
        'Legend items link to the allocations'
      );
    });
  });

  test('the children diagram of a parent job lists all children status figures', async function (assert) {
    this.server.create('job', 'periodic', {
      createAllocations: false,
    });

    await this.store.findAll('job');
    const job = this.store.peekAll('job').get('firstObject');
    this.set('job', job);

    await render(hbs`<Jobs::Children @job={{this.job}} />`);

    const summary =
      find('[data-test-children-status-bar]') ||
      find('[data-test-no-children]');
    assert.ok(summary, 'The children summary or its empty state');
    if (find('[data-test-children-status-bar]')) {
      assert.equal(
        find('[data-test-legend-value="pending"]').textContent.trim(),
        String(this.get('job.pendingChildren')),
        'Pending children'
      );
      assert.equal(
        find('[data-test-legend-value="running"]').textContent.trim(),
        String(this.get('job.runningChildren')),
        'Running children'
      );
      assert.equal(
        find('[data-test-legend-value="dead"]').textContent.trim(),
        String(this.get('job.deadChildren')),
        'Dead children'
      );
    }
  });

  test('clicking a segment calls the selection with its state', async function (assert) {
    this.set('data', [
      { key: 'running', label: 'Running', value: 2 },
      { key: 'failed', label: 'Failed', value: 1 },
    ]);
    this.set('selected', null);
    this.set('select', (segment) => this.set('selected', segment.key));

    await render(hbs`
      <Ui::Distribution @label="Allocations" @data={{this.data}} @onSelect={{this.select}} />
    `);

    await click('[data-test-summary-slice="failed"]');
    assert.equal(this.selected, 'failed');
  });
});
