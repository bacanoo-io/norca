/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

/* eslint-disable qunit/require-expect */
import { module, test } from 'qunit';
import {
  click,
  currentURL,
  fillIn,
  find,
  findAll,
  visit,
} from '@ember/test-helpers';
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

// Finds a list row by the exact text of its name cell
function rowFor(selector, name) {
  return findAll(selector).find(
    (row) =>
      row
        .querySelector('[data-test-role-name], [data-test-token-name]')
        ?.textContent.trim() === name
  );
}

module('Acceptance | roles', function (hooks) {
  setupApplicationTest(hooks);
  setupMirage(hooks);

  hooks.beforeEach(function () {
    window.localStorage.clear();
    faker.seed(1);
    allScenarios.rolesTestCluster(server);
    signInAs(server, (t) => t.type === 'management');
  });

  hooks.afterEach(function () {
    window.localStorage.clear();
  });

  test('roles index lists every role, sorted by name', async function (assert) {
    await AccessControl.visitRoles();
    assert.equal(currentURL(), '/access-control/roles');
    assert.equal(AccessControl.roles.length, server.db.roles.length);

    const names = AccessControl.roles.map((row) => row.name);
    const sorted = server.db.roles
      .map((role) => role.name)
      .sort((a, b) => a.localeCompare(b));
    assert.deepEqual(names, sorted, 'rows are sorted by name');

    await a11yAudit(assert);
  });

  test('roles index shows policies and token counts', async function (assert) {
    await AccessControl.visitRoles();

    const editor = server.db.roles.find('editor');
    const row = rowFor('[data-test-role-row]', 'editor');
    const tags = row.querySelectorAll('[data-test-role-policies] .tag');
    assert.equal(tags.length, editor.policyIds.length);
    assert
      .dom(row.querySelector('[data-test-role-description]'))
      .hasText(editor.description);

    const expectedTokens = server.db.tokens.filter((token) =>
      (token.roleIds || []).includes('editor')
    ).length;
    assert
      .dom(row.querySelector('[data-test-role-total-tokens]'))
      .hasText(String(expectedTokens));
  });

  test('clicking a role row opens the role', async function (assert) {
    await AccessControl.visitRoles();
    await click(rowFor('[data-test-role-row]', 'reader'));
    assert.equal(currentURL(), '/access-control/roles/reader');
    assert.dom('[data-test-title]').includesText('reader');
    assert.dom('[data-test-role-name-input]').hasValue('reader');
  });

  test('role page shows its policies and linked tokens', async function (assert) {
    const role = server.db.roles.find('editor');
    await visit('/access-control/roles/editor');

    assert
      .dom('[data-test-role-policies] tbody tr')
      .exists({ count: server.db.policies.length }, 'all policies listed');

    const checked = findAll('[data-test-role-policies] tbody tr input:checked')
      .map((input) =>
        input
          .closest('tr')
          .querySelector('[data-test-policy-name]')
          .textContent.trim()
      )
      .sort();
    assert.deepEqual(checked, role.policyIds.slice().sort());

    const linkedTokens = server.db.tokens.filter((token) =>
      (token.roleIds || []).includes('editor')
    );
    assert
      .dom('[data-test-role-token-row]')
      .exists({ count: linkedTokens.length });

    await a11yAudit(assert);
  });

  test('a role can be edited', async function (assert) {
    await visit('/access-control/roles/reader');
    await fillIn('[data-test-role-description-input]', 'Reads all the things');
    await click('[data-test-save-role]');
    assert.dom('.flash-message.alert-success').exists();

    const request = server.pretender.handledRequests.find(
      (req) => req.method === 'PUT' && req.url.includes('/v1/acl/role/reader')
    );
    assert.ok(request, 'the role was updated with a PUT');
    const body = JSON.parse(request.requestBody);
    assert.equal(body.Description, 'Reads all the things');
    assert.deepEqual(
      body.Policies.map((p) => p.Name).sort(),
      server.db.roles.find('reader').policyIds.slice().sort(),
      'policies are sent as policy links'
    );

    await AccessControl.visitRoles();
    assert
      .dom(
        rowFor('[data-test-role-row]', 'reader').querySelector(
          '[data-test-role-description]'
        )
      )
      .hasText('Reads all the things');
  });

  test('a role needs at least one policy', async function (assert) {
    await visit('/access-control/roles/editor');
    findAll('[data-test-role-policies] tbody tr input:checked').forEach(
      (input) => input.click()
    );
    await click('[data-test-save-role]');
    assert.dom('.flash-message.alert-critical').exists();
    assert.notOk(
      server.pretender.handledRequests.find((req) => req.method === 'PUT'),
      'nothing is sent to the server'
    );
  });

  test('a new role can be created', async function (assert) {
    await AccessControl.visitRoles();
    await click('[data-test-create-role]');
    assert.equal(currentURL(), '/access-control/roles/new');

    await fillIn('[data-test-role-name-input]', 'not a valid name');
    await click('[data-test-save-role]');
    assert
      .dom('.flash-message.alert-critical')
      .exists('invalid names are rejected');
    assert.equal(currentURL(), '/access-control/roles/new');

    await fillIn('[data-test-role-name-input]', 'auditor');
    await fillIn('[data-test-role-description-input]', 'Looks at things');
    const jobReaderRow = findAll('[data-test-role-policies] tbody tr').find(
      (row) =>
        row.querySelector('[data-test-policy-name]').textContent.trim() ===
        'job-reader'
    );
    await click(jobReaderRow.querySelector('input'));
    await click('[data-test-save-role]');

    assert.dom('.flash-message.alert-success').exists();
    const created = server.db.roles.findBy({ name: 'auditor' });
    assert.ok(created, 'the role exists in the backend');
    assert.equal(currentURL(), `/access-control/roles/${created.id}`);

    const request = server.pretender.handledRequests.find(
      (req) => req.method === 'POST' && req.url.endsWith('/v1/acl/role')
    );
    const body = JSON.parse(request.requestBody);
    assert.equal(body.Name, 'auditor');
    assert.deepEqual(body.Policies, [{ Name: 'job-reader' }]);

    await AccessControl.visitRoles();
    assert.ok(rowFor('[data-test-role-row]', 'auditor'), 'listed');
  });

  test('role names must be unique', async function (assert) {
    await visit('/access-control/roles/new');
    await fillIn('[data-test-role-name-input]', 'editor');
    await click(find('[data-test-role-policies] tbody tr input'));
    await click('[data-test-save-role]');
    assert.dom('.flash-message.alert-critical').exists();
    assert.equal(currentURL(), '/access-control/roles/new');
  });

  test('a role can be deleted from its page', async function (assert) {
    await visit('/access-control/roles/denier');
    await click('[data-test-delete-role]');
    assert.dom('.flash-message.alert-success').exists();
    assert.equal(currentURL(), '/access-control/roles');
    assert.notOk(rowFor('[data-test-role-row]', 'denier'));
    assert.notOk(server.db.roles.find('denier'), 'deleted in the backend');

    // Tokens no longer show the deleted role
    await AccessControl.visitTokens();
    const tokenRow = rowFor('[data-test-token-row]', 'Clientless Role Token');
    assert
      .dom(tokenRow.querySelector('[data-test-token-roles]'))
      .hasText('No Roles');
  });

  test('deleting every role shows an empty state', async function (assert) {
    await AccessControl.visitRoles();
    for (const row of findAll('[data-test-role-row]')) {
      await click(row.querySelector('[data-test-delete-role]'));
    }
    assert.dom('[data-test-role-row]').doesNotExist();
    assert.dom('[data-test-empty-roles-list-headline]').exists();
    assert.equal(
      currentURL(),
      '/access-control/roles',
      'delete buttons do not open the role'
    );
  });

  test('roles are not accessible without a management token', async function (assert) {
    signInAs(server, (t) => t.name === 'High Level Role Token');
    await visit('/access-control/roles');
    assert.equal(currentURL(), '/jobs');
  });
});
