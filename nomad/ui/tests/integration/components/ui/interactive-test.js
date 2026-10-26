/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

/* eslint-disable qunit/require-expect */
import { module, test } from 'qunit';
import { setupRenderingTest } from 'ember-qunit';
import {
  click,
  fillIn,
  focus,
  render,
  triggerEvent,
  triggerKeyEvent,
} from '@ember/test-helpers';
import { hbs } from 'ember-cli-htmlbars';
import { componentA11yAudit, STRICT } from 'nomad-ui/tests/helpers/a11y-audit';

module('Integration | Component | ui interactive', function (hooks) {
  setupRenderingTest(hooks);

  test('Ui::Tabs (local) selects with clicks and arrow keys', async function (assert) {
    this.tabs = [
      { id: 'a', label: 'Overview' },
      { id: 'b', label: 'Versions', count: 3 },
      { id: 'c', label: 'Events' },
    ];
    this.selected = 'a';
    this.select = (id) => this.set('selected', id);
    await render(
      hbs`<Ui::Tabs @label="Job" @tabs={{this.tabs}} @selected={{this.selected}} @onSelect={{this.select}} />`
    );

    assert.dom('[role="tablist"]').hasAttribute('aria-label', 'Job');
    assert
      .dom('[data-test-tab="a"]')
      .hasAttribute('aria-selected', 'true')
      .hasAttribute('tabindex', '0');
    assert.dom('[data-test-tab="b"]').hasAttribute('tabindex', '-1');
    assert.dom('[data-test-tab="b"] .badge').hasText('3');

    await click('[data-test-tab="b"]');
    assert.strictEqual(this.selected, 'b');

    await focus('[data-test-tab="b"]');
    await triggerKeyEvent('[role="tablist"]', 'keydown', 'ArrowRight');
    assert.strictEqual(this.selected, 'c', 'ArrowRight selects the next tab');
    await triggerKeyEvent('[role="tablist"]', 'keydown', 'Home');
    assert.strictEqual(this.selected, 'a', 'Home selects the first tab');
    await componentA11yAudit(this.element, assert, STRICT);
  });

  test('Ui::Modal traps focus, closes on Escape and the close button', async function (assert) {
    this.isOpen = true;
    this.closed = 0;
    this.close = () => {
      this.set('closed', this.closed + 1);
      this.set('isOpen', false);
    };
    await render(hbs`
      <button type="button" data-test-opener>Open</button>
      {{#if this.isOpen}}
        <Ui::Modal @title="Stop job?" @tone="danger" @onClose={{this.close}}>
          <:default>The allocations are stopped.</:default>
          <:footer><button type="button" data-test-cancel>Cancel</button></:footer>
        </Ui::Modal>
      {{/if}}
    `);
    assert.dom('[role="dialog"]').hasAttribute('aria-modal', 'true');
    assert.dom('[data-test-modal-title]').hasText('Stop job?');
    assert
      .dom('[data-test-modal-close]')
      .isFocused('focus moves into the dialog');
    await componentA11yAudit(this.element, assert, STRICT);

    await triggerKeyEvent('[role="dialog"]', 'keydown', 'Escape');
    assert.strictEqual(this.closed, 1, 'Escape closes');
    assert.dom('[data-test-modal]').doesNotExist();

    this.set('isOpen', true);
    await click('[data-test-modal-close]');
    assert.strictEqual(this.closed, 2, 'the close button closes');
  });

  test('Ui::Dropdown opens, navigates with the keyboard and closes', async function (assert) {
    this.chosen = null;
    this.choose = (what) => this.set('chosen', what);
    await render(hbs`
      <Ui::Dropdown @label="Actions" as |dd|>
        <dd.Header>Job</dd.Header>
        <dd.Item data-test-one {{on "click" (fn this.choose "one")}}>One</dd.Item>
        <dd.Divider />
        <dd.Item @tone="danger" data-test-two {{on "click" (fn this.choose "two")}}>Two</dd.Item>
      </Ui::Dropdown>
      <button type="button" data-test-outside>Outside</button>
    `);
    assert.dom('[data-test-dropdown-menu]').doesNotExist();
    assert
      .dom('[data-test-dropdown-trigger]')
      .hasAttribute('aria-expanded', 'false');

    await click('[data-test-dropdown-trigger]');
    assert.dom('[data-test-dropdown-menu]').exists();
    assert
      .dom('[data-test-dropdown-trigger]')
      .hasAttribute('aria-expanded', 'true');
    assert.dom('[data-test-two]').hasClass('text-danger');

    await focus('[data-test-one]');
    await triggerKeyEvent('[data-test-dropdown-menu]', 'keydown', 'ArrowDown');
    assert.dom('[data-test-two]').isFocused('ArrowDown moves to the next item');

    await click('[data-test-two]');
    assert.strictEqual(this.chosen, 'two');
    assert
      .dom('[data-test-dropdown-menu]')
      .doesNotExist('choosing an item closes the menu');

    await click('[data-test-dropdown-trigger]');
    await triggerKeyEvent('[data-test-dropdown-menu]', 'keydown', 'Escape');
    assert.dom('[data-test-dropdown-menu]').doesNotExist('Escape closes');
    assert
      .dom('[data-test-dropdown-trigger]')
      .isFocused('focus returns to the trigger');

    await click('[data-test-dropdown-trigger]');
    await triggerEvent('[data-test-outside]', 'mousedown');
    assert
      .dom('[data-test-dropdown-menu]')
      .doesNotExist('a click outside closes');
  });

  test('Ui::Table renders rows, sorting headers and the empty message', async function (assert) {
    this.rows = [
      { name: 'web', status: 'running' },
      { name: 'api', status: 'pending' },
    ];
    this.sortBy = 'name';
    this.desc = false;
    this.sort = (key, desc) => this.setProperties({ sortBy: key, desc });
    await render(hbs`
      <Ui::Table
        @caption="Jobs"
        @columns={{array (hash key="name" label="Name" sortable=true) (hash key="status" label="Status")}}
        @rows={{this.rows}}
        @sortBy={{this.sortBy}}
        @sortDescending={{this.desc}}
        @onSort={{this.sort}}
        as |row|
      >
        <td>{{row.name}}</td><td>{{row.status}}</td>
      </Ui::Table>
    `);
    assert.dom('[data-test-ui-table-row]').exists({ count: 2 });
    assert
      .dom('[data-test-ui-table-header="name"]')
      .hasAttribute('aria-sort', 'ascending');
    assert.dom('[data-test-ui-table-header="status"] button').doesNotExist();

    await click('[data-test-ui-table-header="name"] button');
    assert.true(this.desc, 'sorting the sorted column flips the direction');
    assert
      .dom('[data-test-ui-table-header="name"]')
      .hasAttribute('aria-sort', 'descending');

    this.set('rows', []);
    assert.dom('[data-test-ui-table-empty]').hasText('Nothing to show.');
    await componentA11yAudit(this.element, assert, STRICT);
  });

  test('Ui::Field, Ui::Input, Ui::Select and Ui::Switch', async function (assert) {
    this.name = '';
    this.region = 'b';
    this.live = false;
    this.setName = (v) => this.set('name', v);
    this.setRegion = (v) => this.set('region', v);
    this.setLive = (v) => this.set('live', v);
    await render(hbs`
      <Ui::Field @label="Name" @help="Lowercase letters." @error="Required" as |f|>
        <Ui::Input @id={{f.id}} @describedBy={{f.describedBy}} @invalid={{f.invalid}} @value={{this.name}} @onInput={{this.setName}} />
      </Ui::Field>
      <Ui::Field @label="Region" as |f|>
        <Ui::Select @id={{f.id}} @options={{array (hash value="a" label="Alpha") "b"}} @value={{this.region}} @onChange={{this.setRegion}} />
      </Ui::Field>
      <Ui::Switch @switch={{true}} @label="Live updates" @checked={{this.live}} @onChange={{this.setLive}} />
    `);
    const input = this.element.querySelector('[data-test-input]');
    assert.dom('[data-test-field-label]').hasAttribute('for', input.id);
    assert
      .dom(input)
      .hasClass('is-invalid')
      .hasAttribute('aria-invalid', 'true');
    assert.strictEqual(
      input.getAttribute('aria-describedby').split(' ').length,
      2,
      'help and error describe the input'
    );
    await fillIn(input, 'web');
    assert.strictEqual(this.name, 'web');

    assert.dom('[data-test-select]').hasValue('b');
    await fillIn('[data-test-select]', 'a');
    assert.strictEqual(this.region, 'a');

    await click('[data-test-switch-input]');
    assert.true(this.live);
    await componentA11yAudit(this.element, assert, STRICT);
  });
});
