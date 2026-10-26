/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

/* eslint-disable qunit/require-expect */
import { module, test } from 'qunit';
import { click, currentURL, fillIn } from '@ember/test-helpers';
import a11yAudit from 'nomad-ui/tests/helpers/a11y-audit';
import {
  cellText,
  namesOf,
  open,
  rowNamed,
  setupAcl,
} from 'nomad-ui/tests/helpers/acl';

// The auth methods the list test starts from, as mirage attributes and as
// the cells the page should show for them.
const METHODS = [
  {
    attrs: {
      name: 'okta',
      type: 'OIDC',
      default: true,
      tokenLocality: 'global',
      maxTokenTTL: '8h',
    },
    cells: { type: 'OIDC', default: 'default' },
  },
  {
    attrs: {
      name: 'ci',
      type: 'JWT',
      default: false,
      tokenLocality: 'local',
      maxTokenTTL: '1h',
    },
    cells: { locality: 'local', ttl: '1h' },
  },
];

module('Acceptance | access control | auth methods', function (hooks) {
  setupAcl(hooks);

  hooks.beforeEach(function () {
    this.server.db.authMethods.remove();
  });

  test('the methods are listed with their settings and can be searched', async function (assert) {
    for (const { attrs } of METHODS) {
      this.server.create('auth-method', attrs);
    }

    await open();
    await click('[data-test-tab-link="auth-methods"]');
    assert.strictEqual(currentURL(), '/access-control/auth-methods');

    assert.strictEqual(namesOf('auth-method').length, METHODS.length);
    for (const { attrs, cells } of METHODS) {
      const row = rowNamed('auth-method', attrs.name);
      for (const [field, text] of Object.entries(cells)) {
        assert.strictEqual(
          cellText(row, 'auth-method', field),
          text,
          `${attrs.name}: ${field}`
        );
      }
    }

    await a11yAudit(assert);

    await fillIn('[data-test-auth-methods-search]', 'ok');
    assert.deepEqual(namesOf('auth-method'), ['okta'], 'search narrows');
  });

  test('with no methods the page points to the CLI', async function (assert) {
    await open('auth-methods');
    assert
      .dom('[data-test-empty-auth-methods]')
      .containsText('norca acl auth-method create');
  });
});
