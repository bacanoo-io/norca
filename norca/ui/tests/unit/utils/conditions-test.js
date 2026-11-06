/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import { module, test } from 'qunit';
import { conditionTone, worstCondition } from 'nomad-ui/utils/conditions';
import { objectHealth } from 'nomad-ui/utils/objects';

module('Unit | Utility | conditions', function () {
  test('a condition that is good when True', function (assert) {
    assert.strictEqual(conditionTone('Ready', 'True'), 'success');
    assert.strictEqual(conditionTone('Ready', 'False'), 'danger');
    assert.strictEqual(conditionTone('ProviderAvailable', 'False'), 'danger');
    assert.strictEqual(conditionTone('NetworkCovered', 'Unknown'), 'neutral');
  });

  test('a condition that is a problem when True', function (assert) {
    assert.expect(14);
    assert.strictEqual(conditionTone('Degraded', 'True'), 'danger');
    assert.strictEqual(conditionTone('JoinFailing', 'True'), 'danger');
    for (const type of [
      'RollBlocked',
      'RollBehind',
      'ForeignNode',
      'ScaleDownBlocked',
      'DeletionBlocked',
      'Drifted',
    ]) {
      assert.strictEqual(conditionTone(type, 'True'), 'warning', type);
      assert.strictEqual(conditionTone(type, 'False'), 'success', type);
    }
  });

  test('a condition that is work in progress when True', function (assert) {
    assert.strictEqual(conditionTone('Rolling', 'True'), 'info');
    assert.strictEqual(conditionTone('Rolling', 'False'), 'neutral');
    assert.strictEqual(conditionTone('Progressing', 'True'), 'info');
  });

  test('the worst condition of a list', function (assert) {
    const list = [
      { type: 'Provisioned', status: 'True' },
      { type: 'RollBlocked', status: 'True' },
      { type: 'JoinFailing', status: 'True' },
      { type: 'Rolling', status: 'True' },
    ];
    assert.deepEqual(worstCondition(list), {
      type: 'JoinFailing',
      status: 'True',
      tone: 'danger',
    });
    assert.strictEqual(
      worstCondition([{ type: 'RollBlocked', status: 'False' }]),
      null
    );
  });

  test('the health of a node group', function (assert) {
    const group = (conds) => ({
      Generation: 1,
      Status: { observedGeneration: 1, conditions: conds },
    });
    assert.deepEqual(
      objectHealth(
        group([
          { type: 'ProviderAvailable', status: 'True' },
          { type: 'RollBlocked', status: 'False' },
        ])
      ),
      { tone: 'success', label: 'ok', behind: false },
      'no Ready condition, every condition good'
    );
    assert.deepEqual(
      objectHealth(
        group([
          { type: 'ProviderAvailable', status: 'True' },
          { type: 'RollBlocked', status: 'True' },
        ])
      ),
      { tone: 'warning', label: 'RollBlocked True', behind: false }
    );
    assert.deepEqual(
      objectHealth(group([{ type: 'ProviderAvailable', status: 'False' }])),
      { tone: 'danger', label: 'ProviderAvailable False', behind: false }
    );
    assert.deepEqual(objectHealth(group([])), {
      tone: 'neutral',
      label: 'unknown',
      behind: false,
    });
  });
});
