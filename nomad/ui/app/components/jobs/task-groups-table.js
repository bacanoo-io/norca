/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Component from '@glimmer/component';
import { tracked } from '@glimmer/tracking';
import { action } from '@ember/object';
import { inject as service } from '@ember/service';
import { sortRows } from 'nomad-ui/utils/sort-rows';

/**
 * The task groups of a job, with their count (manual scaling), allocation
 * states and reserved resources.
 */
export default class JobsTaskGroupsTable extends Component {
  @service router;

  @tracked localSortBy = 'name';
  @tracked localSortDescending = false;

  columns = [
    { key: 'name', label: 'Name', sortable: true, hideable: false },
    { key: 'count', label: 'Count', sortable: true },
    { key: 'allocations', label: 'Allocation status', sortable: true },
    { key: 'volume', label: 'Volume', sortable: true },
    { key: 'cpu', label: 'Reserved CPU', sortable: true },
    { key: 'memory', label: 'Reserved memory', sortable: true },
    { key: 'disk', label: 'Reserved disk', sortable: true },
  ];

  static sortKeys = {
    name: 'name',
    count: 'count',
    allocations: 'queuedOrStartingAllocs',
    volume: 'volumes.length',
    cpu: 'reservedCPU',
    memory: 'reservedMemory',
    disk: 'reservedEphemeralDisk',
  };

  // The caller's sort (query parameters) when given, a local one otherwise.
  get sortBy() {
    return this.args.sortBy ?? this.localSortBy;
  }

  get sortDescending() {
    return this.args.sortDescending ?? this.localSortDescending;
  }

  get sortedTaskGroups() {
    const taskGroups = this.args.job.taskGroups || [];
    return sortRows(
      taskGroups,
      JobsTaskGroupsTable.sortKeys[this.sortBy] || 'name',
      this.sortDescending
    );
  }

  @action
  sort(key, descending) {
    if (this.args.onSort) {
      this.args.onSort(key, descending);
    } else {
      this.localSortBy = key;
      this.localSortDescending = descending;
    }
  }

  @action
  gotoTaskGroup(taskGroup) {
    this.router.transitionTo(
      'jobs.job.task-group',
      this.args.job,
      taskGroup.name
    );
  }
}
