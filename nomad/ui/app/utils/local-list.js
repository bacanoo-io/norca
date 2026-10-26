/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import { tracked } from '@glimmer/tracking';
import { action } from '@ember/object';
import { sortRows } from 'nomad-ui/utils/sort-rows';

/**
 * The state of a unified list kept in memory (no query parameters): search,
 * multi-choice filters, sorting and paging, with the saved view state.
 *
 *   list = new LocalList({
 *     sortBy: 'name',
 *     searchProps: ['name', 'description'],
 *     filters: { type: (row, selection) => selection.includes(row.type) },
 *   });
 *   list.rows(this.tokens)
 *
 * The template binds the list's state and actions to Ui::DataTable
 * (@sortBy={{this.list.sortBy}} @onSort={{this.list.sort}} ...).
 */
export default class LocalList {
  @tracked sortBy;
  @tracked sortDescending = false;
  @tracked searchTerm = '';
  @tracked page = 1;
  @tracked pageSize = 25;
  @tracked selections = {};

  constructor({
    sortBy = 'name',
    sortDescending = false,
    searchProps = ['name'],
    filters = {},
  } = {}) {
    this.sortBy = sortBy;
    this.sortDescending = sortDescending;
    this.searchProps = searchProps;
    this.filters = filters;
  }

  rows(source) {
    const term = this.searchTerm.trim().toLowerCase();
    const filtered = (source || []).filter((row) => {
      if (
        term &&
        !this.searchProps.some((prop) =>
          String(row[prop] ?? (row.get && row.get(prop)) ?? '')
            .toLowerCase()
            .includes(term)
        )
      ) {
        return false;
      }
      return Object.entries(this.filters).every(([key, test]) => {
        const selection = this.selections[key] || [];
        return !selection.length || test(row, selection);
      });
    });
    return sortRows(filtered, this.sortBy, this.sortDescending);
  }

  selection(key) {
    return this.selections[key] || [];
  }

  get viewState() {
    return {
      sortBy: this.sortBy,
      sortDescending: this.sortDescending,
      searchTerm: this.searchTerm,
      selections: this.selections,
    };
  }

  @action
  sort(sortBy, sortDescending) {
    this.sortBy = sortBy;
    this.sortDescending = sortDescending;
  }

  @action
  search(term) {
    this.searchTerm = term;
    this.page = 1;
  }

  @action
  setFilter(key, selection) {
    this.selections = { ...this.selections, [key]: selection };
    this.page = 1;
  }

  @action
  setPage(page) {
    this.page = page;
  }

  @action
  setPageSize(size) {
    this.pageSize = size;
    this.page = 1;
  }

  @action
  applyView(state) {
    if (!state) return;
    if ('sortBy' in state) this.sortBy = state.sortBy;
    if ('sortDescending' in state) this.sortDescending = state.sortDescending;
    if ('searchTerm' in state) this.searchTerm = state.searchTerm || '';
    if ('selections' in state) this.selections = state.selections || {};
    this.page = 1;
  }
}
