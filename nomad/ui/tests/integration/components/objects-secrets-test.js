/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

/* eslint-disable qunit/require-expect */
import { module, test } from 'qunit';
import { setupRenderingTest } from 'ember-qunit';
import { click, fillIn, render, settled } from '@ember/test-helpers';
import { hbs } from 'ember-cli-htmlbars';
import { setupMirage } from 'ember-cli-mirage/test-support';
import { Response } from 'miragejs';
import { componentA11yAudit, STRICT } from 'nomad-ui/tests/helpers/a11y-audit';
import { seedSecrets, secretRecord } from 'nomad-ui/mirage/norca/secrets';
import { seedObjects } from 'nomad-ui/mirage/norca/objects';

module('Integration | Component | objects and secrets', function (hooks) {
  setupRenderingTest(hooks);
  setupMirage(hooks);

  hooks.beforeEach(function () {
    this.owner.lookup('service:token').set('aclEnabled', false);
    seedSecrets(server);
    const raw = server.db.norcaSecrets.findBy({ Path: 'prod/api/stripe' });
    // eslint-disable-next-line no-unused-vars
    const { Values, id, ...secret } = raw;
    this.secret = secret;
  });

  test('Secrets::Reveal reads the value only on click and drops it on hide or another version', async function (assert) {
    this.version = 1;
    this.canRead = true;
    await render(
      hbs`<Secrets::Reveal @secret={{this.secret}} @version={{this.version}} @canRead={{this.canRead}} />`
    );
    const reads = () =>
      server.pretender.handledRequests.filter(
        (r) => r.method === 'GET' && r.url.startsWith('/v1/secret/')
      ).length;
    assert.strictEqual(reads(), 0);
    assert.dom('[data-test-secret-values]').doesNotExist();
    await componentA11yAudit(this.element, assert, STRICT);

    await click('[data-test-secret-reveal-button]');
    assert.strictEqual(reads(), 1);
    assert.dom('[data-test-secret-value]').hasText('sk-canary-value');
    await componentA11yAudit(this.element, assert, STRICT);

    await click('[data-test-secret-hide]');
    assert.dom('[data-test-secret-values]').doesNotExist();

    await click('[data-test-secret-reveal-button]');
    this.set('version', 2);
    await settled();
    assert
      .dom('[data-test-secret-values]')
      .doesNotExist('another version is not shown with the old value');

    this.set('canRead', false);
    assert.dom('[data-test-secret-reveal-denied]').exists();
  });

  test('Secrets::Reveal shows a denied read', async function (assert) {
    server.get(
      '/secret/*path',
      () => new Response(403, {}, 'Permission denied')
    );
    await render(
      hbs`<Secrets::Reveal @secret={{this.secret}} @canRead={{true}} />`
    );
    await click('[data-test-secret-reveal-button]');
    assert.dom('[data-test-secret-reveal-error]').includesText('read-secret');
    assert.dom('[data-test-secret-values]').doesNotExist();
  });

  test('Secrets::ValueForm validates keys and writes with the check index', async function (assert) {
    server.db.norcaSecrets.insert(
      secretRecord({ path: 'app/db', values: [{ a: '1', b: '2' }], index: 300 })
    );
    const raw = server.db.norcaSecrets.findBy({ Path: 'app/db' });
    // eslint-disable-next-line no-unused-vars
    const { Values, id, ...secret } = raw;
    this.secret = secret;
    this.saved = null;
    this.onSaved = (s) => (this.saved = s);
    await render(
      hbs`<Secrets::ValueForm @secret={{this.secret}} @onSaved={{this.onSaved}} @onClose={{this.onSaved}} />`
    );
    assert.dom('[data-test-secret-item]').exists({ count: 2 });
    await componentA11yAudit(this.element, assert, STRICT);

    const keys = this.element.querySelectorAll('[data-test-secret-item-key]');
    await fillIn(keys[1], 'a');
    await click('[data-test-secret-form-submit]');
    assert
      .dom('[data-test-secret-form-problems]')
      .includesText('Item keys must be unique')
      .includesText('Every item needs a value');

    await fillIn(keys[1], 'b');
    const values = this.element.querySelectorAll(
      '[data-test-secret-item-value]'
    );
    await fillIn(values[0], 'x');
    await fillIn(values[1], 'y');
    await click('[data-test-secret-toggle-values]');
    assert.dom(values[0]).hasAttribute('type', 'text');
    await click('[data-test-secret-form-submit]');
    const write = server.pretender.handledRequests
      .filter((r) => r.method === 'PUT')
      .pop();
    assert.ok(write.url.includes(`cas=${secret.ModifyIndex}`));
    assert.deepEqual(JSON.parse(write.requestBody), {
      Items: { a: 'x', b: 'y' },
    });
    assert.strictEqual(this.saved.CurrentVersion, 2);
  });

  test('Secrets::SealStatus lists the servers and needs operator read', async function (assert) {
    await render(hbs`<Secrets::SealStatus />`);
    assert.dom('[data-test-seal-status]').includesText('shares (2 of 3)');
    assert
      .dom('[data-test-seal-server="server-2.global"]')
      .exists()
      .includesText('server-2.global');
    assert.dom('[data-test-seal-warning]').exists();
    await componentA11yAudit(this.element, assert, STRICT);

    const token = this.owner.lookup('service:token');
    token.set('aclEnabled', true);
    await render(hbs`<Secrets::SealStatus />`);
    assert.dom('[data-test-seal-status]').doesNotExist();
  });

  test('Objects::Detail shows the deletion of an object being deleted', async function (assert) {
    seedObjects(server);
    const o = server.db.norcaObjects.findBy({ Name: 'billing' });
    server.db.norcaObjects.update(o.id, {
      DeletionTime: Date.now() * 1e6,
      DeletionPropagation: 'foreground',
    });
    await render(
      hbs`<Objects::Detail @group="db.example.com" @kind="PostgresCluster" @name="billing" @namespace="default" />`
    );
    assert.dom('[data-test-object-deletion]').includesText('(foreground)');
    assert.dom('[data-test-object-deleting-badge]').exists();
    assert.dom('[data-test-delete-object]').doesNotExist();
    assert.dom('[data-test-edit-object]').doesNotExist();
    await componentA11yAudit(this.element, assert, STRICT);
  });
});
