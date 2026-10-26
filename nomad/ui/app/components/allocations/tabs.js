/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Component from '@glimmer/component';

/**
 * The tabs of an allocation (Overview, Files) or, with @task, of a task
 * (Overview, Logs, Files).
 */
export default class AllocationsTabs extends Component {
  get tabs() {
    const { allocation, task } = this.args;
    if (task) {
      const models = [allocation, task];
      return [
        {
          id: 'overview',
          label: 'Overview',
          route: 'allocations.allocation.task.index',
          models,
        },
        {
          id: 'logs',
          label: 'Logs',
          route: 'allocations.allocation.task.logs',
          models,
        },
        {
          id: 'files',
          label: 'Files',
          route: 'allocations.allocation.task.fs-root',
          models,
          currentWhen:
            'allocations.allocation.task.fs-root allocations.allocation.task.fs',
        },
      ];
    }
    return [
      {
        id: 'overview',
        label: 'Overview',
        route: 'allocations.allocation.index',
        models: [allocation],
      },
      {
        id: 'files',
        label: 'Files',
        route: 'allocations.allocation.fs-root',
        models: [allocation],
        currentWhen: 'allocations.allocation.fs-root allocations.allocation.fs',
      },
    ];
  }
}
