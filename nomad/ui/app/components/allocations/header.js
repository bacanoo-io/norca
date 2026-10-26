/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Component from '@glimmer/component';
import { inject as service } from '@ember/service';

/**
 * The header of the allocation and task pages. The breadcrumbs extend from
 * the job and the task group although the routes do not.
 *
 *   <Allocations::Header @allocation={{this.model}}>actions</Allocations::Header>
 *   <Allocations::Header @allocation={{this.model.allocation}} @task={{this.model}} />
 */
export default class AllocationsHeader extends Component {
  @service store;

  get job() {
    const allocation = this.args.allocation;
    const jobId = allocation.belongsTo('job').id();
    return jobId ? this.store.peekRecord('job', jobId) : null;
  }

  get breadcrumbs() {
    const allocation = this.args.allocation;
    const job = this.job;
    const items = [{ label: 'Jobs', route: 'jobs.index', id: 'jobs.index' }];
    if (job) {
      items.push(
        {
          label: job.trimmedName || job.name,
          route: 'jobs.job.index',
          model: job.idWithNamespace,
          id: 'jobs.job.index',
        },
        {
          label: allocation.taskGroupName,
          route: 'jobs.job.task-group',
          models: [job.idWithNamespace, allocation.taskGroupName],
          id: 'jobs.job.task-group',
        }
      );
    }
    items.push({
      label: allocation.shortId,
      route: 'allocations.allocation',
      model: allocation,
      id: 'allocations.allocation',
    });
    if (this.args.task) {
      items.push({
        label: this.args.task.name,
        route: 'allocations.allocation.task',
        models: [allocation, this.args.task],
        id: 'allocations.allocation.task',
      });
    }
    return items;
  }
}
