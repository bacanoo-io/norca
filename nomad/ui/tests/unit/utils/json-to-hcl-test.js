/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import { module, test } from 'qunit';
import jsonToHcl, { escapeHclString } from 'nomad-ui/utils/json-to-hcl';

module('Unit | Utility | json-to-hcl', function () {
  test('empty or missing input gives an empty string', function (assert) {
    assert.equal(jsonToHcl(undefined), '');
    assert.equal(jsonToHcl(null), '');
    assert.equal(jsonToHcl({}), '');
  });

  test('one quoted assignment per entry, each ending in a newline', function (assert) {
    assert.equal(jsonToHcl({ lastName: 'escobar' }), 'lastName="escobar"\n');
    assert.equal(
      jsonToHcl({ region: 'global', datacenter: 'dc1' }),
      'region="global"\n\ndatacenter="dc1"\n'
    );
  });

  test('the output can be followed directly by literal variable text', function (assert) {
    const combined = jsonToHcl({ a: 'b' }).concat('c="d"');
    assert.equal(combined, 'a="b"\nc="d"');
  });

  test('numbers and booleans are written as bare literals', function (assert) {
    assert.equal(
      jsonToHcl({ count: 3, enabled: true }),
      'count=3\n\nenabled=true\n'
    );
  });

  test('string values are escaped for HCL quoted strings', function (assert) {
    assert.equal(
      jsonToHcl({ msg: 'say "hi"\\now' }),
      'msg="say \\"hi\\"\\\\now"\n'
    );
    assert.equal(jsonToHcl({ multi: 'a\nb\tc' }), 'multi="a\\nb\\tc"\n');
  });

  test('template sequences are escaped so they stay literal', function (assert) {
    assert.equal(escapeHclString('${var.x}'), '$${var.x}');
    assert.equal(escapeHclString('%{ if true }'), '%%{ if true }');
    assert.equal(escapeHclString('$5 and 100%'), '$5 and 100%');
  });
});
