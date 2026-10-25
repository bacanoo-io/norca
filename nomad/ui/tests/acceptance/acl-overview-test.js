/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

/* eslint-disable qunit/require-expect */
import { module, test } from 'qunit';
import { click, currentURL, findAll, visit } from '@ember/test-helpers';
import a11yAudit from 'nomad-ui/tests/helpers/a11y-audit';
import { open, setupAcl, signInWith } from 'nomad-ui/tests/helpers/acl';

const TAB_ORDER = ['Overview', 'Tokens', 'Roles', 'Policies', 'Auth methods'];

// Clicks the subnav tab with the given label.
async function tab(label) {
  const link = findAll('[data-test-access-control-subnav] [data-test-tab]')
    .find((el) => el.textContent.trim() === label)
    ?.querySelector('a');
  await click(link);
}

module('Acceptance | access control | overview', function (hooks) {
  setupAcl(hooks);

  test('cards count every record type and the tabs lead to the lists', async function (assert) {
    const db = this.server.db;
    await open();

    assert.strictEqual(currentURL(), '/access-control');
    assert.dom('[data-test-title]').hasText('Access Control');
    assert.dom('[data-test-gutter-link="access-control"]').hasClass('active');

    for (const [card, records] of [
      ['tokens', db.tokens],
      ['roles', db.roles],
      ['policies', db.policies],
    ]) {
      assert
        .dom(`[data-test-${card}-card] [data-test-${card}-count]`)
        .hasText(String(records.length), `${card} are counted`);
    }

    const tabs = findAll(
      '[data-test-access-control-subnav] [data-test-tab]'
    ).map((el) => el.textContent.trim());
    assert.deepEqual(tabs, TAB_ORDER);

    await a11yAudit(assert);

    await click('[data-test-roles-card] a[href$="/access-control/roles"]');
    assert.strictEqual(currentURL(), '/access-control/roles');
    await tab('Tokens');
    assert.strictEqual(currentURL(), '/access-control/tokens');
    await tab('Policies');
    assert.strictEqual(currentURL(), '/access-control/policies');
  });

  test('the gutter link opens the section', async function (assert) {
    await visit('/jobs');
    await click('[data-test-gutter-link="access-control"]');
    assert.strictEqual(currentURL(), '/access-control');
  });

  test('a client token is kept out of the whole section', async function (assert) {
    signInWith(this.server, { name: 'Multi Role And Policy Token' });

    await open();
    assert.strictEqual(currentURL(), '/jobs');
    assert.dom('[data-test-gutter-link="access-control"]').doesNotExist();

    await open('tokens', 'cl4y-t0k3n');
    assert.strictEqual(currentURL(), '/jobs', 'deep links are refused too');
  });
});
