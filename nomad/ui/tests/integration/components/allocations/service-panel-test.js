/**
 * Copyright (c) HashiCorp, Inc.
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import { module, test } from 'qunit';
import { setupRenderingTest } from 'ember-qunit';
import { click, render, settled } from '@ember/test-helpers';
import { hbs } from 'ember-cli-htmlbars';
import { componentA11yAudit } from 'nomad-ui/tests/helpers/a11y-audit';
import Service from '@ember/service';
import EmberObject from '@ember/object';

module('Integration | Component | allocations/service-panel', function (hooks) {
  setupRenderingTest(hooks);

  hooks.beforeEach(function () {
    const mockSystem = Service.extend({
      agent: EmberObject.create({
        config: { UI: { Consul: { BaseUIURL: '' } } },
      }),
    });
    this.owner.register('service:system', mockSystem);
    this.system = this.owner.lookup('service:system');
  });

  const template = hbs`
    {{#if this.service}}
      <Allocations::ServicePanel @service={{this.service}} @allocation={{this.allocation}} @onClose={{this.close}} />
    {{/if}}
  `;

  test('it shows the service and closes', async function (assert) {
    assert.expect(3);
    this.set('close', () => this.set('service', null));
    this.set('allocation', {
      id: 'myAlloc',
      shortId: 'myAlloc',
      clientStatus: 'running',
      node: { id: 'node-1', shortId: 'node-1', name: 'Node 1' },
    });
    this.set('service', {
      name: 'Funky Service',
      provider: 'nomad',
      healthChecks: [],
    });
    await render(template);

    assert
      .dom('[data-test-service-panel-name]')
      .hasText('Funky Service', 'The panel shows the service');
    await componentA11yAudit(this.element, assert);

    await click('[data-test-side-panel-close]');
    assert.dom('[data-test-service-panel]').doesNotExist('The panel closed');
  });

  test('it aggregates the service health', async function (assert) {
    const healthy = {
      name: 'Funky Service',
      provider: 'nomad',
      healthChecks: [
        { Check: 'one', Status: 'success', Alloc: 'myAlloc' },
        { Check: 'two', Status: 'success', Alloc: 'myAlloc' },
      ],
    };
    const unhealthy = {
      name: 'Funky Service',
      provider: 'nomad',
      healthChecks: [
        { Check: 'one', Status: 'failure', Alloc: 'myAlloc' },
        { Check: 'two', Status: 'success', Alloc: 'myAlloc' },
      ],
    };

    this.set('close', () => {});
    this.set('allocation', { id: 'myAlloc', clientStatus: 'running' });
    this.set('service', healthy);
    await render(template);
    assert.dom('[data-test-aggregate-status]').hasText('Healthy');
    assert
      .dom('[data-test-health-checks] [data-test-ui-table-row]')
      .exists({ count: 2 }, 'One row per check');
    assert.dom('[data-test-check-tick]').exists({ count: 2 });

    this.set('service', unhealthy);
    assert.dom('[data-test-aggregate-status]').hasText('Unhealthy');

    this.set('service', healthy);
    this.set('allocation', { id: 'myAlloc2', clientStatus: 'failed' });
    assert.dom('[data-test-aggregate-status]').hasText('Health Unknown');
  });

  test('Consul services have no health checks but a link to Consul', async function (assert) {
    this.set('close', () => {});
    this.set('service', {
      name: 'Consul Service',
      provider: 'consul',
      healthChecks: [],
    });
    await render(template);
    assert.dom('[data-test-aggregate-status]').doesNotExist();
    assert.dom('[data-test-health-checks]').doesNotExist();
    assert.dom('[data-test-consul-link-notice]').doesNotExist();

    this.system.agent.config.UI.Consul.BaseUIURL = 'http://localhost:8500';
    // The panel reads the URL when it is created.
    const service = this.service;
    this.set('service', null);
    await settled();
    this.set('service', service);
    await settled();
    assert.dom('[data-test-consul-link-notice]').exists();
  });
});
