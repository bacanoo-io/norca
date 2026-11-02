/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

/* eslint-disable qunit/require-expect */
import { click, currentURL, fillIn, visit } from '@ember/test-helpers';
import { module, test } from 'qunit';
import { setupApplicationTest } from 'ember-qunit';
import { setupMirage } from 'ember-cli-mirage/test-support';
import a11yAudit, { STRICT } from 'nomad-ui/tests/helpers/a11y-audit';
import { seedObjects } from 'nomad-ui/mirage/norca/objects';
import { seedReleases } from 'nomad-ui/mirage/norca/releases';
import { useToken, lastRequest } from 'nomad-ui/tests/helpers/norca';

const BILLING =
  '/objects/db.example.com/PostgresCluster/billing?namespace=default';

function kind(db) {
  return db.norcaObjectKinds.findBy({ Kind: 'PostgresCluster' });
}

module('Acceptance | object lifecycle and webhooks', function (hooks) {
  setupApplicationTest(hooks);
  setupMirage(hooks);

  hooks.beforeEach(function () {
    window.localStorage.clear();
    server.create('agent');
    server.create('node-pool');
    server.create('node');
    seedObjects(server);
    seedReleases(server);
    useToken(null, 'management');
  });

  test('an object shows its schema version, scale, finalizers and what it owns', async function (assert) {
    // a job and an object owned by billing
    server.create('job', {
      id: 'billing-db',
      name: 'billing-db',
      namespaceId: 'default',
      createAllocations: false,
      shallow: true,
      meta: {
        'norca.owner': 'default/crd/db-example-com/PostgresCluster/billing',
        'norca.owner.kind': 'db.example.com/PostgresCluster',
      },
    });
    server.create('job', {
      id: 'other',
      name: 'other',
      namespaceId: 'default',
      createAllocations: false,
      shallow: true,
    });
    server.db.norcaObjects.insert({
      ID: 'obj-9',
      Group: 'backup.example.com',
      Kind: 'Schedule',
      Namespace: '',
      Name: 'billing-nightly',
      Owners: [
        {
          Kind: 'db.example.com/PostgresCluster',
          Namespace: 'default',
          Name: 'billing',
          ID: 'obj-1',
        },
      ],
      Generation: 1,
      Spec: {},
      Status: {},
      Events: [],
    });

    await visit(BILLING);
    assert.dom('[data-test-object-status]').includesText('Schema version');
    assert.dom('[data-test-object-status]').includesText('v2');
    assert.dom('[data-test-object-scale]').includesText('3 (spec.replicas)');
    assert
      .dom('[data-test-object-scale]')
      .includesText('3 (status.readyReplicas)');
    assert.dom('[data-test-object-finalizer]').hasText('db.example.com/backup');
    assert.dom('[data-test-owned-job="billing-db"]').exists();
    assert.dom('[data-test-owned-job="other"]').doesNotExist();
    assert.ok(
      server.pretender.handledRequests.some(
        (r) =>
          r.url.startsWith('/v1/jobs?') &&
          new URL(r.url, window.location.origin).searchParams.get('filter') ===
            'Meta["norca.owner"] == "default/crd/db-example-com/PostgresCluster/billing"'
      ),
      'the owned jobs are found by their owner meta'
    );
    assert.dom('[data-test-owned-object="billing-nightly"]').exists();
    await a11yAudit(assert, STRICT);

    await click('[data-test-owned-object="billing-nightly"] a');
    assert.strictEqual(
      currentURL(),
      '/objects/backup.example.com/Schedule/billing-nightly'
    );
    assert
      .dom('[data-test-object-owner-link]')
      .hasText('PostgresCluster billing');
    await click('[data-test-object-owner-link]');
    assert.strictEqual(currentURL(), BILLING);
  });

  test('the job page links to the object that owns the job', async function (assert) {
    server.create('job', {
      id: 'billing-db',
      name: 'billing-db',
      namespaceId: 'default',
      createAllocations: false,
      shallow: true,
      meta: {
        'norca.owner': 'default/crd/db-example-com/PostgresCluster/billing',
        'norca.owner.kind': 'db.example.com/PostgresCluster',
      },
    });
    await visit('/jobs/billing-db@default');
    assert.dom('[data-test-job-owner]').includesText('PostgresCluster billing');
    assert
      .dom('[data-test-job-owner-note]')
      .includesText('an object of kind PostgresCluster');
    await click('[data-test-job-owner-link]');
    assert.strictEqual(currentURL(), BILLING);
  });

  test('deleting an object with finalizers marks it; the mode is sent', async function (assert) {
    await visit(BILLING);
    await click('[data-test-delete-object]');
    assert.dom('[data-test-object-delete-confirm]').exists();
    assert.dom('[data-test-confirm-submit]').isDisabled();
    await a11yAudit(assert, STRICT);

    await click('[data-test-radio-option="orphan"]');
    assert
      .dom('[data-test-deletion-mode]')
      .includesText('keep running', 'the mode is explained');
    await fillIn('[data-test-confirm-typed]', 'billing');
    await click('[data-test-confirm-submit]');
    const del = lastRequest(
      'DELETE',
      '/v1/object/db.example.com/PostgresCluster/billing'
    );
    assert.ok(del.url.includes('propagation=orphan'));
    assert.ok(del.url.includes('cas='));
    assert.dom('[data-test-object-deletion]').includesText('(orphan)');
    assert
      .dom('[data-test-object-deletion]')
      .includesText('db.example.com/backup');
    assert.dom('[data-test-object-health]').hasText('deleting');
    assert.dom('[data-test-delete-object]').doesNotExist();
  });

  test('deleting an object without finalizers removes it', async function (assert) {
    const o = server.db.norcaObjects.findBy({ Name: 'reports' });
    server.db.norcaObjects.update(o.id, { Finalizers: [] });
    await visit(
      '/objects/db.example.com/PostgresCluster/reports?namespace=default'
    );
    await click('[data-test-delete-object]');
    await fillIn('[data-test-confirm-typed]', 'reports');
    await click('[data-test-confirm-submit]');
    assert.ok(
      lastRequest(
        'DELETE',
        '/v1/object/db.example.com/PostgresCluster/reports'
      ).url.includes('propagation=background')
    );
    assert.ok(currentURL().startsWith('/objects?kind=db.example.com'));
    assert.dom('[data-test-object-row="reports"]').doesNotExist();
  });

  test('a management token approves and revokes webhooks', async function (assert) {
    const k = kind(server.db);
    server.db.norcaObjectKinds.update(k.id, {
      Webhooks: [
        ...k.Webhooks,
        {
          Name: 'validate',
          Mode: 'validate',
          URL: 'https://pg-operator.platform:8443/validate',
          FailurePolicy: 'fail',
          Operations: ['create', 'update', 'delete'],
          Timeout: 10e9,
          Approval: null,
        },
      ],
    });
    await visit('/kinds/db.example.com/PostgresCluster');
    assert
      .dom('[data-test-kind-pending-webhooks]')
      .includesText('1 webhook waits for approval');
    await a11yAudit(assert, STRICT);

    await click('[data-test-approve-webhook="validate"]');
    assert
      .dom('[data-test-webhook-confirm]')
      .includesText('https://pg-operator.platform:8443/validate');
    await click('[data-test-confirm-submit]');
    assert.ok(
      lastRequest(
        'PUT',
        '/v1/object-kind/db.example.com/PostgresCluster/webhook/validate/approve'
      )
    );
    assert.dom('[data-test-kind-pending-webhooks]').doesNotExist();

    await click('[data-test-revoke-webhook="defaults"]');
    await click('[data-test-confirm-submit]');
    assert.ok(
      lastRequest(
        'PUT',
        '/v1/object-kind/db.example.com/PostgresCluster/webhook/defaults/revoke'
      )
    );
    assert
      .dom('[data-test-kind-pending-webhooks]')
      .includesText('1 webhook waits');
  });

  test('without a management token webhooks cannot be approved', async function (assert) {
    useToken({
      Operator: { Policy: 'write' },
      Namespaces: [{ Name: 'default', Capabilities: ['list-jobs'] }],
    });
    const k = kind(server.db);
    server.db.norcaObjectKinds.update(k.id, {
      Webhooks: k.Webhooks.map((w) => ({ ...w, Approval: null })),
    });
    await visit('/kinds/db.example.com/PostgresCluster');
    assert
      .dom('[data-test-kind-pending-webhooks]')
      .includesText('Ask an administrator');
    assert.dom('[data-test-approve-webhook]').doesNotExist();
    assert.dom('[data-test-edit-kind]').exists('operator write still edits');
  });

  test('a type definition shows its limits and the release that installed it, and is deleted with a typed name', async function (assert) {
    const k = kind(server.db);
    server.db.norcaObjectKinds.update(k.id, {
      Limits: {
        WriteRate: 5,
        WriteBurst: 10,
        IdentityWriteRate: 1,
        IdentityWriteBurst: 2,
        MaxObjectsPerNamespace: 50,
        MaxWatchesPerIdentity: 4,
      },
      Migration: { Spec: 'self.spec.merge({"storage": "10GiB"})', Status: '' },
    });
    await visit('/kinds/db.example.com/PostgresCluster');
    assert
      .dom('[data-test-kind-limits]')
      .includesText('5 per second, burst 10');
    assert.dom('[data-test-kind-limits]').includesText('1 per second, burst 2');
    assert.dom('[data-test-kind-limits]').includesText('at most 4 blocking');
    assert
      .dom('[data-test-kind-owner]')
      .includesText('Release postgres-operator');
    assert.dom('[data-test-kind-migration]').includesText('self.spec.merge');
    assert.dom('[data-test-kind-details]').includesText('v2');

    await click('[data-test-delete-kind]');
    await fillIn('[data-test-confirm-typed]', 'db.example.com/PostgresCluster');
    await click('[data-test-confirm-submit]');
    assert
      .dom('[data-test-confirm-error]')
      .includesText('still has objects', 'the server refuses it');
    await click('[data-test-confirm-cancel]');

    await visit('/kinds/backup.example.com/Schedule');
    assert.dom('[data-test-kind-limits]').includesText('No limits');
    await click('[data-test-delete-kind]');
    await fillIn('[data-test-confirm-typed]', 'backup.example.com/Schedule');
    await click('[data-test-confirm-submit]');

    assert.strictEqual(currentURL(), '/kinds');
    assert
      .dom('[data-test-kind-list] [data-test-ui-table-row]')
      .exists({ count: 1 });
  });

  test('the object list shows the scale and a deleting object', async function (assert) {
    const o = server.db.norcaObjects.findBy({ Name: 'reports' });
    server.db.norcaObjects.update(o.id, { DeletionTime: Date.now() * 1e6 });
    await visit('/objects');
    assert
      .dom('[data-test-object-row="billing"] [data-test-object-scale]')
      .hasText('3 / 3');
    assert
      .dom('[data-test-object-row="reports"] [data-test-object-health]')
      .hasText('deleting');
  });
});
