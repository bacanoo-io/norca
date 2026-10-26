/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import { module, test } from 'qunit';
import {
  normalize,
  compareRevisions,
  signature,
  valuesSchema,
  upgradeCommand,
} from 'nomad-ui/utils/releases';
import {
  kindDocument,
  kindBody,
  objectDocument,
  objectDocumentSchema,
} from 'nomad-ui/utils/objects';
import { validate } from 'nomad-ui/utils/json-schema';
import { usageRows } from 'nomad-ui/utils/quotas';
import {
  flattenJobDiff,
  previewCounts,
  backupSize,
} from 'nomad-ui/utils/backups';
import {
  allowsBlock,
  allowsNamespace,
  allowsObject,
} from 'nomad-ui/utils/norca-acl';
import { healthSummary, ruleRows } from 'nomad-ui/utils/networking';

function token(rules, type = 'client') {
  return {
    aclEnabled: true,
    selfToken: { type },
    selfTokenPolicies: rules.map((rulesJSON) => ({ rulesJSON })),
  };
}

module('Unit | Utility | phase 3 areas', function () {
  test('release views are normalized to PascalCase except user values', function (assert) {
    const r = normalize({
      ID: 'x',
      Spec: {
        pack: { name: 'p', trustedSigner: 'k' },
        values: { image_tag: '1' },
      },
      Status: { conditions: [{ type: 'Drifted', status: 'True' }] },
    });
    assert.strictEqual(r.Spec.Pack.Name, 'p');
    assert.deepEqual(r.Spec.Values, { image_tag: '1' });
    assert.strictEqual(r.Status.Conditions[0].Type, 'Drifted');
    assert.strictEqual(signature(r).label, 'verified');
    assert.strictEqual(
      normalize({ Spec: { Pack: { Name: 'q' } } }).Spec.Pack.Name,
      'q'
    );
  });

  test('two revisions are compared by values and resources', function (assert) {
    const a = {
      Spec: {
        Pack: { Name: 'p', Version: '1' },
        Values: { a: 1, b: { c: 2 } },
        Resources: [
          { Type: 'job', Name: 'j', Hash: '1' },
          { Type: 'variable', Name: 'v', Hash: '1' },
        ],
      },
    };
    const b = {
      Spec: {
        Pack: { Name: 'p', Version: '2' },
        Values: { a: 1, b: { c: 3 } },
        Resources: [
          { Type: 'job', Name: 'j', Hash: '2' },
          { Type: 'namespace', Name: 'n', Hash: '1' },
        ],
      },
    };
    const c = compareRevisions(a, b);
    assert.strictEqual(c.pack, 'p 1 → 2');
    assert.strictEqual(c.changedValues, 1);
    assert.deepEqual(
      c.values.find((v) => v.key === 'b.c'),
      { key: 'b.c', a: '2', b: '3', changed: true }
    );
    assert.deepEqual(c.resources.map((r) => `${r.change}:${r.Name}`).sort(), [
      'added:n',
      'changed:j',
      'removed:v',
    ]);
  });

  test('quota usage rows: limited, unlimited, disallowed and nearly full', function (assert) {
    const rows = usageRows(
      {
        RegionLimit: { CPU: 1000, MemoryMB: 0, MemoryMaxMB: -1 },
        Allocations: 10,
      },
      { RegionLimit: { CPU: 950, MemoryMB: 300 }, Allocations: 2 }
    );
    const by = Object.fromEntries(rows.map((r) => [r.id, r]));
    assert.strictEqual(by.cpu.state, 'nearly');
    assert.strictEqual(by.cpu.percent, 95);
    assert.strictEqual(by.memory.state, 'unlimited');
    assert.strictEqual(by.memory_max.state, 'disallowed');
    assert.strictEqual(by.allocations.state, 'ok');
    assert.notOk(by.variables, 'unlimited and unused dimensions are left out');
  });

  test('backup helpers: job diff rows, preview counts and size', function (assert) {
    const rows = flattenJobDiff({
      Fields: [{ Type: 'Edited', Name: 'Priority', Old: '50', New: '70' }],
      TaskGroups: [
        {
          Type: 'Edited',
          Name: 'web',
          Fields: [{ Type: 'Edited', Name: 'Count', Old: '2', New: '3' }],
          Tasks: [{ Type: 'None', Name: 't' }],
        },
      ],
    });
    assert.deepEqual(
      rows.map((r) => r.path),
      ['Priority', 'group web › Count']
    );
    const counts = previewCounts({
      Counts: {
        'jobs/removed': 2,
        'jobs/reverted': 1,
        'variables/restored': 3,
      },
    });
    assert.deepEqual(
      counts.map((c) => [c.type, c.removed, c.restored, c.reverted]),
      [
        ['jobs', 2, 0, 1],
        ['variables', 0, 3, 0],
      ]
    );
    assert.strictEqual(
      backupSize({
        Destinations: [
          { Objects: { state: { Size: 10 }, keyring: { Size: 5 } } },
        ],
      }),
      15
    );
  });

  test('ACL helpers read the operator blocks, namespace and object capabilities', function (assert) {
    const t = token([
      {
        Backup: { Policy: 'read' },
        Namespaces: [
          {
            Name: 'prod-*',
            Capabilities: ['submit-job'],
            Objects: [{ Kind: 'db.example.com/*', Policy: 'write' }],
          },
          { Name: 'default', Capabilities: ['read-job'] },
        ],
      },
    ]);
    assert.true(allowsBlock(t, 'Backup', 'read'));
    assert.false(allowsBlock(t, 'Backup', 'write'));
    assert.false(allowsBlock(t, 'Quota', 'read'));
    assert.true(allowsNamespace(t, 'prod-eu', 'submit-job'));
    assert.false(allowsNamespace(t, 'default', 'submit-job'));
    assert.true(allowsNamespace(t, '*', 'submit-job'));
    assert.true(
      allowsObject(t, 'prod-eu', 'db.example.com/PostgresCluster', 'write')
    );
    assert.false(
      allowsObject(t, 'default', 'db.example.com/PostgresCluster', 'read')
    );
    assert.true(allowsBlock(token([], 'management'), 'Backup', 'write'));
    assert.true(
      allowsBlock(
        token([
          { Backup: { Policy: 'write' } },
          { Backup: { Policy: 'deny' } },
        ]),
        'Backup',
        'read'
      ) === false
    );
  });

  test('service health and network policy rules', function (assert) {
    const h = healthSummary([
      { Health: 'passing', Ready: true },
      { Health: 'warning', Ready: false },
      { Health: 'critical', Ready: true },
    ]);
    assert.deepEqual(
      [h.passing, h.warning, h.critical, h.discoverable, h.excluded, h.tone],
      [1, 1, 1, 1, 2, 'danger']
    );
    const rules = ruleRows({
      Ingress: [
        {
          Name: 'r',
          Peers: [
            { CIDR: ['10.0.0.0/8'], Namespace: 'a', Selector: { app: 'x' } },
          ],
          Ports: ['80/tcp'],
        },
      ],
    });
    assert.deepEqual(rules[0], {
      direction: 'ingress',
      name: 'r',
      action: 'allow',
      peers: '10.0.0.0/8 namespace a app=x',
      ports: '80/tcp',
    });
  });

  test('release values: the root schema and the prepared upgrade command', function (assert) {
    const revision = {
      Revision: 3,
      Spec: {
        Pack: { Name: 'app', Version: '1.2.0', Registry: 'acme' },
        VariablesSchemas: [
          { Pack: 'app.redis', Schema: '{"type":"object"}' },
          { Pack: 'app', Schema: '{"type":"object","required":["image"]}' },
        ],
      },
    };
    assert.deepEqual(valuesSchema(revision), {
      type: 'object',
      required: ['image'],
    });
    assert.strictEqual(valuesSchema({ Spec: {} }), null);

    const command = upgradeCommand(
      { Name: 'web', Namespace: 'team-a' },
      revision,
      { replicas: 2, image: "it's", ports: [80] }
    );
    assert.strictEqual(
      command,
      [
        'norca pack run app',
        "  --registry='acme'",
        "  --ref='1.2.0'",
        "  --name='web'",
        "  --namespace='team-a'",
        "  --var 'image=it'\\''s'",
        "  --var 'ports=[80]'",
        "  --var 'replicas=2'",
      ].join(' \\\n')
    );
  });

  test('object and kind documents for the editors', function (assert) {
    const kind = {
      Group: 'db.example.com',
      Kind: 'Db',
      Namespaced: true,
      Schema: JSON.stringify({
        type: 'object',
        properties: {
          spec: {
            type: 'object',
            required: ['size'],
            properties: { size: { type: 'integer', maximum: 3 } },
          },
        },
      }),
      CreateIndex: 1,
      ModifyIndex: 2,
    };
    const doc = kindDocument(kind);
    assert.strictEqual(typeof doc.Schema, 'object');
    assert.notOk('ModifyIndex' in doc);
    assert.strictEqual(typeof kindBody(doc).Schema, 'string');

    const object = { Name: 'a', Namespace: 'x', Spec: { size: 1 }, Status: {} };
    assert.deepEqual(objectDocument(object, kind), {
      Name: 'a',
      Namespace: 'x',
      Labels: {},
      Spec: { size: 1 },
    });
    const schema = objectDocumentSchema(kind);
    assert.deepEqual(
      validate(schema, { Name: 'a', Namespace: 'x', Spec: { size: 5 } }).map(
        (e) => e.path
      ),
      ['Spec.size']
    );
    assert.deepEqual(
      validate(schema, { Name: 'a', Namespace: 'x', Spec: {}, Status: {} }).map(
        (e) => e.path
      ).length,
      2,
      'a missing required spec field and the status are reported'
    );
  });
});
