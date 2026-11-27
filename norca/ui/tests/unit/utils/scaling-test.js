/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import { module, test } from 'qunit';
import { engineOf, isBoundsOnly } from 'nomad-ui/utils/scaling';

module('Unit | Utility | scaling', function () {
  test('a policy without a check only bounds the count', function (assert) {
    assert.true(isBoundsOnly({ Min: 1, Max: 3 }));
    assert.true(isBoundsOnly({ Policy: { cooldown: '2m' }, Checks: null }));
    assert.false(isBoundsOnly(null));
    assert.false(isBoundsOnly({ Checks: [{ Name: 'cpu' }] }));
    assert.false(isBoundsOnly({ Policy: { check: { cpu: {} } } }));
    assert.false(isBoundsOnly({ Policy: { target: { 'custom-object': {} } } }));
    assert.false(isBoundsOnly({ Target: { NodeGroup: 'workers' } }));
  });

  test('the engine of a policy as the Scaling page shows it', function (assert) {
    const checked = { Checks: [{ Name: 'cpu' }] };
    assert.strictEqual(engineOf(checked, true), 'builtin');
    assert.strictEqual(engineOf(checked, false), 'external');
    assert.strictEqual(engineOf({ Min: 1, Max: 3 }, true), 'bounds');
  });
});
