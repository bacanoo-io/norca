/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Component from '@glimmer/component';
import { tracked } from '@glimmer/tracking';
import { action } from '@ember/object';
import { inject as service } from '@ember/service';
import { guidFor } from '@ember/object/internals';

/**
 * The unified list (UI plan, common UX patterns): a table in a card with a
 * toolbar for filters, column selection, saved views, row selection with
 * bulk actions, sorting and pagination.
 *
 *   <Ui::DataTable
 *     @id="jobs"
 *     @caption="Jobs"
 *     @columns={{this.columns}}
 *     @rows={{this.filteredJobs}}
 *     @sortBy={{this.sortProperty}} @sortDescending={{this.sortDescending}} @onSort={{this.sort}}
 *     @pageSize={{this.pageSize}} @page={{this.currentPage}} @onPageChange={{this.setPage}}
 *     @onPageSizeChange={{this.setPageSize}}
 *     @onRowClick={{this.gotoJob}} @enumerate={{true}}
 *     @selectable={{true}}
 *     @viewState={{this.viewState}} @onApplyView={{this.applyView}}
 *   >
 *     <:filters>
 *       <Ui::Input @icon="search" @value={{this.searchTerm}} @onInput={{this.search}} />
 *       <Ui::FilterDropdown @label="Status" ... />
 *     </:filters>
 *     <:row as |job r|>
 *       <r.Row data-test-job-row={{job.plainId}}>
 *         <r.Cell @key="name"><LinkTo ...>{{job.name}}</LinkTo></r.Cell>
 *         <r.Cell @key="status"><Ui::StatusIndicator @status={{job.status}} /></r.Cell>
 *       </r.Row>
 *     </:row>
 *     <:bulk as |selected clear|>
 *       <Ui::Button @variant="danger" {{on "click" (fn this.stopAll selected clear)}}>Stop</Ui::Button>
 *     </:bulk>
 *     <:empty><Ui::EmptyState @title="No jobs" @message="..." /></:empty>
 *   </Ui::DataTable>
 *
 * Columns: { key, label, sortable, hideable (default true), hidden (hidden
 * by default), class, srOnly (label only for screen readers) }. Cells are
 * rendered through `r.Cell @key=...`, which leaves out the cells of hidden
 * columns. The column choice and the saved views are remembered per @id
 * (services/list-preferences). Sorting, filtering and the page state are
 * the caller's (query parameters), the table only paginates @rows.
 *
 * Saved views store @viewState (an object of the caller's filters and sort)
 * together with the column choice; choosing one calls @onApplyView(state).
 */
export default class UiDataTable extends Component {
  @service listPreferences;

  @tracked selectedKeys = [];

  guid = guidFor(this);

  get listId() {
    return this.args.id || this.guid;
  }

  get columnPrefs() {
    return this.args.id ? this.listPreferences.columnsFor(this.args.id) : null;
  }

  isColumnVisible(column, prefs = this.columnPrefs) {
    if (column.hideable === false) return true;
    if (prefs?.hidden?.includes(column.key)) return false;
    if (column.hidden) return !!prefs?.shown?.includes(column.key);
    return true;
  }

  get columns() {
    const prefs = this.columnPrefs;
    return (this.args.columns || []).map((column) => {
      const isSorted = !!column.sortable && this.args.sortBy === column.key;
      let ariaSort = null;
      if (isSorted) {
        ariaSort = this.args.sortDescending ? 'descending' : 'ascending';
      }
      return {
        ...column,
        isSorted,
        ariaSort,
        isVisible: this.isColumnVisible(column, prefs),
        isHideable: column.hideable !== false,
      };
    });
  }

  get visibleColumns() {
    return this.columns.filter((column) => column.isVisible);
  }

  get visibleKeys() {
    return this.visibleColumns.map((c) => c.key);
  }

  get hiddenKeys() {
    return this.columns.filter((c) => !c.isVisible).map((c) => c.key);
  }

  // { key: true } for the visible columns, for callers that render cells
  // spanning columns (sub-rows).
  get shownMap() {
    return Object.fromEntries(this.visibleColumns.map((c) => [c.key, true]));
  }

  get hideableColumns() {
    return this.columns.filter((column) => column.isHideable);
  }

