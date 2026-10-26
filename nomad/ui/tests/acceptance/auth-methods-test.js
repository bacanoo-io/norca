/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

/* eslint-disable qunit/require-expect */
import { module, test } from 'qunit';
import { click, currentURL, fillIn, findAll, visit } from '@ember/test-helpers';
import { setupApplicationTest } from 'ember-qunit';
import { setupMirage } from 'ember-cli-mirage/test-support';
import faker from 'nomad-ui/mirage/faker';
import a11yAudit from 'nomad-ui/tests/helpers/a11y-audit';
import { allScenarios } from '../../mirage/scenarios/default';

module('Acceptance | auth methods', function (hooks) {
  setupApplicationTest(hooks);
  setupMirage(hooks);

  hooks.beforeEach(function () {
    window.localStorage.clear();
    faker.seed(1);
    allScenarios.rolesTestCluster(server);
    const token = server.db.tokens.findBy((t) => t.type === 'management');
    window.localStorage.nomadTokenSecret = token.secretId;
  });

  hooks.afterEach(function () {
    window.localStorage.clear();
  });

  test('the auth methods are listed with their settings', async function (assert) {
    server.db.authMethods.remove();
    server.create('auth-method', {
      name: 'okta',
      type: 'OIDC',
      default: true,
      tokenLocality: 'global',
      maxTokenTTL: '8h',
    });
    server.create('auth-method', {
      name: 'ci',
      type: 'JWT',
      default: false,
      tokenLocality: 'local',
      maxTokenTTL: '1h',
    });

    await visit('/access-control');
    await click('[data-test-tab-link="auth-methods"]');
    assert.equal(currentURL(), '/access-control/auth-methods');

    assert.dom('[data-test-auth-method-row]').exists({ count: 2 });
    assert
      .dom('[data-test-auth-method-row="okta"] [data-test-auth-method-type]')
      .hasText('OIDC');
    assert
      .dom('[data-test-auth-method-row="okta"] [data-test-auth-method-default]')
      .hasText('default');
    assert
      .dom('[data-test-auth-method-row="ci"] [data-test-auth-method-locality]')
      .hasText('local');
    assert
      .dom('[data-test-auth-method-row="ci"] [data-test-auth-method-ttl]')
      .hasText('1h');

    await a11yAudit(assert);

    await fillIn('[data-test-auth-methods-search]', 'ok');
    assert.deepEqual(
      findAll('[data-test-auth-method-name]').map((cell) =>
        cell.textContent.trim()
      ),
      ['okta'],
      'The search filters the list'
    );
  });

  test('without auth methods the page says how to set one up', async function (assert) {
    server.db.authMethods.remove();
    await visit('/access-control/auth-methods');
    assert.dom('[data-test-empty-auth-methods]').exists();
    assert
      .dom('[data-test-empty-auth-methods]')
      .containsText('norca acl auth-method create');
  });
});
