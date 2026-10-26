/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Component from '@glimmer/component';
import { tracked } from '@glimmer/tracking';
import { action } from '@ember/object';
import { inject as service } from '@ember/service';
import { sortRows } from 'nomad-ui/utils/sort-rows';
import localStorageProperty from 'nomad-ui/utils/properties/local-storage';

// Sort keys of the columns (paths on the allocation model).
const SORT_PATHS = {
  id: 'shortId',
  taskGroup: 'taskGroupName',
  job: 'job.name',
  created: 'createIndex',
  modified: 'modifyIndex',
  status: 'statusIndex',
  version: 'jobVersion',
  client: 'node.shortId',
};

/**
 * The allocations of a job, a task group or a client, on the unified list.
 *
 *   <Allocations::Table
 *     @id="job-allocations" @context="job" @allocations={{this.filtered}}
 *     @sortBy={{this.sortProperty}} @sortDescending={{this.sortDescending}} @onSort={{this.sort}}
 *     @pageSize={{25}} @page={{this.currentPage}} @onPageChange={{this.setPage}}
 *     @activeTask={{this.activeTask}} @onSetActiveTask={{this.setActiveTask}}
 *   >
 *     <:filters>...</:filters>
 *   </Allocations::Table>
 *
 * @context: job (with the task group column), taskGroup, node or volume (with the
 * job column). The tasks of each allocation are shown under it when "Show
 * tasks" is on (remembered); a task's logs open in a side panel.
 * Sorting is the caller's when @onSort is given, otherwise local; with
 * @presorted the rows are shown in the given order, @sortable={{false}}
 * turns the sortable headers off.
 */
export default class AllocationsTable extends Component {
  @service router;

  @localStorageProperty('nomadShowSubTasks', true) showSubTasks;

  @tracked localSortBy = 'modified';
  @tracked localSortDescending = true;

  get context() {
    return this.args.context || 'job';
  }

  get columns() {
    const context = this.context;
    const columns = [
      { key: 'indicators', label: 'Indicators', srOnly: true, hideable: false },
      { key: 'id', label: 'ID', sortable: true, hideable: false },
    ];
    if (context === 'job') {
      columns.push({ key: 'taskGroup', label: 'Task group', sortable: true });
    }
    // The allocations of a client or a CSI volume belong to many jobs.
    if (context === 'node' || context === 'volume') {
      columns.push({ key: 'job', label: 'Job', sortable: true });
    }
    columns.push(
      { key: 'created', label: 'Created', sortable: true },
      { key: 'modified', label: 'Modified', sortable: true },
      { key: 'status', label: 'Status', sortable: true },
      { key: 'version', label: 'Version', sortable: true }
    );
    if (context !== 'node') {
      columns.push({ key: 'client', label: 'Client', sortable: true });
    }
    if (context !== 'volume') {
      columns.push({ key: 'volume', label: 'Volume' });
    }
    columns.push(
      { key: 'cpu', label: 'CPU' },
      { key: 'memory', label: 'Memory' }
    );
    if (this.args.sortable === false) {
      return columns.map((column) => ({ ...column, sortable: false }));
    }
    return columns;
  }

  // The column keys of the old sort query parameters.
  static fromSortProperty(property) {
    const entry = Object.entries(SORT_PATHS).find(([, p]) => p === property);
    return entry ? entry[0] : property;
  }

  get sortBy() {
    if (this.args.onSort) {
      return AllocationsTable.fromSortProperty(
        this.args.sortBy || 'modifyIndex'
      );
    }
    return this.localSortBy;
  }

  get sortDescending() {
    return this.args.onSort
      ? !!this.args.sortDescending
      : this.localSortDescending;
  }

  get sortedAllocations() {
    if (this.args.presorted) return this.args.allocations || [];
    return sortRows(
      this.args.allocations || [],
      SORT_PATHS[this.sortBy] || 'modifyIndex',
      this.sortDescending
    );
  }

  @action
  sort(key, descending) {
    if (this.args.onSort) {
      this.args.onSort(SORT_PATHS[key] || key, descending);
    } else {
      this.localSortBy = key;
      this.localSortDescending = descending;
    }
  }

  @action
  toggleSubTasks(value) {
    this.showSubTasks = value;
  }

  // The task whose logs are open: @activeTask is "<allocation id>-<task>".
  get activeTaskState() {
    const key = this.args.activeTask;
    if (!key) return null;
    const list = this.args.allocations;
    const allocations =
      typeof list?.toArray === 'function' ? list.toArray() : [...(list || [])];
    const allocation = allocations.find((a) => key.startsWith(`${a.id}-`));
    if (!allocation) return null;
    const name = key.slice(allocation.id.length + 1);
    return (allocation.states || []).find((s) => s.name === name) || null;
  }

  @action
  closeLogs() {
    this.args.onSetActiveTask?.(null);
  }

  @action
  gotoAllocation(allocation) {
    this.router.transitionTo('allocations.allocation', allocation.id);
  }
}