  get showColumnPicker() {
    return this.args.columnPicker !== false && this.hideableColumns.length > 1;
  }

  get showSavedViews() {
    return !!(this.args.id && this.args.onApplyView);
  }

  get colspan() {
    return this.visibleColumns.length + (this.args.selectable ? 1 : 0);
  }

  // Rows and pagination

  get allRows() {
    const rows = this.args.rows;
    if (!rows) return [];
    return typeof rows.toArray === 'function' ? rows.toArray() : [...rows];
  }

  get isPaginated() {
    return !!this.args.pageSize;
  }

  get pageCount() {
    if (!this.isPaginated) return 1;
    return Math.max(1, Math.ceil(this.allRows.length / this.args.pageSize));
  }

  get currentPage() {
    const page = Number(this.args.page) || 1;
    return Math.min(Math.max(page, 1), this.pageCount);
  }

  get pageRows() {
    if (!this.isPaginated) return this.allRows;
    const start = (this.currentPage - 1) * this.args.pageSize;
    return this.allRows.slice(start, start + this.args.pageSize);
  }

  // Selection

  keyFor = (row) => {
    const key = this.args.rowKey || 'id';
    if (typeof key === 'function') return key(row);
    return row?.[key] ?? (typeof row?.get === 'function' ? row.get(key) : row);
  };

  get selectedRows() {
    const keys = new Set(this.selectedKeys);
    return this.allRows.filter((row) => keys.has(this.keyFor(row)));
  }

  get selectedCount() {
    return this.selectedRows.length;
  }

  get pageKeys() {
    return this.pageRows.map(this.keyFor);
  }

  get allPageSelected() {
    const selected = new Set(this.selectedKeys);
    return (
      this.pageKeys.length > 0 && this.pageKeys.every((k) => selected.has(k))
    );
  }

  get somePageSelected() {
    const selected = new Set(this.selectedKeys);
    return !this.allPageSelected && this.pageKeys.some((k) => selected.has(k));
  }

  isSelected = (row) => this.selectedKeys.includes(this.keyFor(row));

  notifySelection() {
    this.args.onSelectionChange?.(this.selectedRows);
  }

  @action
  toggleRow(row, checked) {
    const key = this.keyFor(row);
    const rest = this.selectedKeys.filter((k) => k !== key);
    this.selectedKeys = checked ? [...rest, key] : rest;
    this.notifySelection();
  }

  @action
  togglePage(checked) {
    const pageKeys = new Set(this.pageKeys);
    const rest = this.selectedKeys.filter((k) => !pageKeys.has(k));
    this.selectedKeys = checked ? [...rest, ...this.pageKeys] : rest;
    this.notifySelection();
  }

  @action
  togglePageEvent(event) {
    this.togglePage(event.target.checked);
  }

  @action
  clearSelection() {
    this.selectedKeys = [];
    this.notifySelection();
  }

  // Sorting

  // A new sort column starts descending (as the lists always did); the
  // active column toggles.
  @action
  sort(column) {
    const descending =
      this.args.sortBy === column.key ? !this.args.sortDescending : true;
    this.args.onSort?.(column.key, descending);
  }

  // Columns and views

  @action
  setColumnVisible(column, visible) {
    const prefs = this.columnPrefs || {};
    const hidden = new Set(prefs.hidden || []);
    const shown = new Set(prefs.shown || []);
    hidden.delete(column.key);
    shown.delete(column.key);
    if (!visible) hidden.add(column.key);
    else if (column.hidden) shown.add(column.key);
    this.listPreferences.setColumns(this.listId, {
      hidden: [...hidden],
      shown: [...shown],
    });
  }

  @action
  resetColumns() {
    this.listPreferences.resetColumns(this.listId);
  }

  get viewState() {
    return { ...(this.args.viewState || {}), columns: this.columnPrefs };
  }

  @action
  applyView(state) {
    const { columns, ...rest } = state || {};
    if (columns) this.listPreferences.setColumns(this.listId, columns);
    else this.listPreferences.resetColumns(this.listId);
    this.clearSelection();
    this.args.onApplyView?.(rest);
  }

  @action
  changePage(page) {
    this.args.onPageChange?.(page);
  }
}
