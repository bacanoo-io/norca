/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

/* eslint-disable qunit/require-expect */
import { module, test } from 'qunit';
import { click, currentURL, fillIn, find } from '@ember/test-helpers';
import a11yAudit from 'nomad-ui/tests/helpers/a11y-audit';
import {
  lastBodyTo,
  open,
  pick,
  rowsOf,
  setupAcl,
} from 'nomad-ui/tests/helpers/acl';

const TOKEN_API = '/v1/acl/token';
const CLIENT_TOKEN = 'cl4y-t0k3n';

// Fills the name of the draft and saves it.
async function saveDraft(name) {
  await fillIn('[data-test-token-name-input]', name);
  await click('[data-test-token-save]');
}

module('Acceptance | access control | tokens', function (hooks) {
  setupAcl(hooks);

  test('the list shows every token', async function (assert) {
    await open('tokens');
    assert.strictEqual(rowsOf('token').length, this.server.db.tokens.length);
    await a11yAudit(assert);
  });

  test('a token page renders the editor', async function (assert) {
    await open('tokens', CLIENT_TOKEN);
    assert.dom('[data-test-token-editor]').exists();
    await a11yAudit(assert);
  });

  test('the signed-in token is badged and protected from deletion', async function (assert) {
    const self = this.server.db.tokens.findBy({ type: 'management' });
    await open('tokens', self.id);
    assert.dom('[data-test-own-token-badge]').exists();
    assert.dom('[data-test-delete-token] [data-test-idle-button]').isDisabled();
  });

  test('a client token is created with roles, policies, TTL and global', async function (assert) {
    await open('tokens');
    await click('[data-test-create-token]');
    assert.strictEqual(currentURL(), '/access-control/tokens/new');
    await a11yAudit(assert);

    await click('[data-test-token-global] input');
    await click('[data-test-token-expiration] input[value="10m"]');
    await pick('[data-test-token-roles]', '[data-test-role-name]', 'reader');
    await pick(
      '[data-test-token-policies]',
      '[data-test-policy-name]',
      'operator'
    );
    await saveDraft('CI Token');

    assert.dom('.flash-message.alert-success').exists();
    const sent = lastBodyTo(this.server, 'POST', TOKEN_API);
    assert.deepEqual(
      {
        Name: sent.Name,
        Type: sent.Type,
        Global: sent.Global,
        ExpirationTTL: sent.ExpirationTTL,
        Policies: sent.Policies,
        Roles: sent.Roles,
      },
      {
        Name: 'CI Token',
        Type: 'client',
        Global: true,
        ExpirationTTL: '10m',
        Policies: ['operator'],
        Roles: [{ ID: 'reader' }],
      }
    );

    // The new token opens on its own page with accessor and secret.
    const stored = this.server.db.tokens.findBy({ name: 'CI Token' });
    assert.strictEqual(currentURL(), `/access-control/tokens/${stored.id}`);
    assert.dom('[data-test-token-accessor]').hasValue(stored.accessorId);
    assert.dom('[data-test-token-secret]').hasValue(stored.secretId);
  });

  test('a management token drops roles and policies', async function (assert) {
    await open('tokens', 'new');
    await click(find('[data-test-token-policies] tbody tr input'));

    await click('[data-test-token-type="management"]');
    assert
      .dom('[data-test-token-policies]')
      .doesNotExist('no policy chooser for management tokens');
    assert.dom('[data-test-token-roles]').doesNotExist();

    await saveDraft('Root Token');
    assert.dom('.flash-message.alert-success').exists();
    const sent = lastBodyTo(this.server, 'POST', TOKEN_API);
    assert.strictEqual(sent.Type, 'management');
    assert.deepEqual([sent.Policies, sent.Roles], [[], []]);
  });

  test('an expiration in the past is refused before anything is sent', async function (assert) {
    await open('tokens', 'new');
    await click('[data-test-token-expiration] input[value="custom"]');
    await fillIn('[data-test-token-expiration-time-input]', '2001-01-01T00:00');
    await saveDraft('Too Late');

    assert.dom('.flash-message.alert-critical').exists();
    assert.strictEqual(lastBodyTo(this.server, 'POST', TOKEN_API), undefined);
    assert.strictEqual(currentURL(), '/access-control/tokens/new');
  });

  test('cancelling a new token leaves no draft behind', async function (assert) {
    await open('tokens', 'new');
    await fillIn('[data-test-token-name-input]', 'Abandoned Token');
    await click('[data-test-token-cancel]');

    assert.strictEqual(currentURL(), '/access-control/tokens');
    assert.strictEqual(rowsOf('token').length, this.server.db.tokens.length);
    assert.dom('[data-test-token-name="Abandoned Token"]').doesNotExist();
  });

  test('a saved token keeps its global flag and shows its expiration', async function (assert) {
    await open('tokens', CLIENT_TOKEN);
    assert.dom('[data-test-token-global] input').isDisabled();
    assert.dom('[data-test-token-expiration] input').doesNotExist();

    const lasting = this.server.db.tokens.findBy({ name: 'Multi Role Token' });
    await open('tokens', lasting.id);
    assert.dom('[data-test-token-expiration]').hasText('Token does not expire');
  });
});
