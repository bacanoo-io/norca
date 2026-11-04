/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

/* eslint-disable qunit/require-expect */
import { click, fillIn, select, visit } from '@ember/test-helpers';
import { module, test } from 'qunit';
import { setupApplicationTest } from 'ember-qunit';
import { setupMirage } from 'ember-cli-mirage/test-support';
import a11yAudit, { STRICT } from 'nomad-ui/tests/helpers/a11y-audit';
import { seedNetwork } from 'nomad-ui/mirage/norca/network';
import {
  useToken,
  lastBody,
  lastRequest,
  setEditorText,
} from 'nomad-ui/tests/helpers/norca';
import {
  parseDuration,
  formatDuration,
} from 'nomad-ui/components/endpoints/page';

const ROWS = '[data-test-endpoint-list] [data-test-ui-table-row]';

module('Acceptance | endpoints and domains', function (hooks) {
  setupApplicationTest(hooks);
  setupMirage(hooks);

  hooks.beforeEach(function () {
    window.localStorage.clear();
    server.create('agent');
    server.create('node-pool');
    server.create('node', { status: 'ready' });
  });

  test('durations are read and written as Go durations', function (assert) {
    assert.strictEqual(parseDuration('30s'), 30e9);
    assert.strictEqual(parseDuration('1h30m'), 5400e9);
    assert.strictEqual(parseDuration('0'), 0);
    assert.strictEqual(parseDuration(''), null);
    assert.strictEqual(parseDuration('10 s'), null);
    assert.strictEqual(parseDuration('soon'), null);
    assert.strictEqual(formatDuration(5400e9), '1h30m');
    assert.strictEqual(formatDuration(2e9), '2s');
    assert.strictEqual(formatDuration(0), '');
  });

  test('endpoints: list, create with a node for the checks, change and delete', async function (assert) {
    useToken(null, 'management');
    seedNetwork(server);
    const node = server.db.nodes[0];
    await visit('/endpoints');

    assert.dom('[data-test-page-title]').hasText('Endpoints and domains');
    assert.dom('[data-test-page-pretitle]').hasText('Services and networking');
    assert.dom('[data-test-gutter-link="endpoints"]').exists();
    assert.dom(ROWS).exists({ count: 1 });
    assert
      .dom('[data-test-endpoint-target]')
      .hasText('billing.example.com:443');
    await a11yAudit(assert, STRICT);

    // a domain: a DNS alias without checks
    await click('[data-test-add-endpoint]');
    await a11yAudit(assert, STRICT);
    await fillIn('[data-test-endpoint-name-input]', 'reports-db');
    await select('[data-test-endpoint-kind]', 'alias');
    await click('[data-test-endpoint-save]');
    assert
      .dom('[data-test-endpoint-save-error]')
      .includesText('the DNS name it points to');
    await fillIn(
      '[data-test-endpoint-target-input]',
      'reports.c9xk2.eu-central-1.rds.amazonaws.com'
    );
    await fillIn('[data-test-endpoint-tags]', 'db, reports');
    await click('[data-test-endpoint-save]');
    let body = lastBody('PUT', '/v1/service/external/reports-db');
    assert.strictEqual(body.Kind, 'alias');
    assert.strictEqual(
      body.Target,
      'reports.c9xk2.eu-central-1.rds.amazonaws.com'
    );
    assert.deepEqual(body.Tags, ['db', 'reports']);
    assert.deepEqual(body.Checks, []);
    assert.strictEqual(body.DeregisterCriticalAfter, 3600e9);
    assert.dom(ROWS).exists({ count: 2 });

    // an endpoint with an http check needs the node that runs it
    await click('[data-test-add-endpoint]');
    await fillIn('[data-test-endpoint-name-input]', 'portal');
    await fillIn('[data-test-endpoint-address]', '10.0.0.7');
    await fillIn('[data-test-endpoint-port]', '443');
    await fillIn('[data-test-endpoint-meta]', 'team=web\nowner=ops');
    await click('[data-test-add-check]');
    await select('[data-test-check-type]', 'http');
    await fillIn('[data-test-check-http]', 'https://10.0.0.7/health');
    await click('[data-test-endpoint-save]');
    assert
      .dom('[data-test-endpoint-save-error]')
      .includesText('choose the node');
    await select('[data-test-endpoint-node]', node.id);
    await click('[data-test-endpoint-save]');
    body = lastBody('PUT', '/v1/service/external/portal');
    assert.strictEqual(body.Address, '10.0.0.7');
    assert.strictEqual(body.Port, 443);
    assert.deepEqual(body.Meta, { team: 'web', owner: 'ops' });
    assert.strictEqual(body.NodeID, node.id);
    assert.deepEqual(body.Checks, [
      {
        Type: 'http',
        HTTP: 'https://10.0.0.7/health',
        Method: 'GET',
        Interval: 10e9,
        Timeout: 2e9,
      },
    ]);
    assert.dom(ROWS).exists({ count: 3 });

    // change: the name stays, the check keeps its ID
    await click('[data-test-edit-endpoint="default/portal"]');
    assert.dom('[data-test-endpoint-name-input]').hasAttribute('readonly');
    assert.dom('[data-test-check-interval]').hasValue('10s');
    await fillIn('[data-test-check-interval]', '30s');
    await click('[data-test-endpoint-save]');
    body = lastBody('PUT', '/v1/service/external/portal');
    assert.strictEqual(body.ID, 'portal');
    assert.strictEqual(body.Checks[0].ID, 'http');
    assert.strictEqual(body.Checks[0].Interval, 30e9);

    await click('[data-test-delete-endpoint="default/portal"]');
    await click('[data-test-confirm-submit]');
    assert.ok(lastRequest('DELETE', '/v1/service/external/portal'));
    assert.dom(ROWS).exists({ count: 2 });
  });

  test('a declared service: the servers choose the node that runs the checks', async function (assert) {
    useToken(null, 'management');
    seedNetwork(server);
    await visit('/endpoints');

    await click('[data-test-add-endpoint]');
    await fillIn('[data-test-endpoint-name-input]', 'orders-db');
    await fillIn('[data-test-endpoint-address]', '10.20.0.5');
    await fillIn('[data-test-endpoint-port]', '5432');
    await click('[data-test-add-check]');
    await select('[data-test-check-type]', 'tcp');
    await fillIn('[data-test-check-tcp]', '10.20.0.5:5432');

    // a node chosen by the servers needs the pool, not a node; declared
    // services with node checks need it
    await click('[data-test-endpoint-declared-switch] input');
    await click('[data-test-endpoint-save]');
    assert
      .dom('[data-test-endpoint-save-error]')
      .includesText('chosen by the servers');
    await select('[data-test-endpoint-checker-mode]', 'pool');
    assert.dom('[data-test-endpoint-node]').doesNotExist();
    await fillIn('[data-test-endpoint-checker-pool]', 'default');
    await fillIn('[data-test-endpoint-checker-datacenters]', 'dc1, eu-*');
    await a11yAudit(assert, STRICT);
    await click('[data-test-endpoint-save]');

    const body = lastBody('PUT', '/v1/service/external/orders-db');
    assert.true(body.Declared);
    assert.strictEqual(body.NodeID, undefined);
    assert.deepEqual(body.CheckedBy, {
      NodePool: 'default',
      Datacenters: ['dc1', 'eu-*'],
    });
    assert.dom('[data-test-endpoint-declared]').exists({ count: 1 });
    assert.dom('[data-test-endpoint-chosen]').exists({ count: 1 });

    // change: the choice of the servers is kept
    await click('[data-test-edit-endpoint="default/orders-db"]');
    assert.dom('[data-test-endpoint-checker-mode]').hasValue('pool');
    assert.dom('[data-test-endpoint-checker-pool]').hasValue('default');
    assert
      .dom('[data-test-endpoint-checker-datacenters]')
      .hasValue('dc1, eu-*');
  });

  test('the HCL view: the form as text, checked by the servers', async function (assert) {
    useToken(null, 'management');
    seedNetwork(server);
    await visit('/endpoints');

    await click('[data-test-add-endpoint]');
    await fillIn('[data-test-endpoint-name-input]', 'cache');
    await fillIn('[data-test-endpoint-address]', '10.0.0.9');
    await fillIn('[data-test-endpoint-port]', '6379');
    await click('[data-test-endpoint-views] [data-test-tab="hcl"]');
    assert.dom('[data-test-endpoint-hcl]').exists();
    assert.dom('[data-test-endpoint-save]').doesNotExist();
    const text = document
      .querySelector('[data-test-unified-editor] .CodeMirror')
      .CodeMirror.getValue();
    assert.ok(text.includes('external_service "cache"'), text);
    assert.ok(text.includes('address = "10.0.0.9"'), text);
    await a11yAudit(assert, STRICT);

    // a text with an error: the problem names the line, nothing is written
    await setEditorText(
      'external_service "cache" {\n  address = "10.0.0.9"\n  nope = 1\n}\n'
    );
    await click('[data-test-editor-review-button]');
    assert.dom('[data-test-editor-problem]').includesText('nope');
    assert.notOk(lastRequest('PUT', '/v1/service/external/cache'));

    // two services in one text, one with node checks chosen by the servers
    await setEditorText(
      [
        'external_service "cache" {',
        '  namespace = "default"',
        '  address = "10.0.0.9"',
        '  port = 6379',
        '}',
        '',
        'external_service "orders-db" {',
        '  address = "10.20.0.5"',
        '  check {',
        '    type = "tcp"',
        '    tcp = "10.20.0.5:5432"',
        '  }',
        '  checked_by {',
        '    node_pool = "default"',
        '  }',
        '}',
        '',
      ].join('\n')
    );
    await click('[data-test-editor-review-button]');
    await click('[data-test-editor-submit]');
    assert.notOk(document.querySelector('[data-test-endpoint-editor]'));

    let body = lastBody('PUT', '/v1/service/external/cache');
    assert.true(body.Declared);
    assert.strictEqual(body.Port, 6379);
    body = lastBody('PUT', '/v1/service/external/orders-db');
    assert.deepEqual(body.CheckedBy, { NodePool: 'default' });
    assert.dom(ROWS).exists({ count: 3 });
    assert.dom('[data-test-endpoint-declared]').exists({ count: 2 });

    // an existing service opens as text, and the text goes back to the form
    await click('[data-test-edit-endpoint="default/orders-db"]');
    await click('[data-test-endpoint-views] [data-test-tab="hcl"]');
    const hcl = document
      .querySelector('[data-test-unified-editor] .CodeMirror')
      .CodeMirror.getValue();
    assert.ok(hcl.includes('node_pool = "default"'), hcl);
    await click('[data-test-endpoint-views] [data-test-tab="form"]');
    assert.dom('[data-test-endpoint-checker-mode]').hasValue('pool');
    assert.dom('[data-test-endpoint-address]').hasValue('10.20.0.5');
  });

  test('a token without register-external-service only reads', async function (assert) {
    useToken({
      Namespaces: [{ Name: '*', Capabilities: ['read-job', 'list-jobs'] }],
    });
    seedNetwork(server);
    await visit('/endpoints');
    assert.dom(ROWS).exists({ count: 1 });
    assert.dom('[data-test-add-endpoint]').doesNotExist();
    assert.dom('[data-test-edit-endpoint]').doesNotExist();
  });

  test('without node write the node of the checks cannot be chosen', async function (assert) {
    useToken({
      Namespaces: [
        {
          Name: '*',
          Capabilities: ['read-job', 'list-jobs', 'register-external-service'],
        },
      ],
    });
    await visit('/endpoints');
    await click('[data-test-add-endpoint]');
    assert.dom('[data-test-endpoint-node]').isDisabled();
  });
});
