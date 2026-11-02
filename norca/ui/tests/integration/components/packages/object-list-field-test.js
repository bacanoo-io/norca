/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import { module, test } from 'qunit';
import { setupRenderingTest } from 'ember-qunit';
import {
  click,
  fillIn,
  find,
  findAll,
  render,
  select,
} from '@ember/test-helpers';
import hbs from 'htmlbars-inline-precompile';
import { formSchema } from 'nomad-ui/utils/pack-form';
import { componentA11yAudit } from 'nomad-ui/tests/helpers/a11y-audit';

const SCHEMA = {
  type: 'object',
  properties: {
    entrypoints: {
      type: 'array',
      'x-ui-widget': 'object-list',
      title: 'Entrypoints',
      items: {
        type: 'object',
        required: ['name', 'port'],
        properties: {
          name: { type: 'string', 'x-ui-order': 1 },
          port: { type: 'integer', 'x-ui-order': 2 },
          protocol: {
            type: 'string',
            enum: ['http', 'https', 'tcp'],
            'x-ui-order': 3,
          },
        },
      },
    },
  },
};

module(
  'Integration | Component | packages/object-list-field',
  function (hooks) {
    setupRenderingTest(hooks);

    hooks.beforeEach(function () {
      this.field = formSchema(SCHEMA).fields[0];
      this.value = [{ name: 'web', port: 80, protocol: 'http' }];
      this.errors = {};
      this.onChange = (field, value) => this.set('value', value);
    });

    test('edits, adds and removes rows', async function (assert) {
      assert.expect(10);
      await render(hbs`<Packages::ObjectListField @field={{this.field}} @value={{this.value}}
      @errors={{this.errors}} @onChange={{this.onChange}} />`);

      assert.strictEqual(findAll('[data-test-ui-table-row]').length, 1);
      await componentA11yAudit(this.element, assert);
      assert.dom('[data-test-pack-cell="0.name"] input').hasValue('web');
      assert.dom('[data-test-pack-cell="0.protocol"] select').hasValue('http');

      // Typing keeps the cell (and its focus): the rows are stable.
      const input = find('[data-test-pack-cell="0.name"] input');
      await fillIn(input, 'websecure');
      assert.strictEqual(
        find('[data-test-pack-cell="0.name"] input'),
        input,
        'the same input element'
      );
      await fillIn('[data-test-pack-cell="0.port"] input', '443');
      await select('[data-test-pack-cell="0.protocol"] select', 'https');
      assert.deepEqual(this.value, [
        { name: 'websecure', port: 443, protocol: 'https' },
      ]);

      await click('[data-test-pack-table-add]');
      assert.strictEqual(findAll('[data-test-ui-table-row]').length, 2);
      await fillIn('[data-test-pack-cell="1.name"] input', 'postgres');
      await fillIn('[data-test-pack-cell="1.port"] input', '5432');
      await select('[data-test-pack-cell="1.protocol"] select', 'tcp');
      assert.deepEqual(this.value[1], {
        name: 'postgres',
        port: 5432,
        protocol: 'tcp',
      });

      await click('[data-test-pack-table-remove="0"]');
      assert.deepEqual(this.value, [
        { name: 'postgres', port: 5432, protocol: 'tcp' },
      ]);
      assert.dom('[data-test-pack-cell="0.name"] input').hasValue('postgres');
    });

    test('shows the errors of the cells', async function (assert) {
      this.errors = { 'entrypoints.0.port': 'Port is required' };
      await render(hbs`<Packages::ObjectListField @field={{this.field}} @value={{this.value}}
      @errors={{this.errors}} @onChange={{this.onChange}} />`);
      assert
        .dom('[data-test-pack-cell="0.port"] [data-test-pack-cell-error]')
        .hasText('Port is required');
      assert.dom('[data-test-pack-cell="0.port"] input').hasClass('is-invalid');
    });
  }
);
