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
import AccessControl from 'nomad-ui/tests/pages/access-control';

function signInAs(server, predicate) {
  const token = server.db.tokens.findBy(predicate);
  window.localStorage.nomadTokenSecret = token.secretId;
  return token;
}

function rowByName(tableSelector, nameSelector, name) {
  return findAll(`${tableSelector} tbody tr`).find(
    (row) => row.querySelector(nameSelector)?.textContent.trim() === name
  );
}

function lastRequest(server, method, urlSuffix) {
  return server.pretender.handledRequests
    .filter((req) => req.method === method && req.url.endsWith(urlSuffix))
    .pop();
}

module('Acceptance | access control', function (hooks) {
  setupApplicationTest(hooks);
  setupMirage(hooks);

  hooks.beforeEach(function () {
    window.localStorage.clear();
    faker.seed(1);
    allScenarios.rolesTestCluster(server);
  });

  hooks.afterEach(function () {
    window.localStorage.clear();
  });

  test('the overview shows counts and links for tokens, roles and policies', async function (assert) {
    signInAs(server, (t) => t.type === 'management');
    await AccessControl.visit();

    assert.equal(currentURL(), '/access-control');
    assert
      .dom('[data-test-gutter-link="access-control"]')
      .hasClass('is-active');
    assert.equal(AccessControl.title, 'Access Control');

    assert.equal(
      AccessControl.tokensCard.count,
      String(server.db.tokens.length)
    );
    assert.equal(AccessControl.rolesCard.count, String(server.db.roles.length));
    assert.equal(
      AccessControl.policiesCard.count,
      String(server.db.policies.length)
    );

    assert.deepEqual(
      AccessControl.subnav.tabs.map((tab) => tab.name),
      ['Overview', 'Tokens', 'Roles', 'Policies']
    );

    await a11yAudit(assert);

    await click('[data-test-roles-card] h2 a');
    assert.equal(currentURL(), '/access-control/roles');

    await AccessControl.subnav.tabs.objectAt(1).click();
    assert.equal(currentURL(), '/access-control/tokens');

    await AccessControl.subnav.tabs.objectAt(3).click();
    assert.equal(currentURL(), '/access-control/policies');
  });

  test('the gutter menu leads to the overview', async function (assert) {
    signInAs(server, (t) => t.type === 'management');
    await visit('/jobs');
    await click('[data-test-gutter-link="access-control"]');
    assert.equal(currentURL(), '/access-control');
  });

  test('client tokens cannot see the section', async function (assert) {
    signInAs(server, (t) => t.name === 'Multi Role And Policy Token');
    await visit('/access-control');
    assert.equal(currentURL(), '/jobs');
    assert.dom('[data-test-gutter-link="access-control"]').doesNotExist();

    await visit('/access-control/tokens/cl4y-t0k3n');
    assert.equal(currentURL(), '/jobs');
  });

  test('the tokens index renders without errors and passes an a11y audit', async function (assert) {
    signInAs(server, (t) => t.type === 'management');
    await AccessControl.visitTokens();
    assert.equal(AccessControl.tokens.length, server.db.tokens.length);
    await a11yAudit(assert);
  });

  test('a token page passes an a11y audit', async function (assert) {
    signInAs(server, (t) => t.type === 'management');
    await AccessControl.visitToken({ id: 'cl4y-t0k3n' });
    assert.dom('[data-test-token-editor]').exists();
    await a11yAudit(assert);
  });

  test('your own token is marked and cannot be deleted from its page', async function (assert) {
    const self = signInAs(server, (t) => t.type === 'management');
    await AccessControl.visitToken({ id: self.id });
    assert.dom('[data-test-own-token-badge]').exists();
    assert.dom('[data-test-delete-token]').isDisabled();
  });

  test('creating a client token sends the selected roles, policies, TTL and global flag', async function (assert) {
    signInAs(server, (t) => t.type === 'management');
    await AccessControl.visitTokens();
    await AccessControl.createToken();
    assert.equal(currentURL(), '/access-control/tokens/new');
    await a11yAudit(assert);

    await fillIn('[data-test-token-name-input]', 'CI Token');
    await click('[data-test-token-global]');
    await click('.expiration-time input[value="10m"]');

    const roleRow = rowByName(
      '[data-test-token-roles]',
      '[data-test-role-name]',
      'reader'
    );
    await click(roleRow.querySelector('input'));
    const policyRow = rowByName(
      '[data-test-token-policies]',
      '[data-test-policy-name]',
      'operator'
    );
    await click(policyRow.querySelector('input'));

    await click('[data-test-token-save]');
    assert.dom('.flash-message.alert-success').exists();

    const body = JSON.parse(
      lastRequest(server, 'POST', '/v1/acl/token').requestBody
    );
    assert.equal(body.Name, 'CI Token');
    assert.equal(body.Type, 'client');
    assert.true(body.Global);
    assert.equal(body.ExpirationTTL, '10m');
    assert.deepEqual(body.Policies, ['operator']);
    assert.deepEqual(body.Roles, [{ ID: 'reader' }]);

    // After creation, the new token's page shows its accessor and secret
    const created = server.db.tokens.findBy({ name: 'CI Token' });
    assert.equal(currentURL(), `/access-control/tokens/${created.id}`);
    assert.dom('[data-test-token-accessor]').hasValue(created.accessorId);
    assert.dom('[data-test-token-secret]').hasValue(created.secretId);
  });

  test('management tokens are created without roles or policies', async function (assert) {
    signInAs(server, (t) => t.type === 'management');
    await AccessControl.visitNewToken();

    await fillIn('[data-test-token-name-input]', 'Root Token');
    const policyRow = findAll('[data-test-token-policies] tbody tr')[0];
    await click(policyRow.querySelector('input'));

    await click('[data-test-token-type="management"]');
    assert
      .dom('[data-test-token-policies]')
      .doesNotExist('policies cannot be chosen for management tokens');
    assert.dom('[data-test-token-roles]').doesNotExist();

    await click('[data-test-token-save]');
    assert.dom('.flash-message.alert-success').exists();
    const body = JSON.parse(
      lastRequest(server, 'POST', '/v1/acl/token').requestBody
    );
    assert.equal(body.Type, 'management');
    assert.deepEqual(body.Policies, []);
    assert.deepEqual(body.Roles, []);
  });

  test('a custom expiration in the past is rejected', async function (assert) {
    signInAs(server, (t) => t.type === 'management');
    await AccessControl.visitNewToken();
    await fillIn('[data-test-token-name-input]', 'Too Late');
    await click('.expiration-time input[value="custom"]');
    await fillIn('[data-test-token-expiration-time-input]', '2001-01-01T00:00');
    await click('[data-test-token-save]');
    assert.dom('.flash-message.alert-critical').exists();
    assert.notOk(lastRequest(server, 'POST', '/v1/acl/token'));
    assert.equal(currentURL(), '/access-control/tokens/new');
  });

  test('leaving the new token page discards the draft', async function (assert) {
    signInAs(server, (t) => t.type === 'management');
    await AccessControl.visitNewToken();
    await fillIn('[data-test-token-name-input]', 'Abandoned Token');
    await click('[data-test-token-cancel]');
    assert.equal(currentURL(), '/access-control/tokens');
    assert.equal(AccessControl.tokens.length, server.db.tokens.length);
    assert.dom('[data-test-token-name="Abandoned Token"]').doesNotExist();
  });

  test('existing tokens show their expiration and cannot change the global flag', async function (assert) {
    signInAs(server, (t) => t.type === 'management');
    await AccessControl.visitToken({ id: 'cl4y-t0k3n' });
    assert.dom('[data-test-token-global]').isDisabled();
    assert.dom('.expiration-time input').doesNotExist();

    const neverExpiring = server.db.tokens.findBy({ name: 'Multi Role Token' });
    await AccessControl.visitToken({ id: neverExpiring.id });
    assert.dom('.expiration-time').hasText('Token does not expire');
  });
});
