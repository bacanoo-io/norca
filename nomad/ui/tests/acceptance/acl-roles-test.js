/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

/* eslint-disable qunit/require-expect */
import { module, test } from 'qunit';
import { click, currentURL, fillIn, find, findAll } from '@ember/test-helpers';
import a11yAudit from 'nomad-ui/tests/helpers/a11y-audit';
import {
  cellText,
  confirmDelete,
  lastBodyTo,
  namesOf,
  open,
  pick,
  picked,
  requestsTo,
  rowNamed,
  rowsOf,
  setupAcl,
  signInWith,
  tokensWithRole,
} from 'nomad-ui/tests/helpers/acl';

const POLICY_PICKER = '[data-test-role-policies]';
const POLICY_NAME = '[data-test-policy-name]';

module('Acceptance | access control | roles', function (hooks) {
  setupAcl(hooks);

  test('the list holds every role in name order', async function (assert) {
    await open('roles');
    assert.strictEqual(currentURL(), '/access-control/roles');

    const expected = this.server.db.roles
      .map((role) => role.name)
      .sort((a, b) => a.localeCompare(b));
    assert.deepEqual(namesOf('role'), expected);

    await a11yAudit(assert);
  });

  test('a list row shows the policies, description and tokens of a role', async function (assert) {
    const editor = this.server.db.roles.find('editor');
    await open('roles');

    const row = rowNamed('role', 'editor');
    assert.strictEqual(
      row.querySelectorAll('[data-test-role-policies] .badge').length,
      editor.policyIds.length
    );
    assert.strictEqual(
      cellText(row, 'role', 'description'),
      editor.description
    );
    assert.strictEqual(
      cellText(row, 'role', 'total-tokens'),
      String(tokensWithRole(this.server, 'editor').length)
    );
  });

  test('clicking a row opens that role', async function (assert) {
    await open('roles');
    await click(rowNamed('role', 'reader'));
    assert.strictEqual(currentURL(), '/access-control/roles/reader');
    assert.dom('[data-test-title]').includesText('reader');
    assert.dom('[data-test-role-name-input]').hasValue('reader');
  });

  test('a role page ticks its policies and lists its tokens', async function (assert) {
    const editor = this.server.db.roles.find('editor');
    await open('roles', 'editor');

    assert
      .dom(`${POLICY_PICKER} tbody tr`)
      .exists({ count: this.server.db.policies.length });
    assert.deepEqual(
      picked(POLICY_PICKER, POLICY_NAME),
      [...editor.policyIds].sort()
    );
    assert
      .dom('[data-test-role-token-row]')
      .exists({ count: tokensWithRole(this.server, 'editor').length });

    await a11yAudit(assert);
  });

  test('editing a role sends its policies as links', async function (assert) {
    const reader = this.server.db.roles.find('reader');
    await open('roles', 'reader');
    await fillIn('[data-test-role-description-input]', 'Reads all the things');
    await click('[data-test-save-role]');
    assert.dom('.flash-message.alert-success').exists();

    const sent = lastBodyTo(this.server, 'PUT', /\/v1\/acl\/role\/reader/);
    assert.ok(sent, 'the role is updated with a PUT');
    assert.strictEqual(sent.Description, 'Reads all the things');
    assert.deepEqual(
      sent.Policies.map((link) => link.Name).sort(),
      [...reader.policyIds].sort()
    );

    await open('roles');
    assert.strictEqual(
      cellText(rowNamed('role', 'reader'), 'role', 'description'),
      'Reads all the things'
    );
  });

  test('a role without policies is not saved', async function (assert) {
    await open('roles', 'editor');
    for (const box of findAll(`${POLICY_PICKER} tbody tr input:checked`)) {
      box.click();
    }
    await click('[data-test-save-role]');

    assert.dom('.flash-message.alert-critical').exists();
    assert.strictEqual(
      requestsTo(this.server, 'PUT', /\/v1\/acl\/role/).length,
      0,
      'nothing reaches the server'
    );
  });

  test('a new role is validated, created and listed', async function (assert) {
    await open('roles');
    await click('[data-test-create-role]');
    assert.strictEqual(currentURL(), '/access-control/roles/new');

    await fillIn('[data-test-role-name-input]', 'not a valid name');
    await click('[data-test-save-role]');
    assert.dom('.flash-message.alert-critical').exists('bad name refused');
    assert.strictEqual(currentURL(), '/access-control/roles/new');

    await fillIn('[data-test-role-name-input]', 'auditor');
    await fillIn('[data-test-role-description-input]', 'Looks at things');
    await pick(POLICY_PICKER, POLICY_NAME, 'job-reader');
    await click('[data-test-save-role]');
    assert.dom('.flash-message.alert-success').exists();

    const stored = this.server.db.roles.findBy({ name: 'auditor' });
    assert.ok(stored, 'mirage has the role');
    assert.strictEqual(currentURL(), `/access-control/roles/${stored.id}`);
    const sent = lastBodyTo(this.server, 'POST', '/v1/acl/role');
    assert.strictEqual(sent.Name, 'auditor');
    assert.deepEqual(sent.Policies, [{ Name: 'job-reader' }]);

    await open('roles');
    assert.ok(rowNamed('role', 'auditor'), 'the list shows it');
  });

  test('a role name that is taken is refused', async function (assert) {
    await open('roles', 'new');
    await fillIn('[data-test-role-name-input]', 'editor');
    await click(find(`${POLICY_PICKER} tbody tr input`));
    await click('[data-test-save-role]');
    assert.dom('.flash-message.alert-critical').exists();
    assert.strictEqual(currentURL(), '/access-control/roles/new');
  });

  test('deleting a role on its page also unlinks it from tokens', async function (assert) {
    await open('roles', 'denier');
    await confirmDelete('[data-test-delete-role]');

    assert.dom('.flash-message.alert-success').exists();
    assert.strictEqual(currentURL(), '/access-control/roles');
    assert.notOk(rowNamed('role', 'denier'), 'gone from the list');
    assert.notOk(this.server.db.roles.find('denier'), 'gone from mirage');

    await open('tokens');
    assert.strictEqual(
      cellText(rowNamed('token', 'Clientless Role Token'), 'token', 'roles'),
      'No Roles'
    );
  });

  test('deleting all roles from the list leaves the empty state', async function (assert) {
    await open('roles');
    // Always delete the first row; stop after as many rounds as there were
    // rows, so a failing delete cannot loop forever.
    for (let left = rowsOf('role').length; left > 0; left--) {
      await confirmDelete(
        rowsOf('role')[0].querySelector('[data-test-delete-role]')
      );
    }
    assert.dom('[data-test-empty-roles-list]').exists();
    assert.strictEqual(
      currentURL(),
      '/access-control/roles',
      'the delete buttons did not open a role'
    );
  });

  test('a client token cannot reach the roles', async function (assert) {
    signInWith(this.server, { name: 'High Level Role Token' });
    await open('roles');
    assert.strictEqual(currentURL(), '/jobs');
  });
});
