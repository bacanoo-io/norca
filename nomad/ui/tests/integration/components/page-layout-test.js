/**
 * Copyright (c) HashiCorp, Inc.
 * SPDX-License-Identifier: MPL-2.0
 */

/* eslint-disable qunit/require-expect */
import { module, test } from 'qunit';
import { setupRenderingTest } from 'ember-qunit';
import { click, render } from '@ember/test-helpers';
import { hbs } from 'ember-cli-htmlbars';
import startMirage from 'ember-cli-mirage/start-mirage';
import { componentA11yAudit, STRICT } from 'nomad-ui/tests/helpers/a11y-audit';

// The global frame: left menu with the groups of the information
// architecture, top bar, and the .legacy scope for pages not migrated yet.
module('Integration | Component | page layout', function (hooks) {
  setupRenderingTest(hooks);

  hooks.beforeEach(function () {
    window.localStorage.clear();
    this.server = startMirage(this.owner);
    this.server.create('agent');
    // LinkTo in a rendering test needs the routes set up.
    // eslint-disable-next-line ember/no-private-routing-service
    this.owner.lookup('router:main').setupRouter();
  });

  hooks.afterEach(function () {
    this.server.shutdown();
  });

  test('the menu is grouped per the information architecture', async function (assert) {
    await render(hbs`<PageLayout>content</PageLayout>`);

    const groups = [
      ...this.element.querySelectorAll('[data-test-nav-group]'),
    ].map((el) => el.textContent.trim());
    assert.deepEqual(groups.slice(0, 3), ['Overview', 'Workloads', 'Capacity']);
    assert.ok(groups.includes('Operations'));
    assert.dom('[data-test-gutter-link="dashboard"]').hasText('Dashboard');
    assert.dom('[data-test-gutter-link="jobs"]').hasText('Jobs');
    assert
      .dom('[data-test-gutter-link="servers"]')
      .hasText('Servers and regions');
    assert
      .dom('[data-test-gutter-link="optimize"]')
      .doesNotExist('no Optimize item');
    assert.dom('[data-test-brand]').hasText('Norca');
    assert.dom('.nomad-logo').doesNotExist('no HashiCorp logo');
    await componentA11yAudit(this.element, assert, STRICT);
  });

  test('pages that are not migrated render inside the legacy scope', async function (assert) {
    await render(
      hbs`<PageLayout><p data-test-inner>legacy page</p></PageLayout>`
    );
    assert.dom('[data-test-legacy-scope] [data-test-inner]').exists();
    assert
      .dom('[data-test-legacy-scope]')
      .hasClass('legacy')
      .hasAttribute('data-bs-theme', 'light');

    await render(
      hbs`<PageLayout @legacy={{false}}><p data-test-inner>new page</p></PageLayout>`
    );
    assert.dom('[data-test-legacy-scope]').doesNotExist();
    assert.dom('[data-test-page-content] [data-test-inner]').exists();
  });

  test('the menu collapses to icons and remembers it', async function (assert) {
    await render(hbs`<PageLayout>content</PageLayout>`);

    assert.dom('[data-test-frame]').doesNotHaveClass('norca-sidebar-collapsed');
    await click('[data-test-sidebar-collapse]');
    assert.dom('[data-test-frame]').hasClass('norca-sidebar-collapsed');
    assert
      .dom('[data-test-sidebar-collapse]')
      .hasAttribute('aria-pressed', 'true');
    assert.strictEqual(window.localStorage.norcaSidebarCollapsed, 'true');

    await click('[data-test-sidebar-collapse]');
    assert.dom('[data-test-frame]').doesNotHaveClass('norca-sidebar-collapsed');
  });

  test('the mobile menu toggle opens and closes the menu', async function (assert) {
    await render(hbs`<PageLayout>content</PageLayout>`);

    assert.dom('#norca-sidebar-menu').doesNotHaveClass('show');
    await click('[data-test-sidebar-toggle]');
    assert.dom('#norca-sidebar-menu').hasClass('show');
    assert
      .dom('[data-test-sidebar-toggle]')
      .hasAttribute('aria-expanded', 'true');
    await click('[data-test-sidebar-toggle]');
    assert.dom('#norca-sidebar-menu').doesNotHaveClass('show');
  });
});
