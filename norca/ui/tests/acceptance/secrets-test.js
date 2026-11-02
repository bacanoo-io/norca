/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

/* eslint-disable qunit/require-expect */
import {
  click,
  currentURL,
  fillIn,
  visit,
  waitUntil,
  settled,
} from '@ember/test-helpers';
import { module, test } from 'qunit';
import { setupApplicationTest } from 'ember-qunit';
import { setupMirage } from 'ember-cli-mirage/test-support';
import a11yAudit, { STRICT } from 'nomad-ui/tests/helpers/a11y-audit';
import { seedSecrets } from 'nomad-ui/mirage/norca/secrets';
import {
  useToken,
  lastRequest,
  lastBody,
  setEditorText,
  editorJSON,
} from 'nomad-ui/tests/helpers/norca';

const CANARIES = [
  'first-canary-value',
  'second-canary-value',
  'third-canary-value',
  'sk-canary-value',
];

function streamResponse(lines) {
  return Promise.resolve({
    status: 200,
    ok: true,
    body: null,
    text: () =>
      Promise.resolve(lines.map((l) => JSON.stringify(l)).join('\n') + '\n'),
  });
}

function noValueIn(assert, where, text) {
  for (const canary of CANARIES)
    assert.notOk(
      String(text).includes(canary),
      `no secret value in ${where} (${canary})`
    );
}

// The value read requests (GET /v1/secret/<path> without ?metadata).
function valueReads() {
  return server.pretender.handledRequests.filter(
    (r) =>
      r.method === 'GET' &&
      r.url.startsWith('/v1/secret/') &&
      !r.url.includes('metadata')
  );
}

