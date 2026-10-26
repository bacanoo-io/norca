/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Component from '@glimmer/component';
import { action } from '@ember/object';
import { inject as service } from '@ember/service';
import { sortRows } from 'nomad-ui/utils/sort-rows';

/**
 * The child jobs (launches) of a periodic or parameterized job. Sorting
 * and the page are the caller's query parameters (@sortBy,
 * @sortDescending, @page and the matching setters).
 */
export default class JobsChildren extends Component {
  @service router;
  @service userSettings;

  columns = [
    { key: 'name', label: 'Name', sortable: true, hideable: false },
    { key: 'submitTime', label: 'Submitted', sortable: true },
    { key: 'status', label: 'Status', sortable: true },
    { key: 'groups', label: 'Groups' },
    { key: 'summary', label: 'Allocations' },
  ];

  get sortBy() {
    return this.columns.some((c) => c.key === this.args.sortBy)
      ? this.args.sortBy
      : 'submitTime';
  }

  get sortDescending() {
    return this.args.sortBy ? !!this.args.sortDescending : true;
  }

  get page() {
    return this.args.page || 1;
  }

  get summaryData() {
    const summary = this.args.job.summary;
    const get = (p) => (summary?.get ? summary.get(p) : 0) || 0;
    return [
      { key: 'pending', label: 'Pending', value: get('pendingChildren') },
      { key: 'running', label: 'Running', value: get('runningChildren') },
      { key: 'dead', label: 'Dead', value: get('deadChildren') },
    ];
  }

  get sortedChildren() {
    const children = this.args.job.children || [];
    return sortRows(
      children,
      this.sortBy === 'name' ? 'name' : this.sortBy,
      this.sortDescending
    );
  }

  @action
  sort(key, descending) {
    this.args.onSort?.(key, descending);
  }

  @action
  setPage(page) {
    this.args.onPageChange?.(page);
  }

  @action
  setPageSize(size) {
    this.userSettings.pageSize = size;
    this.args.onPageChange?.(1);
  }

  @action
  gotoJob(child) {
    this.router.transitionTo('jobs.job.index', child.idWithNamespace);
  }
}
