/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import { module, test } from 'qunit';
import { diffLines, diffHunks, diffStats } from 'nomad-ui/utils/text-diff';

module('Unit | Utility | text-diff', function () {
  test('identical texts have only equal lines', function (assert) {
    const ops = diffLines('a\nb\nc', 'a\nb\nc');
    assert.deepEqual(
      ops.map((o) => o.type),
      ['equal', 'equal', 'equal']
    );
    assert.deepEqual(diffStats('a\nb', 'a\nb'), { added: 0, removed: 0 });
  });

  test('added, removed and changed lines with line numbers', function (assert) {
    const ops = diffLines('a\nb\nc\nd', 'a\nc\nd\ne');
    assert.deepEqual(
      ops.map((o) => `${o.type}:${o.text}`),
      ['equal:a', 'del:b', 'equal:c', 'equal:d', 'add:e']
    );
    const del = ops.find((o) => o.type === 'del');
    assert.equal(del.oldNo, 2, 'removed line keeps its old number');
    assert.equal(del.newNo, null, 'removed line has no new number');
    const add = ops.find((o) => o.type === 'add');
    assert.equal(add.newNo, 4);
    assert.equal(add.oldNo, null);
  });

  test('an empty original makes every line an addition', function (assert) {
    assert.deepEqual(diffStats('', 'x\ny'), { added: 2, removed: 0 });
    assert.deepEqual(diffStats('x\ny', ''), { added: 0, removed: 2 });
  });

  test('windows line endings and a final newline do not count as changes', function (assert) {
    assert.deepEqual(diffStats('a\r\nb\r\n', 'a\nb'), { added: 0, removed: 0 });
  });

  test('hunks keep context around changes and collapse the rest', function (assert) {
    const old = Array.from({ length: 20 }, (_, i) => `line ${i}`).join('\n');
    const changed = old.replace('line 10', 'line ten');
    const lines = diffHunks(old, changed, 2);
    assert.deepEqual(
      lines.map((l) => l.type),
      ['skip', 'equal', 'equal', 'del', 'add', 'equal', 'equal', 'skip']
    );
    assert.equal(lines[0].count, 8, 'unchanged lines before are collapsed');
    assert.equal(lines[7].count, 7, 'unchanged lines after are collapsed');
  });
});