module('Acceptance | secrets', function (hooks) {
  setupApplicationTest(hooks);
  setupMirage(hooks);

  hooks.beforeEach(function () {
    window.localStorage.clear();
    server.create('agent');
    server.create('node-pool');
    server.create('node');
    server.create('namespace', { id: 'staging' });
    seedSecrets(server);
    useToken(null, 'management');
  });

  test('the secrets are listed with metadata only, and the seal status is shown', async function (assert) {
    await visit('/secrets');

    assert.dom('[data-test-page-title]').hasText('Secrets');
    assert.dom('[data-test-gutter-link="secrets"]').exists();
    assert
      .dom('[data-test-secret-list] [data-test-ui-table-row]')
      .exists({ count: 4 });
    const pg = document.querySelector(
      '[data-test-secret-row="prod/db/postgres"]'
    );
    assert.dom(pg.querySelector('[data-test-secret-version]')).hasText('v3');
    assert.dom(pg.querySelector('[data-test-secret-state]')).hasText('active');
    assert
      .dom('[data-test-secret-row="old/token"] [data-test-secret-state]')
      .hasText('deleted');
    assert.dom(pg).includesText('team-billing');

    assert.dom('[data-test-seal-status]').includesText('Seal: shares (2 of 3)');
    assert
      .dom('[data-test-seal-warning]')
      .includesText('1 of 2 servers is sealed');
    assert
      .dom('[data-test-seal-server="server-2.global"]')
      .exists('the servers are listed');
    noValueIn(
      assert,
      'the page',
      document.querySelector('#ember-testing').textContent
    );
    noValueIn(
      assert,
      'the list response',
      lastRequest('GET', '/v1/secrets').responseText
    );
    await a11yAudit(assert, STRICT);

    await fillIn('[data-test-secret-search]', 'stripe');
    assert
      .dom('[data-test-secret-list] [data-test-ui-table-row]')
      .exists({ count: 1 });
  });

  test('a secret shows its metadata and version history; the value only after an explicit reveal', async function (assert) {
    await visit('/secrets');
    await click('[data-test-secret-link="prod/db/postgres"]');
    assert.strictEqual(
      currentURL(),
      '/secrets/s/prod/db/postgres?namespace=default'
    );
    assert
      .dom('[data-test-secret-metadata]')
      .includesText('Credentials of the billing database');
    assert
      .dom('[data-test-secret-metadata]')
      .includesText('password, username');
    assert.dom('[data-test-secret-metadata]').includesText('30 days');
    assert
      .dom('[data-test-secret-versions] [data-test-ui-table-row]')
      .exists({ count: 3 });
    assert
      .dom('[data-test-secret-versions]')
      .includesText('destroyed', 'version 1 was destroyed');
    assert.strictEqual(valueReads().length, 0, 'no value read without a click');
    noValueIn(
      assert,
      'the page',
      document.querySelector('#ember-testing').textContent
    );
    await a11yAudit(assert, STRICT);

    await click('[data-test-secret-reveal-button]');
    assert.strictEqual(valueReads().length, 1, 'one value read');
    assert.dom('[data-test-secret-values]').includesText('third-canary-value');
    assert.notOk(
      window.location.href.includes('canary'),
      'the value is not in the URL'
    );
    const store = this.owner.lookup('service:store');
    assert.notOk(
      store.peekAll('variable').length,
      'nothing is pushed into the store'
    );
    await a11yAudit(assert, STRICT);

    await click('[data-test-secret-hide]');
    assert.dom('[data-test-secret-values]').doesNotExist();
    noValueIn(
      assert,
      'the page after hide',
      document.querySelector('#ember-testing').textContent
    );

    await click('[data-test-show-version="2"]');
    assert.dom('[data-test-secret-value-card]').includesText('Version 2');
    assert.dom('[data-test-secret-values]').doesNotExist('needs a new click');
    await click('[data-test-secret-reveal-button]');
    assert.ok(
      lastRequest('GET', '/v1/secret/prod/db/postgres?').url.includes(
        'version=2'
      )
    );
    assert.dom('[data-test-secret-values]').includesText('second-canary-value');

    // navigating away drops the value
    await click('[data-test-gutter-link="secrets"]');
    await click('[data-test-secret-link="prod/db/postgres"]');
    assert.dom('[data-test-secret-values]').doesNotExist();
    noValueIn(
      assert,
      'the page after navigation',
      document.querySelector('#ember-testing').textContent
    );
  });

  test('restore a version as a new version, delete, undelete and destroy versions', async function (assert) {
    await visit('/secrets/s/prod/db/postgres?namespace=default');

    await click('[data-test-restore-version="2"]');
    assert
      .dom('[data-test-secret-confirm]')
      .includesText('written as version 4');
    await click('[data-test-confirm-submit]');
    const write = lastRequest('PUT', '/v1/secret/prod/db/postgres');
    assert.ok(write.url.includes('cas='), 'conditional on the check index');
    assert.deepEqual(JSON.parse(write.requestBody).Items, {
      username: 'billing',
      password: 'second-canary-value',
    });
    assert
      .dom('[data-test-secret-versions] [data-test-ui-table-row]')
      .exists({ count: 4 });
    noValueIn(
      assert,
      'the page',
      document.querySelector('#ember-testing').textContent
    );

    await click('[data-test-delete-version="3"]');
    await click('[data-test-confirm-submit]');
    assert.ok(
      lastRequest('DELETE', '/v1/secret/prod/db/postgres').url.includes(
        'versions=3'
      )
    );
    await click('[data-test-undelete-version="3"]');
    await click('[data-test-confirm-submit]');
    assert.ok(
      lastRequest('POST', '/v1/secret/prod/db/postgres/undelete').url.includes(
        'versions=3'
      )
    );

    await click('[data-test-destroy-version="2"]');
    assert.dom('[data-test-confirm-submit]').isDisabled('typed confirmation');
    await fillIn('[data-test-confirm-typed]', 'v2');
    await click('[data-test-confirm-submit]');
    assert.ok(
      lastRequest('POST', '/v1/secret/prod/db/postgres/destroy').url.includes(
        'versions=2'
      )
    );

    await click('[data-test-destroy-secret]');
    await fillIn('[data-test-confirm-typed]', 'prod/db/postgres');
    await click('[data-test-confirm-submit]');
    assert.strictEqual(currentURL(), '/secrets');
    assert.dom('[data-test-secret-row="prod/db/postgres"]').doesNotExist();
  });

  test('create a secret and write a new version with a check index', async function (assert) {
    await visit('/secrets');
    await click('[data-test-new-secret]');
    assert.dom('[data-test-secret-form]').exists();
    assert
      .dom('[data-test-secret-item-value]')
      .hasAttribute(
        'type',
        'password',
        'values are typed into password fields'
      );
    await a11yAudit(assert, STRICT);

    await fillIn('[data-test-secret-path]', '/bad/');
    await click('[data-test-secret-form-submit]');
    assert
      .dom('[data-test-secret-form-problems]')
      .includesText('path is not valid');

    await fillIn('[data-test-secret-path]', 'prod/cache/redis');
    await fillIn('[data-test-secret-description]', 'Cache password');
    await fillIn('[data-test-secret-item-key]', 'password');
    await fillIn('[data-test-secret-item-value]', 'new-canary-value');
    await click('[data-test-secret-form-submit]');
    const create = lastRequest('PUT', '/v1/secret/prod/cache/redis');
    assert.ok(create.url.includes('cas=0'), 'a new secret must not exist');
    assert.deepEqual(JSON.parse(create.requestBody), {
      Items: { password: 'new-canary-value' },
      Meta: { Description: 'Cache password', Owner: '' },
    });
    assert.strictEqual(
      currentURL(),
      '/secrets/s/prod/cache/redis?namespace=default'
    );
    assert.notOk(
      document
        .querySelector('#ember-testing')
        .textContent.includes('new-canary-value'),
      'the value is not shown after the write'
    );

    await click('[data-test-new-secret-version]');
    assert
      .dom('[data-test-secret-item-key]')
      .hasValue('password', 'the keys of the current version are kept');
    assert.dom('[data-test-secret-item-value]').hasValue('');
    await click('[data-test-secret-fill]');
    assert
      .dom('[data-test-secret-item-value]')
      .hasValue('new-canary-value', 'filled in on request');
    await click('[data-test-secret-item-add]');
    const keys = document.querySelectorAll('[data-test-secret-item-key]');
    const values = document.querySelectorAll('[data-test-secret-item-value]');
    await fillIn(keys[1], 'user');
    await fillIn(values[1], 'cache');
    await click('[data-test-secret-form-submit]');
    const version = lastRequest('PUT', '/v1/secret/prod/cache/redis');
    assert.notOk(version.url.includes('cas=0'));
    assert.ok(/cas=\d+/.test(version.url));
    assert.deepEqual(Object.keys(JSON.parse(version.requestBody).Items), [
      'password',
      'user',
    ]);
    assert.dom('[data-test-secret-metadata]').includesText('v2');
  });

  test('a write that conflicts says so', async function (assert) {
    await visit('/secrets/s/prod/api/stripe?namespace=default');
    // another writer changes the secret meanwhile
    const s = server.db.norcaSecrets.findBy({ Path: 'prod/api/stripe' });
    server.db.norcaSecrets.update(s.id, { ModifyIndex: 9999 });

    await click('[data-test-new-secret-version]');
    await fillIn('[data-test-secret-item-value]', 'x');
    await click('[data-test-secret-form-submit]');
    assert
      .dom('[data-test-secret-form-error]')
      .includesText('changed since the page loaded it');
  });

  test('metadata is edited in the unified editor', async function (assert) {
    await visit('/secrets/s/prod/api/stripe?namespace=default');
    await click('[data-test-edit-secret-meta]');
    const doc = editorJSON();
    assert.strictEqual(doc.Owner, 'team-payments');
    await setEditorText({ ...doc, MaxVersions: 500 });
    assert.dom('[data-test-editor-problem]').includesText('MaxVersions');
    await setEditorText({
      ...doc,
      Owner: 'team-checkout',
      Labels: { pci: 'yes' },
    });
    await click('[data-test-editor-review-button]');
    await click('[data-test-editor-submit]');
    const body = lastBody('PUT', '/v1/secret/prod/api/stripe');
    assert.strictEqual(body.Meta.Owner, 'team-checkout');
    assert.notOk('Items' in body, 'no version is written');
    assert.dom('[data-test-secret-metadata]').includesText('team-checkout');
    assert.dom('[data-test-secret-detail]').includesText('pci=yes');
  });

  test('the jobs that reference a path are listed', async function (assert) {
    const job = server.create('job', {
      id: 'billing-api',
      name: 'billing-api',
      namespaceId: 'default',
      createAllocations: false,
      groupsCount: 1,
    });
    // the task of the job reads the secret with a secret block
    const task = server.schema.jobs.find(job.id).taskGroups.models[0].tasks
      .models[0];
    task.update({ secrets: [{ Name: 'db', Path: 'prod/db/postgres' }] });
    await visit('/secrets/s/prod/db/postgres?namespace=default');
    await waitUntil(() => document.querySelector('[data-test-secret-job]'));
    assert
      .dom('[data-test-secret-job="billing-api"]')
      .exists('the job is linked');
    assert.dom('[data-test-secret-jobs]').includesText('secret "db"');

    await visit('/secrets/s/prod/api/stripe?namespace=default');

    assert
      .dom('[data-test-secret-jobs]')
      .includesText('No job of the namespace references this path');
  });

  test('last access comes from the SecretAccess audit records', async function (assert) {
    const eventStream = this.owner.lookup('service:event-stream');
    eventStream.enabled = true;
    const urls = [];
    let served = false;
    eventStream.request = (url) => {
      urls.push(url);
      if (served || !url.includes('SecretAccess')) return new Promise(() => {});
      served = true;
      return streamResponse([
        {
          Index: 50,
          Events: [
            {
              Topic: 'SecretAccess',
              Type: 'SecretAccessed',
              Key: 'prod/api/stripe',
              Namespace: 'default',
              Index: 50,
              Payload: {
                Time: new Date().toISOString(),
                Namespace: 'default',
                Path: 'prod/api/stripe',
                Version: 1,
                Operation: 'read',
                Result: 'ok',
                Accessor: {
                  JobID: 'checkout',
                  TaskGroup: 'web',
                  Task: 'app',
                },
              },
            },
          ],
        },
      ]);
    };

    await visit('/secrets');
    await waitUntil(() =>
      document
        .querySelector(
          '[data-test-secret-row="prod/api/stripe"] [data-test-secret-last-access]'
        )
        ?.textContent.includes('read by')
    );
    assert
      .dom(
        '[data-test-secret-row="prod/api/stripe"] [data-test-secret-last-access]'
      )
      .includesText('read by task checkout/web/app');
    assert.ok(
      urls.some(
        (u) => u.includes('topic=SecretAccess') && u.includes('index=1')
      ),
      'the buffered audit records are replayed'
    );
    for (const { connection } of eventStream.connections.values())
      connection.stop();
    await settled();
  });

  test('a token without read-secret sees metadata but cannot reveal; without write it cannot change', async function (assert) {
    useToken({
      Namespaces: [
        {
          Name: 'default',
          Capabilities: ['list-jobs'],
          Secrets: [{ PathSpec: 'prod/*', Capabilities: ['list-secrets'] }],
        },
      ],
    });
    await visit('/secrets');
    assert.dom('[data-test-gutter-link="secrets"]').exists();
    assert.dom('[data-test-new-secret]').doesNotExist();
    assert.dom('[data-test-seal-status]').doesNotExist('needs operator read');

    await visit('/secrets/s/prod/api/stripe?namespace=default');
    assert.dom('[data-test-secret-reveal-denied]').exists();
    assert.dom('[data-test-secret-reveal-button]').doesNotExist();
    assert.dom('[data-test-new-secret-version]').doesNotExist();
    assert.dom('[data-test-destroy-secret]').doesNotExist();
    assert.dom('[data-test-delete-version="1"]').doesNotExist();
  });

  test('a token without any secret capability has no Secrets menu item', async function (assert) {
    useToken({
      Namespaces: [{ Name: 'default', Capabilities: ['list-jobs'] }],
    });
    await visit('/');
    assert.dom('[data-test-gutter-link="secrets"]').doesNotExist();
  });

  test('the dashboard shows the seal and warns about sealed servers', async function (assert) {
    await visit('/');
    assert.dom('[data-test-region-seal]').includesText('shares (2 of 3)');
    assert.dom('[data-test-region-seal]').includesText('1 sealed');
    assert.dom('[data-test-dashboard]').includesText('1 server is sealed');
  });
});
