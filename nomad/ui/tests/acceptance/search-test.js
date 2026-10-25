/**
 * Copyright (c) HashiCorp, Inc.
 * SPDX-License-Identifier: MPL-2.0
 */

/* eslint-disable qunit/require-expect, ember-a11y-testing/a11y-audit-called */
import { module, test } from 'qunit';
import { currentURL, find, triggerKeyEvent, visit } from '@ember/test-helpers';
import { setupApplicationTest } from 'ember-qunit';
import { setupMirage } from 'ember-cli-mirage/test-support';
import { Response } from 'miragejs';
import Layout from 'nomad-ui/tests/pages/layout';
import { componentA11yAudit, STRICT } from 'nomad-ui/tests/helpers/a11y-audit';

// The command palette (Ctrl+K) replaces the global search: pages, actions
// and the results of the fuzzy search API in one keyboard-driven list.
module('Acceptance | search', function (hooks) {
  setupApplicationTest(hooks);
  setupMirage(hooks);

  hooks.beforeEach(function () {
    window.localStorage.clear();
    server.create('agent');
    server.create('node-pool');
    server.create('node', { name: 'xyz' });
    server.create('namespace');
    server.create('namespace', { id: 'dev' });
    server.create('job', {
      id: 'vwxyz',
      namespaceId: 'default',
      groupsCount: 1,
      groupTaskCount: 1,
    });
    server.create('job', {
      id: 'xyzw',
      name: 'xyzw job',
      namespaceId: 'dev',
      groupsCount: 1,
      groupTaskCount: 1,
    });
    server.create('csi-plugin', { id: 'xyz-plugin', createVolumes: false });
  });

  test('the palette opens from the top bar, with Ctrl+K and with /, and closes with Escape', async function (assert) {
    await visit('/jobs');

    await Layout.navbar.palette.open();
    assert.true(Layout.palette.isPresent);
    assert.dom('[data-test-palette-input]').isFocused();
    // The palette is audited with every rule; the page behind it is not
    // migrated yet.
    await componentA11yAudit(find('[data-test-palette]'), assert, STRICT);

    await triggerKeyEvent('[data-test-palette-input]', 'keydown', 'Escape');
    assert.false(Layout.palette.isPresent, 'Escape closes it');

    await triggerKeyEvent(document, 'keydown', 'K', { ctrlKey: true });
    assert.true(Layout.palette.isPresent, 'Ctrl+K opens it');
    await triggerKeyEvent(document, 'keydown', 'K', { ctrlKey: true });
    assert.false(Layout.palette.isPresent, 'Ctrl+K closes it again');

    await triggerKeyEvent(document, 'keydown', '/');
    assert.true(Layout.palette.isPresent, '/ opens it');
  });

  test('pages and actions are filtered by the query and navigate', async function (assert) {
    await visit('/jobs');
    await Layout.navbar.palette.open();

    assert.ok(Layout.palette.items.length > 5, 'pages and actions are listed');
    await Layout.palette.input('clie');
    assert.equal(Layout.palette.items[0].id, 'page-clients');
    assert.true(Layout.palette.items[0].isActive, 'the first match is active');

    await Layout.palette.keydown({ key: 'Enter' });
    assert.equal(currentURL(), '/clients');
    assert.false(Layout.palette.isPresent, 'choosing closes the palette');
  });

  test('fuzzy search results are listed per context and navigate', async function (assert) {
    await visit('/jobs');
    await Layout.navbar.palette.open();
    await Layout.palette.input('xy');

    const ids = Layout.palette.items.map((item) => item.id);
    assert.ok(
      ids.includes('job-default-vwxyz'),
      'a job of the default namespace'
    );
    assert.ok(ids.includes('job-dev-xyzw'), 'a job of another namespace');
    assert.ok(
      ids.some((id) => id.startsWith('node-')),
      'a client'
    );
    assert.ok(ids.includes('plugin-xyz-plugin'), 'a CSI plugin');

    await Layout.palette.items
      .toArray()
      .find((item) => item.id === 'job-dev-xyzw')
      .choose();
    assert.equal(currentURL(), '/jobs/xyzw@dev');

    await Layout.navbar.palette.open();
    await Layout.palette.input('xy');
    await Layout.palette.items
      .toArray()
      .find((item) => item.id === 'plugin-xyz-plugin')
      .choose();
    assert.equal(currentURL(), '/csi/plugins/xyz-plugin');
  });

  test('arrow keys move the selection', async function (assert) {
    await visit('/jobs');
    await Layout.navbar.palette.open();
    assert.true(Layout.palette.items[0].isActive);
    await Layout.palette.keydown({ key: 'ArrowDown' });
    assert.true(Layout.palette.items[1].isActive);
    await Layout.palette.keydown({ key: 'ArrowUp' });
    assert.true(Layout.palette.items[0].isActive);
  });

  test('without the fuzzy search API only pages and actions are searched', async function (assert) {
    server.post('/search/fuzzy', () => new Response(500, {}, ''));

    await visit('/jobs');
    await Layout.navbar.palette.open();
    await Layout.palette.input('xy');

    assert.notOk(
      Layout.palette.items.toArray().some((item) => item.id.startsWith('job-')),
      'no search results'
    );
    const searches = server.pretender.handledRequests.filter(
      (request) =>
        request.url === '/v1/search/fuzzy' &&
        !request.requestBody.includes('feature-detection-query')
    );
    assert.equal(
      searches.length,
      0,
      'no search request after the failed feature detection'
    );
  });
});
