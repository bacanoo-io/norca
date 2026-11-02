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
  findAll,
  render,
  select,
  triggerKeyEvent,
} from '@ember/test-helpers';
import { hbs } from 'ember-cli-htmlbars';
import { componentA11yAudit, STRICT } from 'nomad-ui/tests/helpers/a11y-audit';

const ROWS = Array.from({ length: 30 }, (_, i) => ({
  id: `job-${String(i).padStart(2, '0')}`,
  status: i % 3 === 0 ? 'dead' : 'running',
  type: i % 2 ? 'batch' : 'service',
}));

module('Integration | Component | ui lists', function (hooks) {
  setupRenderingTest(hooks);

  hooks.beforeEach(function () {
    window.localStorage.clear();
    this.owner.lookup('service:list-preferences').clearAll();
  });

  hooks.afterEach(function () {
    window.localStorage.clear();
  });

  const TEMPLATE = hbs`
    <Ui::DataTable
      @id="test-list"
      @caption="Jobs"
      @columns={{this.columns}}
      @rows={{this.rows}}
      @sortBy={{this.sortBy}}
      @sortDescending={{this.desc}}
      @onSort={{this.sort}}
      @pageSize={{this.pageSize}}
      @page={{this.page}}
      @onPageChange={{this.setPage}}
      @onPageSizeChange={{this.setPageSize}}
      @selectable={{true}}
      @onRowClick={{this.open}}
      @viewState={{this.viewState}}
      @onApplyView={{this.applyView}}
    >
      <:filters>
        <Ui::FilterDropdown
          @label="Status"
          @options={{this.statusOptions}}
          @selection={{this.statusSelection}}
          @onSelect={{this.setStatus}}
          data-test-status-filter
        />
      </:filters>
      <:row as |row r|>
        <r.Row data-test-row={{row.id}}>
          <r.Cell @key="id" data-test-cell-id>{{row.id}}</r.Cell>
          <r.Cell @key="status" data-test-cell-status>{{row.status}}</r.Cell>
          <r.Cell @key="type" data-test-cell-type>{{row.type}}</r.Cell>
        </r.Row>
      </:row>
      <:bulk as |selected clear|>
        <button type="button" data-test-bulk-stop {{on "click" (fn this.stop selected clear)}}>Stop</button>
      </:bulk>
    </Ui::DataTable>
  `;

  function setup(context) {
    context.setProperties({
      columns: [
        { key: 'id', label: 'Name', sortable: true, hideable: false },
        { key: 'status', label: 'Status', sortable: true },
        { key: 'type', label: 'Type', hidden: true },
      ],
      statusOptions: [
        { key: 'running', label: 'Running' },
        { key: 'dead', label: 'Dead' },
      ],
      statusSelection: [],
      sortBy: 'id',
      desc: false,
      page: 1,
      pageSize: 10,
      opened: null,
      stopped: null,
    });
    context.set('rows', ROWS);
    context.set('sort', (key, desc) =>
      context.setProperties({ sortBy: key, desc })
    );
    context.set('setPage', (page) => context.set('page', page));
    context.set('setPageSize', (size) =>
      context.setProperties({ pageSize: size, page: 1 })
    );
    context.set('open', (row) => context.set('opened', row.id));
    context.set('setStatus', (selection) => {
      context.set('statusSelection', selection);
      context.set(
        'rows',
        selection.length
          ? ROWS.filter((r) => selection.includes(r.status))
          : ROWS
      );
    });
    context.set('stop', (selected, clear) => {
      context.set(
        'stopped',
        selected.map((r) => r.id)
      );
      clear();
    });
    context.set('viewState', { status: [] });
    context.set('applyView', (state) => context.set('appliedView', state));
  }

  test('pagination, sorting and row clicks', async function (assert) {
    setup(this);
    await render(TEMPLATE);

    assert.dom('[data-test-row]').exists({ count: 10 });
    assert.dom('[data-test-pagination-info]').hasText('1–10 of 30');
    await click('[data-test-pagination-next]');
    assert.equal(this.page, 2);
    assert.dom('[data-test-row]').exists({ count: 10 });
    assert.dom('[data-test-row]').hasAttribute('data-test-row', 'job-10');

    await select('[data-test-page-size-select]', '25');
    assert.equal(this.pageSize, 25);
    assert.dom('[data-test-row]').exists({ count: 25 });

    await click('[data-test-sort-by="status"]');
    assert.equal(this.sortBy, 'status');
    assert.true(this.desc, 'a new sort column starts descending');
    assert
      .dom('[data-test-ui-table-header="status"]')
      .hasAttribute('aria-sort', 'descending');

    await click('[data-test-row="job-01"] [data-test-cell-status]');
    assert.equal(this.opened, 'job-01', 'a click on the row opens it');

    await triggerKeyEvent('[data-test-row="job-02"]', 'keydown', 'Enter');
    assert.equal(this.opened, 'job-02', 'Enter on the row opens it');

    await componentA11yAudit(this.element, assert, STRICT);
  });

  test('filters, columns and saved views', async function (assert) {
    setup(this);
    await render(TEMPLATE);

    assert
      .dom('[data-test-cell-type]')
      .doesNotExist('type is hidden by default');

    await click('[data-test-column-picker] [data-test-dropdown-trigger]');
    await click('[data-test-column-option="type"]');
    assert.dom('[data-test-cell-type]').exists('the column is shown');
    await click('[data-test-column-option="status"]');
    assert.dom('[data-test-cell-status]').doesNotExist('the column is hidden');
    assert.ok(
      window.localStorage.getItem('norcaList.test-list').includes('status'),
      'the column choice is remembered'
    );

    await click('[data-test-status-filter] [data-test-dropdown-trigger]');
    await click('[data-test-dropdown-option="dead"]');
    assert.deepEqual(this.statusSelection, ['dead']);
    assert.dom('[data-test-dropdown-count]').hasText('1');
    assert.dom('[data-test-pagination-info]').hasText('1–10 of 10');

    this.set('viewState', { status: ['dead'] });
    await click('[data-test-saved-views] [data-test-dropdown-trigger]');
    await click('[data-test-save-view]');
    await fillIn('[data-test-save-view-name]', 'Dead jobs');
    await click('[data-test-save-view-submit]');

    await click('[data-test-saved-views] [data-test-dropdown-trigger]');
    assert.dom('[data-test-saved-view="Dead jobs"]').exists();
    await click('[data-test-saved-view="Dead jobs"]');
    assert.deepEqual(this.appliedView, { status: ['dead'] });

    await componentA11yAudit(this.element, assert, STRICT);
  });

  test('row selection and bulk actions', async function (assert) {
    setup(this);
    await render(TEMPLATE);

    assert.dom('[data-test-bulk-bar]').doesNotExist();
    await click('[data-test-row="job-00"] [data-test-select-row]');
    await click('[data-test-row="job-01"] [data-test-select-row]');
    assert.dom('[data-test-bulk-count]').hasText('2 selected');
    await click('[data-test-bulk-stop]');
    assert.deepEqual(this.stopped, ['job-00', 'job-01']);
    assert.dom('[data-test-bulk-bar]').doesNotExist('selection cleared');

    await click('[data-test-select-all]');
    assert.dom('[data-test-bulk-count]').hasText('10 selected');
    assert.equal(
      findAll('[data-test-select-row]').filter((c) => c.checked).length,
      10
    );
  });

  test('Ui::ConfirmButton asks before it acts', async function (assert) {
    this.set('confirmed', 0);
    this.set('confirm', () => this.set('confirmed', this.confirmed + 1));
    await render(hbs`
      <Ui::ConfirmButton @label="Stop job" @message="Stop it?" @confirmLabel="Yes, stop" @onConfirm={{this.confirm}} data-test-confirm />
      <Ui::ConfirmButton @label="Purge" @message="Purge it?" @typedConfirmation="example" @onConfirm={{this.confirm}} data-test-typed />
    `);

    await click('[data-test-confirm] [data-test-idle-button]');
    assert
      .dom('[data-test-confirm] [data-test-confirmation-message]')
      .hasText('Stop it?');
    await click('[data-test-confirm] [data-test-cancel-button]');
    assert.equal(this.confirmed, 0, 'cancel does not confirm');
    await click('[data-test-confirm] [data-test-idle-button]');
    await click('[data-test-confirm] [data-test-confirm-button]');
    assert.equal(this.confirmed, 1);

    await click('[data-test-typed] [data-test-idle-button]');
    assert
      .dom('[data-test-typed-confirmation] [data-test-confirm-button]')
      .isDisabled();
    await fillIn('[data-test-typed-confirmation-input]', 'example');
    await click('[data-test-typed-confirmation] [data-test-confirm-button]');
    assert.equal(this.confirmed, 2);
  });
});
