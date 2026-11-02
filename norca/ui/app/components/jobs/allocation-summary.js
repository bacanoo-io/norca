/**
 * SPDX-FileCopyrightText: 2026 Norca contributors
 * SPDX-License-Identifier: MPL-2.0
 */

import Component from '@glimmer/component';
import { action } from '@ember/object';
import { inject as service } from '@ember/service';

const STATES = [
  ['queued', 'Queued', 'queuedAllocs'],
  ['starting', 'Starting', 'startingAllocs'],
  ['running', 'Running', 'runningAllocs'],
  ['complete', 'Complete', 'completeAllocs'],
  [
    'unknown',
    'Unknown',
    'unknownAllocs',
    'Allocation is unknown since its node is disconnected.',
  ],
  ['failed', 'Failed', 'failedAllocs'],
  ['lost', 'Lost', 'lostAllocs'],
];

/**
 * The allocation states of a job or a task group summary as a distribution;
 * the segments and the legend lead to the allocations of the state.
 *
 *   <Jobs::AllocationSummary @summary={{@job.summary}} @job={{@job}} />
 */
export default class JobsAllocationSummary extends Component {
  @service router;

  get data() {
    const summary = this.args.summary;
    if (!summary) return [];
    const job = this.args.job;
    const query = (status) => ({
      status: JSON.stringify([status]),
      namespace: job?.belongsTo?.('namespace').id(),
    });
    return STATES.map(([key, label, property, help]) => ({
      key,
      label,
      help,
      value: summary.get ? summary.get(property) : summary[property],
      ...(job && key !== 'queued'
        ? { route: 'jobs.job.allocations', model: job, query: query(key) }
        : {}),
    }));
  }

  get total() {
    return this.data.reduce((sum, d) => sum + (d.value || 0), 0);
  }

  @action
  select(segment) {
    if (!this.args.job) return;
    this.router.transitionTo('jobs.job.allocations', this.args.job, {
      queryParams: {
        status: JSON.stringify([segment.key]),
        namespace: this.args.job.get('namespace.name'),
      },
    });
  }
}
