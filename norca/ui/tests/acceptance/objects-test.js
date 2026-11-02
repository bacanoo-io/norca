/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

/* eslint-disable qunit/require-expect */
import { click, currentURL, fillIn, select, visit } from '@ember/test-helpers';
import { module, test } from 'qunit';
import { setupApplicationTest } from 'ember-qunit';
import { setupMirage } from 'ember-cli-mirage/test-support';
import a11yAudit, { STRICT } from 'nomad-ui/tests/helpers/a11y-audit';
import { seedObjects } from 'nomad-ui/mirage/norca/objects';
import { seedReleases } from 'nomad-ui/mirage/norca/releases';
import {
  useToken,
  lastRequest,
  lastBody,
  setEditorText,
  editorJSON,
} from 'nomad-ui/tests/helpers/norca';

module('Acceptance | objects and type definitions', function (hooks) {
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

  test('objects are listed with the columns of their type definition', async function (assert) {
    await visit('/objects');

    assert.dom('[data-test-page-title]').hasText('Objects');
    assert.dom('[data-test-gutter-link="objects"]').exists();
    assert
      .dom('[data-test-object-list] [data-test-ui-table-header="col-0"]')
      .hasText('Version');
    assert
      .dom('[data-test-object-list] [data-test-ui-table-row]')
      .exists({ count: 3 });
    const billing = [
      ...document.querySelectorAll(
        '[data-test-object-list] [data-test-ui-table-row]'
      ),
    ].find((r) => r.textContent.includes('billing'));
    const cells = [...billing.querySelectorAll('[data-test-object-cell]')].map(
      (c) => c.textContent.trim()
    );
    assert.deepEqual(cells, ['16', '3', 'True']);
    assert
      .dom(billing.querySelector('[data-test-object-health]'))
      .hasText('ready');
    const reports = [
      ...document.querySelectorAll(
        '[data-test-object-list] [data-test-ui-table-row]'
      ),
    ].find((r) => r.textContent.includes('reports'));
    assert
      .dom(reports.querySelector('[data-test-object-health]'))
      .hasText('not ready');
    assert
      .dom(reports.querySelector('[data-test-object-generation]'))
      .hasText('4 / 3');
    await a11yAudit(assert, STRICT);

    await select('[data-test-kind-select]', 'backup.example.com/Schedule');
    assert.ok(currentURL().includes('kind=backup.example.com'));
    assert
      .dom('[data-test-object-list] [data-test-ui-table-empty]')
      .includesText('No object of this type');
  });

  test('an object shows spec, status, conditions, owners and its event log', async function (assert) {
    await visit('/objects');
    await click('[data-test-object-link="reports"]');
    assert.strictEqual(
      currentURL(),
      '/objects/db.example.com/PostgresCluster/reports?namespace=default'
    );

    assert.dom('[data-test-object-health]').hasText('not ready');
    assert
      .dom('[data-test-object-behind]')
      .includesText('generation 4, observed 3');
    assert.dom('[data-test-object-spec]').includesText('"replicas": 2');
    assert.dom('[data-test-object-status]').includesText('Observed generation');
    assert
      .dom('[data-test-object-conditions] [data-test-ui-table-row]')
      .exists({ count: 2 });
    assert.dom('[data-test-object-events]').includesText('ReplicaFailed (3×)');
    await a11yAudit(assert, STRICT);

    await visit(
      '/objects/db.example.com/PostgresCluster/test-db?namespace=staging'
    );
    assert.dom('[data-test-object-owner]').includesText('Release test-stack');
  });

  test('an unknown object says what to do', async function (assert) {
    await visit(
      '/objects/db.example.com/PostgresCluster/nope?namespace=default'
    );
    assert
      .dom('[data-test-object-error]')
      .includesText('may have been deleted');
  });

  test('type definitions list scope, objects, rules, webhooks and the package', async function (assert) {
    await visit('/kinds');
    assert.dom('[data-test-page-title]').hasText('Type definitions');
    assert
      .dom('[data-test-kind-list] [data-test-ui-table-row]')
      .exists({ count: 2 });
    const pg = [
      ...document.querySelectorAll(
        '[data-test-kind-list] [data-test-ui-table-row]'
      ),
    ].find((r) => r.textContent.includes('PostgresCluster'));
    assert.dom(pg.querySelector('[data-test-kind-objects]')).hasText('3');
    assert
      .dom(pg.querySelector('[data-test-kind-package]'))
      .hasText('postgres-operator');
    assert.dom(pg).includesText('yes (spec.replicas)');
    await a11yAudit(assert, STRICT);

    await click('[data-test-kind-link="db.example.com/PostgresCluster"]');
    assert.strictEqual(currentURL(), '/kinds/db.example.com/PostgresCluster');
    assert.dom('[data-test-kind-schema]').includesText('"maximum": 5');
    assert
      .dom('[data-test-kind-rules]')
      .includesText('replicas cannot drop below 2');
    assert
      .dom('[data-test-kind-columns] [data-test-ui-table-row]')
      .exists({ count: 3 });
    assert.dom('[data-test-webhook-approved]').includesText('approved');
    assert.dom('[data-test-kind-limits]').includesText('at most 50');
    await a11yAudit(assert, STRICT);
  });

  test('admission policies: list, rules and delete with a typed name', async function (assert) {
    await visit('/admission');
    assert.dom('[data-test-page-title]').hasText('Admission policies');
    assert
      .dom('[data-test-admission-policies] [data-test-ui-table-row]')
      .exists({ count: 2 });
    assert
      .dom('[data-test-admission-policies]')
      .includesText('namespaces prod*');
    await a11yAudit(assert, STRICT);

    await click('[data-test-admission-link="no-privileged-containers"]');
    assert
      .dom('[data-test-admission-details]')
      .includesText('deny: the write is rejected');
    assert
      .dom('[data-test-admission-rules]')
      .includesText("t.Driver == 'docker'");
    await a11yAudit(assert, STRICT);

    await click('[data-test-delete-admission]');
    await fillIn('[data-test-confirm-typed]', 'no-privileged-containers');
    await click('[data-test-confirm-submit]');
    assert.ok(
      lastRequest('DELETE', '/v1/admission-policy/no-privileged-containers')
    );
    assert.strictEqual(currentURL(), '/admission');
    assert
      .dom('[data-test-admission-policies] [data-test-ui-table-row]')
      .exists({ count: 1 });
  });

  test('admission policies: create with the CEL editor, compile errors are shown', async function (assert) {
    await visit('/admission');
    await click('[data-test-new-admission]');
    assert.dom('[data-test-admission-editor]').exists();
    assert.dom('[data-test-cel-reference]').includesText('request');
    assert.strictEqual(editorJSON().Action, 'warn', 'starts from a template');
    await a11yAudit(assert, STRICT);

    const policy = {
      Name: 'max-count',
      Action: 'deny',
      Match: { Resources: ['job'] },
      Rules: [
        {
          Expression: 'job.TaskGroups.all(tg, tg.Count <= 10',
          Message: 'At most 10 instances per group.',
        },
      ],
    };
    await setEditorText({ ...policy, Action: 'block' });
    assert
      .dom('[data-test-editor-problem]')
      .includesText('Action: must be one of', 'the schema is checked locally');
    assert.dom('[data-test-editor-review-button]').isDisabled();

    await setEditorText(policy);
    await click('[data-test-editor-review-button]');
    assert.dom('[data-test-editor-review] [data-test-text-diff]').exists();
    await click('[data-test-editor-submit]');
    assert
      .dom('[data-test-editor-submit-error]')
      .includesText('Syntax error', 'the CEL compile error is shown');

    await click('[data-test-editor-back]');
    policy.Rules[0].Expression = 'job.TaskGroups.all(tg, tg.Count <= 10)';
    await setEditorText(policy);
    await click('[data-test-editor-review-button]');
    await click('[data-test-editor-submit]');
    assert.strictEqual(currentURL(), '/admission/max-count');
    assert.deepEqual(lastBody('PUT', '/v1/admission-policy/max-count'), policy);
    assert.dom('[data-test-admission-rules]').includesText('tg.Count <= 10');
  });

  test('admission policies: edit keeps the name', async function (assert) {
    await visit('/admission/team-label');
    await click('[data-test-edit-admission]');
    const doc = editorJSON();
    assert.strictEqual(doc.Name, 'team-label');
    assert.notOk('ModifyIndex' in doc, 'the indexes are left out');

    await setEditorText({ ...doc, Name: 'renamed' });
    await click('[data-test-editor-review-button]');
    assert.dom('[data-test-editor-problem]').includesText('cannot be renamed');

    await setEditorText({ ...doc, Action: 'deny' });
    await click('[data-test-editor-review-button]');
    await click('[data-test-editor-submit]');
    assert.dom('[data-test-admission-editor]').doesNotExist();
    assert
      .dom('[data-test-admission-details]')
      .includesText('deny: the write is rejected');
  });

  test('objects: edit with schema validation and a dry run', async function (assert) {
    await visit(
      '/objects/db.example.com/PostgresCluster/billing?namespace=default'
    );
    await click('[data-test-edit-object]');
    assert.dom('[data-test-object-editor]').exists();
    const doc = editorJSON();
    assert.deepEqual(Object.keys(doc), ['Name', 'Namespace', 'Labels', 'Spec']);
    await a11yAudit(assert, STRICT);

    await setEditorText({ ...doc, Spec: { ...doc.Spec, replicas: 9 } });
    assert
      .dom('[data-test-editor-problem]')
      .includesText('Spec.replicas: must be at most 5');
    assert.dom('[data-test-editor-review-button]').isDisabled();

    await setEditorText({ ...doc, Spec: { ...doc.Spec, replicas: 4 } });
    await click('[data-test-editor-review-button]');
    assert.dom('[data-test-object-dry-run]').includesText('generation 8');
    const dryRun = lastRequest(
      'PUT',
      '/v1/object/db.example.com/PostgresCluster/billing'
    );
    assert.ok(dryRun.url.includes('dry_run=true'), 'the review is a dry run');
    assert.dom('[data-test-object-dry-run-diff]').exists();

    await click('[data-test-editor-submit]');
    const write = lastRequest(
      'PUT',
      '/v1/object/db.example.com/PostgresCluster/billing'
    );
    assert.notOk(write.url.includes('dry_run'), 'then the write');
    assert.ok(write.url.includes('cas='), 'conditional on the edited version');
    assert.dom('[data-test-object-editor]').doesNotExist();
    assert.dom('[data-test-object-spec]').includesText('"replicas": 4');
  });

  test('objects: a new object gets the schema defaults in the dry run', async function (assert) {
    await visit('/objects');
    await click('[data-test-new-object]');
    await setEditorText({
      Name: 'orders',
      Namespace: 'default',
      Labels: {},
      Spec: { version: '16', replicas: 2 },
    });
    await click('[data-test-editor-review-button]');
    assert
      .dom('[data-test-object-dry-run]')
      .includesText('schema defaults filled in');
    assert.dom('[data-test-object-dry-run-diff]').includesText('10GiB');
    await click('[data-test-editor-submit]');
    assert.strictEqual(
      currentURL(),
      '/objects/db.example.com/PostgresCluster/orders?namespace=default'
    );
  });

  test('type definitions: the plan reports stored objects a schema change breaks', async function (assert) {
    await visit('/kinds/db.example.com/PostgresCluster');
    await click('[data-test-edit-kind]');
    assert.dom('[data-test-kind-editor]').exists();
    const doc = editorJSON();
    assert.strictEqual(
      typeof doc.Schema,
      'object',
      'the schema is edited as JSON'
    );
    // CodeMirror scrolls a long document; the keyboard reaches it through
    // the editor's text area.
    await a11yAudit(assert, STRICT, 'scrollable-region-focusable');

    const narrow = JSON.parse(JSON.stringify(doc));
    narrow.Schema.properties.spec.properties.replicas.maximum = 2;
    await setEditorText(narrow);
    await click('[data-test-editor-review-button]');
    assert
      .dom('[data-test-kind-plan-failures]')
      .includesText('billing', 'billing has 3 replicas');
    await click('[data-test-editor-submit]');
    assert.dom('[data-test-editor-submit-error]').includesText('invalid');

    await click('[data-test-editor-back]');
    const wide = JSON.parse(JSON.stringify(doc));
    wide.Schema.properties.spec.properties.replicas.maximum = 9;
    await setEditorText(wide);
    await click('[data-test-editor-review-button]');
    assert.dom('[data-test-kind-plan-ok]').includesText('3 stored objects');
    await click('[data-test-editor-submit]');
    const body = lastBody(
      'PUT',
      '/v1/object-kind/db.example.com/PostgresCluster'
    );
    assert.strictEqual(typeof body.Schema, 'string', 'sent as a string');
    assert.dom('[data-test-kind-schema]').includesText('"maximum": 9');
  });

  test('a token without operator write cannot edit type definitions', async function (assert) {
    useToken({
      Operator: { Policy: 'read' },
      Namespaces: [{ Name: 'default', Capabilities: ['list-jobs'] }],
    });
    await visit('/kinds/db.example.com/PostgresCluster');
    assert.dom('[data-test-edit-kind]').doesNotExist();
    await visit('/kinds');
    assert.dom('[data-test-new-kind]').doesNotExist();
  });

  test('a token without admission rights does not see the menu item', async function (assert) {
    useToken({
      Namespaces: [{ Name: 'default', Capabilities: ['list-jobs'] }],
    });
    await visit('/');
    assert.dom('[data-test-gutter-link="admission"]').doesNotExist();
    assert.dom('[data-test-gutter-link="kinds"]').exists();
  });
});
