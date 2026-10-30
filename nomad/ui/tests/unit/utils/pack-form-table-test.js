/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import { module, test } from 'qunit';
import {
  formSchema,
  initialState,
  newRow,
  overrides,
  setCell,
  validate,
} from 'nomad-ui/utils/pack-form';

// A list of objects with the object-list widget, as the servers synthesize
// it from list(object({...})) with the enum of values.schema.json.
const SCHEMA = {
  type: 'object',
  properties: {
    entrypoints: {
      type: 'array',
      'x-ui-widget': 'object-list',
      title: 'Entrypoints',
      minItems: 1,
      default: [{ name: 'web', port: 80, protocol: 'http' }],
      items: {
        type: 'object',
        required: ['name', 'port'],
        properties: {
          name: { type: 'string', 'x-ui-order': 1 },
          port: {
            type: 'integer',
            minimum: 1,
            maximum: 65535,
            'x-ui-order': 2,
          },
          protocol: {
            type: 'string',
            enum: ['http', 'https', 'tcp', 'udp'],
            'x-ui-order': 3,
          },
          proxy_protocol: { type: 'boolean', 'x-ui-order': 4 },
          options: { type: 'object', 'x-ui-order': 5 },
        },
      },
    },
    raw: {
      type: 'array',
      'x-ui-widget': 'object-list',
      items: { type: 'object' },
    },
  },
};

module('Unit | Utility | pack-form table', function () {
  test('an object-list of declared items is a table with a column per attribute', function (assert) {
    const form = formSchema(SCHEMA);
    const f = form.fields.find((f) => f.key === 'entrypoints');
    assert.strictEqual(f.widget, 'table');
    assert.deepEqual(
      f.columns.map((c) => [c.key, c.widget, c.required]),
      [
        ['name', 'text', true],
        ['port', 'number', true],
        ['protocol', 'select', false],
        ['proxy_protocol', 'switch', false],
        ['options', 'text', false],
      ]
    );
    assert.true(f.columns[4].json, 'objects in a cell are JSON text');
    assert.strictEqual(
      form.fields.find((f) => f.key === 'raw').widget,
      'json',
      'items without attributes stay JSON'
    );
  });

  test('rows and cells', function (assert) {
    const f = formSchema(SCHEMA).fields.find((f) => f.key === 'entrypoints');
    assert.deepEqual(newRow(f), { proxy_protocol: false });

    let row = setCell(f, {}, f.columns[0], 'websecure');
    row = setCell(f, row, f.columns[1], '443');
    row = setCell(f, row, f.columns[2], 'https');
    assert.deepEqual(row, { name: 'websecure', port: 443, protocol: 'https' });

    // An emptied optional attribute is removed (the pack's default
    // applies); a required one stays, so it is reported.
    row = setCell(f, row, f.columns[2], '');
    assert.deepEqual(row, { name: 'websecure', port: 443 });
    row = setCell(f, row, f.columns[0], '');
    assert.strictEqual(row.name, '');

    row = setCell(f, row, f.columns[4], '{"a":1}');
    assert.deepEqual(row.options, { a: 1 });
  });

  test('validation per cell and of the list', function (assert) {
    const form = formSchema(SCHEMA);
    const errors = validate(form, {
      entrypoints: [
        { name: 'web', port: 80, protocol: 'http' },
        { name: '', port: 70000, protocol: 'smtp' },
      ],
    });
    assert.deepEqual(Object.keys(errors).sort(), [
      'entrypoints.1.name',
      'entrypoints.1.port',
      'entrypoints.1.protocol',
    ]);
    assert.ok(errors['entrypoints.1.port'].includes('at most 65535'));

    assert.ok(
      validate(form, { entrypoints: [] }).entrypoints.includes('at least 1'),
      'minItems'
    );
  });

  test('the defaults and the overrides', function (assert) {
    const form = formSchema(SCHEMA);
    const { values } = initialState(form, {});
    assert.deepEqual(values.entrypoints, [
      { name: 'web', port: 80, protocol: 'http' },
    ]);
    assert.deepEqual(overrides(form, values), {}, 'the default is not sent');

    // Values of CLI installs are JSON text.
    const cli = initialState(form, {
      entrypoints: '[{"name":"tcp","port":5432,"protocol":"tcp"}]',
    }).values;
    assert.strictEqual(cli.entrypoints[0].port, 5432);

    const changed = {
      ...values,
      entrypoints: [...values.entrypoints, { name: 'grpc', port: 50051 }],
    };
    assert.deepEqual(overrides(form, changed).entrypoints.length, 2);
  });
});
