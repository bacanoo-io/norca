/**
 * Copyright (c) HashiCorp, Inc.
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

/* eslint-disable qunit/require-expect */
import { find, findAll, render } from '@ember/test-helpers';
import { module, test } from 'qunit';
import { setupRenderingTest } from 'ember-qunit';
import hbs from 'htmlbars-inline-precompile';
import { componentA11yAudit } from 'nomad-ui/tests/helpers/a11y-audit';
import startMirage from 'ember-cli-mirage/start-mirage';

module('Integration | Component | jobs/tabs', function (hooks) {
  setupRenderingTest(hooks);

  hooks.beforeEach(function () {
    window.localStorage.clear();
    this.server = startMirage(this.owner);
    this.server.createList('namespace', 3);
  });

  hooks.afterEach(function () {
    this.server.shutdown();
    window.localStorage.clear();
  });

  const labels = () =>
    findAll('[data-test-tab]').map((tab) => tab.textContent.trim());

  test('the tabs of a service job include the deployments, scaling, placement and events', async function (assert) {
    const store = this.owner.lookup('service:store');
    const job = store.createRecord('job', {
      id: '["service-job","default"]',
      plainId: 'service-job',
      name: 'service-job',
      type: 'service',
    });
    this.set('job', job);

    await render(hbs`<Jobs::Tabs @job={{this.job}} />`);

    assert.ok(find('[data-test-subnav="job"]'), 'The job tabs are rendered');
    [
      'Overview',
      'Allocations',
      'Deployments',
      'Versions',
      'Scaling',
      'Services',
      'Placement',
      'Events',
      'Evaluations',
      'Definition',
    ].forEach((label) => {
      assert.ok(labels().includes(label), `${label} tab`);
    });

    await componentA11yAudit(this.element, assert);
  });

  test('the tabs of a batch job do not include deployments', async function (assert) {
    const store = this.owner.lookup('service:store');
    const job = store.createRecord('job', {
      id: '["batch-job","default"]',
      plainId: 'batch-job',
      name: 'batch-job',
      type: 'batch',
    });
    this.set('job', job);

    await render(hbs`<Jobs::Tabs @job={{this.job}} />`);

    assert.ok(labels().includes('Definition'), 'Definition tab');
    assert.ok(labels().includes('Versions'), 'Versions tab');
    assert.notOk(labels().includes('Deployments'), 'No deployments tab');
  });

  test('the parent of periodic launches has no tabs for allocations, scaling or placement', async function (assert) {
    const store = this.owner.lookup('service:store');
    const job = store.createRecord('job', {
      id: '["parent","default"]',
      plainId: 'parent',
      name: 'parent',
      type: 'batch',
      periodic: true,
    });
    this.set('job', job);

    await render(hbs`<Jobs::Tabs @job={{this.job}} />`);

    ['Allocations', 'Scaling', 'Placement', 'Evaluations'].forEach((label) => {
      assert.notOk(labels().includes(label), `No ${label} tab`);
    });
    assert.ok(labels().includes('Events'), 'Events tab');
  });
});
