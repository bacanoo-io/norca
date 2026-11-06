/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import { module, test } from 'qunit';
import {
  fnv32a,
  jitterOf,
  nodeBadges,
  nodeIndex,
  parseDuration,
  replacementTimes,
} from 'nomad-ui/utils/node-scaling';

const groups = [
  {
    Name: 'edge-dc1',
    Status: {
      nodes: [
        { instance: 'i-1', state: 'running', node: 'n1', ready: true },
        {
          instance: 'i-2',
          state: 'running',
          node: 'n2',
          ready: true,
          removing: 'leaving',
        },
        { instance: 'i-3', state: 'creating' },
        { instance: 'i-4', state: 'running', node: 'n4', ready: true },
      ],
      rollout: {
        replacing: [{ instance: 'i-4', reason: 'marked', phase: 'surging' }],
        outdated: ['node-1'],
      },
      strayNodes: [{ node: 'n9', name: 'x', instance: 'i-9', kind: 'foreign' }],
    },
  },
];

const labels = (index, id, name, meta) =>
  nodeBadges(index, { id, name }, meta).map((b) => b.label);

module('Unit | Utility | node-scaling', function () {
  test('the index of the nodes of the node groups', function (assert) {
    const index = nodeIndex(groups);
    assert.deepEqual(index.groups, [
      { name: 'edge-dc1', nodes: ['n1', 'n2', 'n4', 'n9'], joining: 1 },
    ]);
    assert.deepEqual(labels(index, 'n1', 'node-1'), ['Autoscaled', 'Outdated']);
    assert.deepEqual(labels(index, 'n2', 'node-2'), ['Autoscaled', 'Removing']);
    assert.deepEqual(labels(index, 'n4', 'node-4'), [
      'Autoscaled',
      'Replacing',
    ]);
    assert.deepEqual(labels(index, 'n9', 'x'), ['Unverified']);
    assert.deepEqual(
      labels(index, 'n5', 'y', {
        'norca.node_group': 'edge-dc1',
        'norca.created_by': 'nodescaler',
      }),
      ['Unverified'],
      'a node with the marks the group does not list'
    );
    assert.deepEqual(labels(index, 'n6', 'z', {}), [], 'a node added by hand');
    assert.deepEqual(labels(null, 'n1', 'node-1', {}), [], 'not loaded yet');
  });

  test('durations of the NodeGroup schema', function (assert) {
    assert.strictEqual(parseDuration('45d'), 45 * 86400e3);
    assert.strictEqual(parseDuration('1h30m'), 5400e3);
    assert.strictEqual(parseDuration('soon'), 0);
  });

  test('the hash and the jitter are those of the operator', function (assert) {
    // The values of hash/fnv New32a and jitterOf of norca-nodescaler.
    assert.strictEqual(fnv32a('ng-scaled-1-1'), 1545920931);
    assert.strictEqual(fnv32a('workers-dc1-3-2'), 3270832022);
    assert.strictEqual(jitterOf('ng-scaled-1-1'), 0.0931);
  });

  test('when a node is replaced by its age', function (assert) {
    const created = Date.parse('2026-01-01T00:00:00Z');
    const meta = {
      'norca.created_at': '2026-01-01T00:00:00Z',
      'norca.cert_expires': '2026-03-02T00:00:00Z',
    };
    const t = replacementTimes(meta, null, 'ng-scaled-1-1');
    assert.strictEqual(t.hard.getTime(), created + 55 * 86400e3);
    assert.strictEqual(
      t.soft.getTime(),
      created + 45 * 86400e3 * (1 - 0.15 * jitterOf('ng-scaled-1-1'))
    );
    const t2 = replacementTimes(
      meta,
      { rolling: { max_node_age: '10d', jitter: '0%' } },
      'a'
    );
    assert.strictEqual(t2.soft.getTime(), created + 10 * 86400e3);
  });
});
