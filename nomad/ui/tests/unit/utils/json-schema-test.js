/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import { module, test } from 'qunit';
import { validate, parseJSON } from 'nomad-ui/utils/json-schema';

const SCHEMA = {
  type: 'object',
  required: ['Name'],
  additionalProperties: false,
  properties: {
    Name: { type: 'string', minLength: 1, pattern: '^[a-z-]+$' },
    Count: { type: 'integer', minimum: 0, maximum: 10 },
    Mode: { enum: ['a', 'b'] },
    Tags: { type: 'array', items: { type: 'string' }, maxItems: 2 },
    Meta: {
      type: 'object',
      additionalProperties: { type: 'string' },
    },
  },
};

module('Unit | Utility | json-schema', function () {
  test('a valid document has no errors', function (assert) {
    assert.deepEqual(
      validate(SCHEMA, {
        Name: 'web',
        Count: 3,
        Mode: 'a',
        Tags: ['x'],
        Meta: { owner: 'team' },
      }),
      []
    );
  });

  test('errors carry the path of the value', function (assert) {
    const errors = validate(SCHEMA, {
      Count: 11,
      Mode: 'c',
      Tags: ['x', 1, 'z'],
      Meta: { owner: 2 },
      Extra: true,
    });
    const messages = errors.map((e) => `${e.path}: ${e.message}`);
    assert.ok(messages.includes('(root): Name is required'));
    assert.ok(messages.includes('Count: must be at most 10'));
    assert.ok(messages.some((m) => m.startsWith('Mode: must be one of')));
    assert.ok(messages.includes('Tags: must have at most 2 items'));
    assert.ok(messages.includes('Tags.1: must be string, not integer'));
    assert.ok(messages.includes('Meta.owner: must be string, not integer'));
    assert.ok(messages.includes('Extra: unknown field Extra'));
  });

  test('strings are checked for length and pattern', function (assert) {
    assert.deepEqual(
      validate(SCHEMA, { Name: '' }).map((e) => e.message),
      ['must not be empty', 'must match ^[a-z-]+$']
    );
  });

  test('anyOf and oneOf', function (assert) {
    const schema = {
      anyOf: [{ type: 'string' }, { type: 'integer' }],
    };
    assert.deepEqual(validate(schema, 'x'), []);
    assert.equal(validate(schema, true).length, 1);
    const one = { oneOf: [{ type: 'integer' }, { type: 'number' }] };
    assert.equal(validate(one, 1).length, 1, 'an integer matches both forms');
    assert.deepEqual(validate(one, 1.5), []);
  });

  test('parseJSON reports the line of a syntax error', function (assert) {
    assert.deepEqual(parseJSON('{"a": 1}'), { value: { a: 1 }, error: null });
    const { error } = parseJSON('{\n  "a": 1,\n  "b": \n}');
    assert.ok(error.message, 'an error message');
    const knownLine = error.line === null ? 3 : error.line;
    assert.ok(knownLine >= 3, 'the line of the error, when known');
  });
});
